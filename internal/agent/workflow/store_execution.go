package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// StartWorkflow atomically fixes the run, its input artifact, and initial events.
func (workflowStore *Store) StartWorkflow(
	ctx context.Context,
	run WorkflowRunRecord,
	input Handoff,
) error {
	budget, err := json.Marshal(run.Budget)
	if err != nil {
		return fmt.Errorf("marshal workflow budget: %w", err)
	}
	selection, err := json.Marshal(run.Selection)
	if err != nil {
		return fmt.Errorf("marshal workflow definition selection: %w", err)
	}
	actorPermissions, err := json.Marshal(run.ActorPermissions)
	if err != nil {
		return fmt.Errorf("marshal workflow actor permissions: %w", err)
	}
	scenarioPermissions, err := json.Marshal(run.ScenarioPermissions)
	if err != nil {
		return fmt.Errorf("marshal workflow scenario permissions: %w", err)
	}
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow %q: %w", run.ID, err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_runs(
		id,parent_run_id,workflow_id,workflow_version,workflow_hash,selection_json,input_hash,actor_user_id,
		actor_tenant_id,actor_permissions_json,scenario,scenario_permissions_json,
		status,budget_json,input_tokens,output_tokens,reasoning_tokens,total_tokens,
		tool_call_count,cost_micros,retry_count,error_code,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		run.ID, run.ParentRunID, run.WorkflowID, run.WorkflowVersion, run.WorkflowHash, selection, run.InputHash,
		run.ActorUserID, run.ActorTenantID, actorPermissions, run.Scenario,
		scenarioPermissions, RunRunning, budget,
		run.Usage.InputTokens, run.Usage.OutputTokens, run.Usage.ReasoningTokens,
		run.Usage.TotalTokens, run.Usage.ToolCalls, run.Usage.CostMicros,
		run.Usage.Retries, "",
		store.DatabaseTime(run.StartedAt.UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return fmt.Errorf("create workflow run %q: %w", run.ID, err)
	}
	if err := saveHandoffTx(ctx, tx, input); err != nil {
		return err
	}
	events := []Event{
		{
			WorkflowRunID: run.ID, Kind: "workflow_started",
			Summary: "workflow started", CreatedAt: run.StartedAt,
		},
		{
			WorkflowRunID: run.ID, Kind: "handoff_created", NodeID: input.ProducerNodeID,
			Summary: "workflow input created", CreatedAt: input.CreatedAt,
		},
	}
	if err := appendEventsTx(ctx, tx, run.ID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow %q start: %w", run.ID, err)
	}
	workflowStore.publish(events)
	return nil
}

// StartNode atomically creates an Attempt and its started event.
func (workflowStore *Store) StartNode(ctx context.Context, run NodeRunRecord) error {
	inputs, err := json.Marshal(run.InputHandoffIDs)
	if err != nil {
		return fmt.Errorf("marshal workflow node inputs: %w", err)
	}
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow node %q/%q: %w", run.WorkflowRunID, run.NodeID, err)
	}
	defer tx.Rollback()
	if err := lockRunningWorkflow(ctx, tx, run.WorkflowRunID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workflow_node_runs(
		workflow_run_id,node_id,attempt,kind,agent_run_id,input_handoff_ids_json,
		output_handoff_id,status,error_code,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		run.WorkflowRunID, run.NodeID, run.Attempt, run.Kind, "", inputs, "",
		RunRunning, "", store.DatabaseTime(run.StartedAt.UTC().Format(time.RFC3339Nano)),
	); err != nil {
		return fmt.Errorf("create workflow node run %q/%q: %w", run.WorkflowRunID, run.NodeID, err)
	}
	events := []Event{{
		WorkflowRunID: run.WorkflowRunID, Kind: "node_started", NodeID: run.NodeID,
		Summary: "workflow node started", CreatedAt: run.StartedAt,
	}}
	if err := appendEventsTx(ctx, tx, run.WorkflowRunID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow node %q/%q start: %w", run.WorkflowRunID, run.NodeID, err)
	}
	workflowStore.publish(events)
	return nil
}

