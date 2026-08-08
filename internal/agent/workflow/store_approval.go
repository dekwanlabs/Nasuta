package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// CancelWorkflow atomically closes active node Attempts before the Run.
func (workflowStore *Store) CancelWorkflow(
	ctx context.Context,
	workflowRunID string,
	endedAt time.Time,
) (CancelTransition, error) {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return CancelTransition{}, fmt.Errorf("begin workflow %q cancellation: %w", workflowRunID, err)
	}
	defer tx.Rollback()
	status, err := lockWorkflow(ctx, tx, workflowRunID)
	if err != nil {
		return CancelTransition{}, err
	}
	if status != RunRunning && status != RunWaitingHuman {
		if err := tx.Commit(); err != nil {
			return CancelTransition{}, fmt.Errorf(
				"commit idempotent workflow %q cancellation: %w",
				workflowRunID, err,
			)
		}
		return CancelTransition{Status: status}, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT node_id,attempt
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND status IN (?,?)
		ORDER BY node_id,attempt FOR UPDATE`,
		workflowRunID, RunRunning, RunWaitingHuman,
	)
	if err != nil {
		return CancelTransition{}, fmt.Errorf("lock workflow %q active nodes: %w", workflowRunID, err)
	}
	type activeNode struct {
		id      string
		attempt int
	}
	nodes := make([]activeNode, 0)
	for rows.Next() {
		var node activeNode
		if err := rows.Scan(&node.id, &node.attempt); err != nil {
			rows.Close()
			return CancelTransition{}, fmt.Errorf("scan workflow %q active node: %w", workflowRunID, err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Close(); err != nil {
		return CancelTransition{}, fmt.Errorf("close workflow %q active nodes: %w", workflowRunID, err)
	}
	if err := rows.Err(); err != nil {
		return CancelTransition{}, fmt.Errorf("iterate workflow %q active nodes: %w", workflowRunID, err)
	}
	events := make([]Event, 0, len(nodes)+1)
	for _, node := range nodes {
		result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
			SET status=?,error_code=?,ended_at=?
			WHERE workflow_run_id=? AND node_id=? AND attempt=?
			AND status IN (?,?)`,
			RunCancelled,
			"workflow_cancelled",
			store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
			workflowRunID,
			node.id,
			node.attempt,
			RunRunning,
			RunWaitingHuman,
		)
		if err != nil {
			return CancelTransition{}, fmt.Errorf(
				"cancel workflow node %q/%q attempt %d: %w",
				workflowRunID, node.id, node.attempt, err,
			)
		}
		if err := requireSingleTransition(result, workflowRunID+"/"+node.id); err != nil {
			return CancelTransition{}, err
		}
		events = append(events, Event{
			WorkflowRunID: workflowRunID,
			Kind:          "node_cancelled",
			NodeID:        node.id,
			Summary:       "workflow node cancelled",
			CreatedAt:     endedAt,
		})
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs
		SET status=?,error_code=?,ended_at=?
		WHERE id=? AND status IN (?,?)`,
		RunCancelled,
		"workflow_cancelled",
		store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
		workflowRunID,
		RunRunning,
		RunWaitingHuman,
	)
	if err != nil {
		return CancelTransition{}, fmt.Errorf("cancel workflow run %q: %w", workflowRunID, err)
	}
	if err := requireSingleTransition(result, workflowRunID); err != nil {
		return CancelTransition{}, err
	}
	events = append(events, Event{
		WorkflowRunID: workflowRunID,
		Kind:          "workflow_cancelled",
		Summary:       "workflow cancelled",
		CreatedAt:     endedAt,
	})
	if err := appendEventsTx(ctx, tx, workflowRunID, events); err != nil {
		return CancelTransition{}, err
	}
	if err := tx.Commit(); err != nil {
		return CancelTransition{}, fmt.Errorf("commit workflow %q cancellation: %w", workflowRunID, err)
	}
	workflowStore.publish(events)
	return CancelTransition{Applied: true, Status: RunCancelled}, nil
}

// DecideHumanApproval atomically records the immutable decision and its state transition.
func (workflowStore *Store) DecideHumanApproval(
	ctx context.Context,
	approval WorkflowApproval,
	approvedHandoff *Handoff,
) (ApprovalTransition, error) {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return ApprovalTransition{}, fmt.Errorf(
			"begin workflow approval %q/%q: %w",
			approval.WorkflowRunID, approval.NodeID, err,
		)
	}
	defer tx.Rollback()
	runStatus, err := lockWorkflow(ctx, tx, approval.WorkflowRunID)
	if err != nil {
		return ApprovalTransition{}, err
	}
	existing, err := getApprovalTx(ctx, tx, approval.WorkflowRunID, approval.NodeID)
	if err == nil {
		if existing.Decision != approval.Decision {
			return ApprovalTransition{}, fmt.Errorf(
				"%w for workflow node %q/%q",
				ErrApprovalConflict, approval.WorkflowRunID, approval.NodeID,
			)
		}
		if err := tx.Commit(); err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"commit idempotent workflow approval %q/%q: %w",
				approval.WorkflowRunID, approval.NodeID, err,
			)
		}
		return ApprovalTransition{
			Approval: *existing, Applied: false, RunStatus: runStatus,
		}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return ApprovalTransition{}, err
	}
	if runStatus != RunWaitingHuman {
		return ApprovalTransition{}, fmt.Errorf(
			"workflow run %q is %q, expected %q: %w",
			approval.WorkflowRunID, runStatus, RunWaitingHuman, ErrConflict,
		)
	}
	attempt, kind, nodeStatus, err := lockLatestNodeRun(
		ctx, tx, approval.WorkflowRunID, approval.NodeID,
	)
	if err != nil {
		return ApprovalTransition{}, err
	}
	if kind != NodeHumanApproval {
		return ApprovalTransition{}, fmt.Errorf(
			"workflow node %q/%q kind %q does not require human approval: %w",
			approval.WorkflowRunID, approval.NodeID, kind, ErrConflict,
		)
	}
	if nodeStatus != RunWaitingHuman {
		return ApprovalTransition{}, fmt.Errorf(
			"workflow node %q/%q is %q, expected %q: %w",
			approval.WorkflowRunID, approval.NodeID, nodeStatus, RunWaitingHuman, ErrConflict,
		)
	}
	var events []Event
	switch approval.Decision {
	case ApprovalApproved:
		if approvedHandoff == nil {
			return ApprovalTransition{}, fmt.Errorf(
				"approved workflow node %q/%q requires a handoff: %w",
				approval.WorkflowRunID, approval.NodeID, ErrInvalid,
			)
		}
		if err := saveHandoffTx(ctx, tx, *approvedHandoff); err != nil {
			return ApprovalTransition{}, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
			SET output_handoff_id=?,approval_decision=?,approver_user_id=?,
				approver_tenant_id=?,approval_comment=?,approval_decided_at=?,
				status=?,error_code='',ended_at=?
			WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`,
			approvedHandoff.ID, approval.Decision, approval.ApproverUserID,
			approval.ApproverTenantID, approval.Comment,
			store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
			RunSucceeded,
			store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
			approval.WorkflowRunID, approval.NodeID, attempt, RunWaitingHuman,
		)
		if err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"approve workflow node %q/%q: %w",
				approval.WorkflowRunID, approval.NodeID, err,
			)
		}
		if err := requireSingleTransition(result, approval.WorkflowRunID+"/"+approval.NodeID); err != nil {
			return ApprovalTransition{}, err
		}
		result, err = tx.ExecContext(ctx, `UPDATE workflow_runs
			SET status=?,error_code='',ended_at=NULL WHERE id=? AND status=?`,
			RunRunning, approval.WorkflowRunID, RunWaitingHuman,
		)
		if err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"resume workflow run %q: %w", approval.WorkflowRunID, err,
			)
		}
		if err := requireSingleTransition(result, approval.WorkflowRunID); err != nil {
			return ApprovalTransition{}, err
		}
		events = []Event{
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "human_approved",
				NodeID: approval.NodeID, Summary: "workflow node approved",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "handoff_created",
				NodeID: approval.NodeID, Summary: "approved handoff created",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "node_succeeded",
				NodeID: approval.NodeID, Summary: "workflow node succeeded",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "workflow_resumed",
				Summary: "workflow resumed", CreatedAt: approval.DecidedAt,
			},
		}
		if err := appendEventsTx(ctx, tx, approval.WorkflowRunID, events); err != nil {
			return ApprovalTransition{}, err
		}
		runStatus = RunRunning
	case ApprovalRejected:
		result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
			SET approval_decision=?,approver_user_id=?,approver_tenant_id=?,
				approval_comment=?,approval_decided_at=?,
				status=?,error_code=?,ended_at=?
			WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`,
			approval.Decision, approval.ApproverUserID, approval.ApproverTenantID,
			approval.Comment,
			store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
			RunFailed, "human_approval_rejected",
			store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
			approval.WorkflowRunID, approval.NodeID, attempt, RunWaitingHuman,
		)
		if err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"reject workflow node %q/%q: %w",
				approval.WorkflowRunID, approval.NodeID, err,
			)
		}
		if err := requireSingleTransition(result, approval.WorkflowRunID+"/"+approval.NodeID); err != nil {
			return ApprovalTransition{}, err
		}
		result, err = tx.ExecContext(ctx, `UPDATE workflow_runs
			SET status=?,error_code=?,ended_at=? WHERE id=? AND status=?`,
			RunFailed, "human_approval_rejected",
			store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
			approval.WorkflowRunID, RunWaitingHuman,
		)
		if err != nil {
			return ApprovalTransition{}, fmt.Errorf(
				"reject workflow run %q: %w", approval.WorkflowRunID, err,
			)
		}
		if err := requireSingleTransition(result, approval.WorkflowRunID); err != nil {
			return ApprovalTransition{}, err
		}
		events = []Event{
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "human_rejected",
				NodeID: approval.NodeID, Summary: "workflow node rejected",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "node_failed",
				NodeID: approval.NodeID, Summary: "workflow node failed",
				CreatedAt: approval.DecidedAt,
			},
			{
				WorkflowRunID: approval.WorkflowRunID, Kind: "workflow_failed",
				Summary: "workflow failed", CreatedAt: approval.DecidedAt,
			},
		}
		if err := appendEventsTx(ctx, tx, approval.WorkflowRunID, events); err != nil {
			return ApprovalTransition{}, err
		}
		runStatus = RunFailed
	default:
		return ApprovalTransition{}, fmt.Errorf(
			"workflow approval decision %q is invalid: %w",
			approval.Decision, ErrInvalid,
		)
	}
	if err := tx.Commit(); err != nil {
		return ApprovalTransition{}, fmt.Errorf(
			"commit workflow approval %q/%q: %w",
			approval.WorkflowRunID, approval.NodeID, err,
		)
	}
	workflowStore.publish(events)
	return ApprovalTransition{
		Approval: approval, Applied: true, RunStatus: runStatus,
	}, nil
}

func getApprovalTx(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	nodeID string,
) (*WorkflowApproval, error) {
	var approval WorkflowApproval
	if err := tx.QueryRowContext(ctx, `SELECT
		workflow_run_id,node_id,approval_decision,approver_user_id,approver_tenant_id,
		approval_comment,approval_decided_at
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=? AND approval_decision IS NOT NULL
		ORDER BY attempt DESC LIMIT 1`,
		workflowRunID, nodeID,
	).Scan(
		&approval.WorkflowRunID, &approval.NodeID, &approval.Decision,
		&approval.ApproverUserID, &approval.ApproverTenantID, &approval.Comment,
		&approval.DecidedAt,
	); err != nil {
		return nil, fmt.Errorf(
			"get workflow approval %q/%q: %w",
			workflowRunID, nodeID, err,
		)
	}
	return &approval, nil
}

func lockLatestNodeRun(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	nodeID string,
) (int, NodeKind, RunStatus, error) {
	var attempt int
	var kind NodeKind
	var status RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT attempt,kind,status
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=?
		ORDER BY attempt DESC LIMIT 1 FOR UPDATE`,
		workflowRunID, nodeID,
	).Scan(&attempt, &kind, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", "", fmt.Errorf(
				"lock workflow node %q/%q: %w",
				workflowRunID, nodeID, ErrNotFound,
			)
		}
		return 0, "", "", fmt.Errorf(
			"lock workflow node %q/%q: %w",
			workflowRunID, nodeID, err,
		)
	}
	return attempt, kind, status, nil
}
