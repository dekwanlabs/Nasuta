package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// CancelRun atomically closes active node Attempts before the Run.
func (workflowStore *Store) CancelRun(
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
	nodes, err := lockActiveWorkflowNodes(ctx, tx, workflowRunID)
	if err != nil {
		return CancelTransition{}, err
	}
	events := make([]Event, 0, len(nodes)+1)
	for _, node := range nodes {
		event, err := cancelWorkflowNode(ctx, tx, workflowRunID, node, endedAt)
		if err != nil {
			return CancelTransition{}, err
		}
		events = append(events, event)
	}
	runEvent, err := cancelWorkflowRun(ctx, tx, workflowRunID, endedAt)
	if err != nil {
		return CancelTransition{}, err
	}
	events = append(events, runEvent)
	if err := appendEventsTx(ctx, tx, workflowRunID, events); err != nil {
		return CancelTransition{}, err
	}
	if err := tx.Commit(); err != nil {
		return CancelTransition{}, fmt.Errorf("commit workflow %q cancellation: %w", workflowRunID, err)
	}
	workflowStore.publish(events)
	return CancelTransition{Applied: true, Status: RunCancelled}, nil
}

type activeWorkflowNode struct {
	id      string
	attempt int
}

func lockActiveWorkflowNodes(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
) ([]activeWorkflowNode, error) {
	rows, err := tx.QueryContext(ctx, `SELECT node_id,attempt
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND status IN (?,?)
		ORDER BY node_id,attempt FOR UPDATE`,
		workflowRunID, RunRunning, RunWaitingHuman,
	)
	if err != nil {
		return nil, fmt.Errorf("lock workflow %q active nodes: %w", workflowRunID, err)
	}
	defer rows.Close()
	nodes := make([]activeWorkflowNode, 0)
	for rows.Next() {
		var node activeWorkflowNode
		if err := rows.Scan(&node.id, &node.attempt); err != nil {
			return nil, fmt.Errorf("scan workflow %q active node: %w", workflowRunID, err)
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow %q active nodes: %w", workflowRunID, err)
	}
	return nodes, nil
}

func cancelWorkflowNode(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	node activeWorkflowNode,
	endedAt time.Time,
) (Event, error) {
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
		return Event{}, fmt.Errorf(
			"cancel workflow node %q/%q attempt %d: %w",
			workflowRunID, node.id, node.attempt, err,
		)
	}
	if err := requireSingleTransition(result, workflowRunID+"/"+node.id); err != nil {
		return Event{}, err
	}
	return Event{
		WorkflowRunID: workflowRunID,
		Kind:          "node_cancelled",
		NodeID:        node.id,
		Summary:       "workflow node cancelled",
		CreatedAt:     endedAt,
	}, nil
}

func cancelWorkflowRun(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	endedAt time.Time,
) (Event, error) {
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
		return Event{}, fmt.Errorf("cancel workflow run %q: %w", workflowRunID, err)
	}
	if err := requireSingleTransition(result, workflowRunID); err != nil {
		return Event{}, err
	}
	return Event{
		WorkflowRunID: workflowRunID,
		Kind:          "workflow_cancelled",
		Summary:       "workflow cancelled",
		CreatedAt:     endedAt,
	}, nil
}

// DecideApproval atomically records the immutable decision and its state transition.
func (workflowStore *Store) DecideApproval(
	ctx context.Context,
	approval Approval,
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
	transition, handled, err := decideApprovalIdempotent(ctx, tx, approval, runStatus)
	if err != nil {
		return ApprovalTransition{}, err
	}
	if handled {
		return transition, nil
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
		events, err = approveNodeTx(ctx, tx, approval, approvedHandoff, attempt)
		if err != nil {
			return ApprovalTransition{}, err
		}
		runStatus = RunRunning
	case ApprovalRejected:
		events, err = rejectNodeTx(ctx, tx, approval, attempt)
		if err != nil {
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

// decideApprovalIdempotent returns the already-recorded decision when one
// exists. The second return reports whether the request was fully handled.
func decideApprovalIdempotent(
	ctx context.Context,
	tx *sql.Tx,
	approval Approval,
	runStatus RunStatus,
) (ApprovalTransition, bool, error) {
	existing, err := getApprovalTx(ctx, tx, approval.WorkflowRunID, approval.NodeID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return ApprovalTransition{}, false, err
		}
		return ApprovalTransition{}, false, nil
	}
	if existing.Decision != approval.Decision {
		return ApprovalTransition{}, false, fmt.Errorf(
			"%w for workflow node %q/%q",
			ErrApprovalConflict, approval.WorkflowRunID, approval.NodeID,
		)
	}
	if err := tx.Commit(); err != nil {
		return ApprovalTransition{}, false, fmt.Errorf(
			"commit idempotent workflow approval %q/%q: %w",
			approval.WorkflowRunID, approval.NodeID, err,
		)
	}
	return ApprovalTransition{
		Approval: *existing, Applied: false, RunStatus: runStatus,
	}, true, nil
}

// approveNodeTx persists an approved decision and returns its events.
func approveNodeTx(
	ctx context.Context,
	tx *sql.Tx,
	approval Approval,
	approvedHandoff *Handoff,
	attempt int,
) ([]Event, error) {
	if approvedHandoff == nil {
		return nil, fmt.Errorf(
			"approved workflow node %q/%q requires a handoff: %w",
			approval.WorkflowRunID, approval.NodeID, ErrInvalid,
		)
	}
	if err := saveHandoffTx(ctx, tx, *approvedHandoff); err != nil {
		return nil, err
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
		return nil, fmt.Errorf(
			"approve workflow node %q/%q: %w",
			approval.WorkflowRunID, approval.NodeID, err,
		)
	}
	if err := requireSingleTransition(result, approval.WorkflowRunID+"/"+approval.NodeID); err != nil {
		return nil, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE workflow_runs
		SET status=?,error_code='',ended_at=NULL WHERE id=? AND status=?`,
		RunRunning, approval.WorkflowRunID, RunWaitingHuman,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"resume workflow run %q: %w", approval.WorkflowRunID, err,
		)
	}
	if err := requireSingleTransition(result, approval.WorkflowRunID); err != nil {
		return nil, err
	}
	events := []Event{
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
		return nil, err
	}
	return events, nil
}

// rejectNodeTx persists a rejected decision and returns its events.
func rejectNodeTx(
	ctx context.Context,
	tx *sql.Tx,
	approval Approval,
	attempt int,
) ([]Event, error) {
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
		return nil, fmt.Errorf(
			"reject workflow node %q/%q: %w",
			approval.WorkflowRunID, approval.NodeID, err,
		)
	}
	if err := requireSingleTransition(result, approval.WorkflowRunID+"/"+approval.NodeID); err != nil {
		return nil, err
	}
	result, err = tx.ExecContext(ctx, `UPDATE workflow_runs
		SET status=?,error_code=?,ended_at=? WHERE id=? AND status=?`,
		RunFailed, "human_approval_rejected",
		store.DatabaseTime(approval.DecidedAt.UTC().Format(time.RFC3339Nano)),
		approval.WorkflowRunID, RunWaitingHuman,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"reject workflow run %q: %w", approval.WorkflowRunID, err,
		)
	}
	if err := requireSingleTransition(result, approval.WorkflowRunID); err != nil {
		return nil, err
	}
	events := []Event{
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
		return nil, err
	}
	return events, nil
}

func getApprovalTx(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	nodeID string,
) (*Approval, error) {
	var approval Approval
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