// SucceedNode atomically records output facts before exposing a successful Attempt.
func (workflowStore *Store) SucceedNode(
	ctx context.Context,
	workflowRunID string,
	nodeID string,
	attempt int,
	agentRunID string,
	handoff Handoff,
	decision *GateDecision,
	usage WorkflowUsage,
	endedAt time.Time,
) error {
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow node %q/%q success: %w", workflowRunID, nodeID, err)
	}
	defer tx.Rollback()
	if err := lockRunningWorkflow(ctx, tx, workflowRunID); err != nil {
		return err
	}
	if err := saveHandoffTx(ctx, tx, handoff); err != nil {
		return err
	}
	var (
		gateDecisionID any
		gateID         any
		gateSubject    any
		gateResult     any
		gateReasons    any
		gateFindings   any
		gateEvaluated  any
	)
	if decision != nil {
		reasons, err := json.Marshal(decision.ReasonCodes)
		if err != nil {
			return fmt.Errorf("marshal gate reason codes: %w", err)
		}
		findings, err := json.Marshal(decision.FindingIDs)
		if err != nil {
			return fmt.Errorf("marshal gate finding ids: %w", err)
		}
		gateDecisionID = "gate_" + handoff.ContentHash[:24]
		gateID = decision.GateID
		gateSubject = decision.SubjectHash
		gateResult = decision.Decision
		gateReasons = reasons
		gateFindings = findings
		gateEvaluated = store.DatabaseTime(
			decision.EvaluatedAt.UTC().Format(time.RFC3339Nano),
		)
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
		SET agent_run_id=?,output_handoff_id=?,status=?,error_code='',
			input_tokens=?,output_tokens=?,reasoning_tokens=?,total_tokens=?,
			tool_call_count=?,cost_micros=?,retry_count=?,
			gate_decision_id=?,gate_id=?,gate_subject_hash=?,gate_decision=?,
			gate_reason_codes_json=?,gate_finding_ids_json=?,gate_evaluated_at=?,
			ended_at=?
		WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`,
		agentRunID, handoff.ID, RunSucceeded,
		usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.TotalTokens,
		usage.ToolCalls, usage.CostMicros, usage.Retries,
		gateDecisionID, gateID, gateSubject, gateResult, gateReasons, gateFindings,
		gateEvaluated,
		store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
		workflowRunID, nodeID, attempt, RunRunning,
	)
	if err != nil {
		return fmt.Errorf("complete workflow node %q/%q: %w", workflowRunID, nodeID, err)
	}
	if err := requireSingleTransition(result, workflowRunID+"/"+nodeID); err != nil {
		return err
	}
	if err := accumulateWorkflowUsageTx(ctx, tx, workflowRunID, usage); err != nil {
		return err
	}
	events := []Event{
		{
			WorkflowRunID: workflowRunID, Kind: "handoff_created", NodeID: nodeID,
			Summary: "node handoff created", CreatedAt: endedAt,
		},
		{
			WorkflowRunID: workflowRunID, Kind: "node_succeeded", NodeID: nodeID,
			Summary: "workflow node succeeded", CreatedAt: endedAt,
		},
	}
	if decision != nil {
		events = append(events, Event{
			WorkflowRunID: workflowRunID, Kind: "gate_evaluated", NodeID: nodeID,
			Summary: "workflow gate evaluated", CreatedAt: endedAt,
		})
	}
	if err := appendEventsTx(ctx, tx, workflowRunID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow node %q/%q success: %w", workflowRunID, nodeID, err)
	}
	workflowStore.publish(events)
	return nil
}

// FailNode atomically closes a non-successful Attempt and appends its terminal event.
func (workflowStore *Store) FailNode(
	ctx context.Context,
	workflowRunID string,
	nodeID string,
	attempt int,
	agentRunID string,
	status RunStatus,
	errorCode string,
	usage WorkflowUsage,
	endedAt time.Time,
) error {
	eventKind := "node_failed"
	summary := "workflow node failed"
	if status == RunWaitingHuman {
		eventKind = "human_review_required"
		summary = "workflow node requires human approval"
	} else if status != RunFailed && status != RunCancelled && status != RunTimedOut {
		return fmt.Errorf("workflow node terminal status %q is invalid", status)
	}
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow node %q/%q failure: %w", workflowRunID, nodeID, err)
	}
	defer tx.Rollback()
	runStatus, err := lockWorkflow(ctx, tx, workflowRunID)
	if err != nil {
		return err
	}
	if runStatus != RunRunning {
		if runStatus == RunCancelled && status == RunCancelled {
			nodeStatus, statusErr := getNodeRunStatusTx(
				ctx, tx, workflowRunID, nodeID, attempt,
			)
			if statusErr == nil && nodeStatus == RunCancelled {
				return nil
			}
			if statusErr != nil {
				return statusErr
			}
		}
		return fmt.Errorf(
			"workflow run %q is %q: %w",
			workflowRunID, runStatus, ErrConflict,
		)
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_node_runs
		SET agent_run_id=?,status=?,error_code=?,
			input_tokens=?,output_tokens=?,reasoning_tokens=?,total_tokens=?,
			tool_call_count=?,cost_micros=?,retry_count=?,ended_at=?
		WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`,
		agentRunID, status, errorCode,
		usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.TotalTokens,
		usage.ToolCalls, usage.CostMicros, usage.Retries,
		store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
		workflowRunID, nodeID, attempt, RunRunning,
	)
	if err != nil {
		return fmt.Errorf("fail workflow node %q/%q: %w", workflowRunID, nodeID, err)
	}
	if err := requireSingleTransition(result, workflowRunID+"/"+nodeID); err != nil {
		return err
	}
	if err := accumulateWorkflowUsageTx(ctx, tx, workflowRunID, usage); err != nil {
		return err
	}
	events := []Event{{
		WorkflowRunID: workflowRunID, Kind: eventKind, NodeID: nodeID,
		Summary: summary, CreatedAt: endedAt,
	}}
	if err := appendEventsTx(ctx, tx, workflowRunID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow node %q/%q failure: %w", workflowRunID, nodeID, err)
	}
	workflowStore.publish(events)
	return nil
}

// FinishWorkflow atomically records an optional final output and the Run terminal state.
func (workflowStore *Store) FinishWorkflow(
	ctx context.Context,
	workflowRunID string,
	status RunStatus,
	errorCode string,
	output *Handoff,
	endedAt time.Time,
) error {
	eventKind, summary, err := workflowTerminalEvent(status)
	if err != nil {
		return err
	}
	tx, err := workflowStore.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workflow %q completion: %w", workflowRunID, err)
	}
	defer tx.Rollback()
	runStatus, err := lockWorkflow(ctx, tx, workflowRunID)
	if err != nil {
		return err
	}
	if runStatus != RunRunning {
		if runStatus == RunCancelled && status == RunCancelled {
			return nil
		}
		return fmt.Errorf(
			"workflow run %q is %q: %w",
			workflowRunID, runStatus, ErrConflict,
		)
	}
	events := make([]Event, 0, 2)
	if output != nil {
		if err := saveHandoffTx(ctx, tx, *output); err != nil {
			return err
		}
		events = append(events, Event{
			WorkflowRunID: workflowRunID, Kind: "handoff_created", NodeID: output.ProducerNodeID,
			Summary: "workflow output created", CreatedAt: endedAt,
		})
	}
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs
		SET status=?,error_code=?,ended_at=? WHERE id=? AND status=?`,
		status, errorCode, store.DatabaseTime(endedAt.UTC().Format(time.RFC3339Nano)),
		workflowRunID, RunRunning,
	)
	if err != nil {
		return fmt.Errorf("complete workflow run %q: %w", workflowRunID, err)
	}
	if err := requireSingleTransition(result, workflowRunID); err != nil {
		return err
	}
	events = append(events, Event{
		WorkflowRunID: workflowRunID, Kind: eventKind, Summary: summary, CreatedAt: endedAt,
	})
	if err := appendEventsTx(ctx, tx, workflowRunID, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workflow %q completion: %w", workflowRunID, err)
	}
	workflowStore.publish(events)
	return nil
}

func accumulateWorkflowUsageTx(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	usage WorkflowUsage,
) error {
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs
		SET input_tokens=input_tokens+?,output_tokens=output_tokens+?,
			reasoning_tokens=reasoning_tokens+?,total_tokens=total_tokens+?,
			tool_call_count=tool_call_count+?,cost_micros=cost_micros+?,
			retry_count=retry_count+?
		WHERE id=? AND status=?`,
		usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.TotalTokens,
		usage.ToolCalls, usage.CostMicros, usage.Retries,
		workflowRunID, RunRunning,
	)
	if err != nil {
		return fmt.Errorf("accumulate workflow run %q usage: %w", workflowRunID, err)
	}
	return requireSingleTransition(result, workflowRunID+"/usage")
}

func lockWorkflow(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
) (RunStatus, error) {
	var status RunStatus
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
		workflowRunID,
	).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("lock workflow run %q: %w", workflowRunID, ErrNotFound)
		}
		return "", fmt.Errorf("lock workflow run %q: %w", workflowRunID, err)
	}
	return status, nil
}

func lockRunningWorkflow(ctx context.Context, tx *sql.Tx, workflowRunID string) error {
	status, err := lockWorkflow(ctx, tx, workflowRunID)
	if err != nil {
		return err
	}
	if status != RunRunning {
		return fmt.Errorf("workflow run %q is %q: %w", workflowRunID, status, ErrConflict)
	}
	return nil
}

func getNodeRunStatusTx(
	ctx context.Context,
	tx *sql.Tx,
	workflowRunID string,
	nodeID string,
	attempt int,
) (RunStatus, error) {
	var status RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT status
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=? AND attempt=? LIMIT 1`,
		workflowRunID, nodeID, attempt,
	).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf(
				"get workflow node %q/%q attempt %d: %w",
				workflowRunID, nodeID, attempt, ErrNotFound,
			)
		}
		return "", fmt.Errorf(
			"get workflow node %q/%q attempt %d: %w",
			workflowRunID, nodeID, attempt, err,
		)
	}
	return status, nil
}

func saveHandoffTx(ctx context.Context, tx *sql.Tx, handoff Handoff) error {
	references, evidenceUnits, evidenceConflicts, err := marshalHandoffJSON(handoff)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO handoff_artifacts(
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,evidence_units_json,evidence_conflicts_json,
		completeness,content_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		handoff.ID, handoff.WorkflowRunID, handoff.ProducerNodeID, handoff.ProducerRunID,
		handoff.Schema.ID, handoff.Schema.Version, handoff.Payload, references,
		evidenceUnits, evidenceConflicts, handoff.Completeness, handoff.ContentHash,
		store.DatabaseTime(handoff.CreatedAt.UTC().Format(time.RFC3339Nano)),
	)
	if err != nil {
		return fmt.Errorf("save handoff %q: %w", handoff.ID, err)
	}
	return nil
}

func workflowTerminalEvent(status RunStatus) (string, string, error) {
	switch status {
	case RunSucceeded:
		return "workflow_succeeded", "workflow succeeded", nil
	case RunFailed:
		return "workflow_failed", "workflow failed", nil
	case RunCancelled:
		return "workflow_cancelled", "workflow cancelled", nil
	case RunTimedOut:
		return "workflow_timed_out", "workflow timed out", nil
	case RunWaitingHuman:
		return "human_review_required", "workflow requires human approval", nil
	default:
		return "", "", fmt.Errorf("workflow terminal status %q is invalid", status)
	}
}
