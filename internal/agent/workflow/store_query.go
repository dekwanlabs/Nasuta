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

func (workflowStore *Store) GetRun(ctx context.Context, id string) (*RunRecord, error) {
	var run RunRecord
	row := workflowStore.db.QueryRowContext(ctx, `SELECT
		id,parent_run_id,round_number,base_depth,workflow_id,workflow_version,workflow_hash,selection_json,input_hash,actor_user_id,
		actor_tenant_id,actor_permissions_json,scenario,scenario_permissions_json,
		status,budget_json,input_tokens,output_tokens,reasoning_tokens,total_tokens,
		tool_call_count,cost_micros,retry_count,error_code,stop_reason,started_at,ended_at
		FROM workflow_runs WHERE id=? LIMIT 1`, id)
	if err := scanRecord(row, &run); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get workflow run %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("get workflow run %q: %w", id, err)
	}
	return &run, nil
}

// LoadTerminalResult reads only the Run and its canonical output artifact.
func (workflowStore *Store) LoadTerminalResult(
	ctx context.Context,
	workflowRunID string,
) (TerminalResult, error) {
	run, err := workflowStore.GetRun(ctx, workflowRunID)
	if err != nil {
		return TerminalResult{}, err
	}
	result := TerminalResult{Run: *run}
	switch run.Status {
	case RunRunning, RunWaitingHuman:
		return TerminalResult{}, fmt.Errorf(
			"workflow run %q is %q: %w",
			workflowRunID, run.Status, ErrConflict,
		)
	case RunFailed, RunCancelled, RunTimedOut:
		return result, nil
	case RunSucceeded:
	default:
		return TerminalResult{}, fmt.Errorf(
			"workflow run %q has unknown status %q: %w",
			workflowRunID, run.Status, ErrInvariant,
		)
	}
	row := workflowStore.db.QueryRowContext(ctx, `SELECT
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,evidence_units_json,evidence_conflicts_json,
		completeness,content_hash,created_at
		FROM handoff_artifacts
		WHERE workflow_run_id=? AND producer_node_id='workflow.output'
		LIMIT 1`, workflowRunID)
	output, err := scanHandoff(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TerminalResult{}, fmt.Errorf(
				"workflow run %q succeeded without workflow.output: %w",
				workflowRunID, ErrInvariant,
			)
		}
		return TerminalResult{}, fmt.Errorf(
			"load workflow run %q output: %w",
			workflowRunID, err,
		)
	}
	result.Output = &output
	return result, nil
}

// ListNodeRuns returns all Attempts in stable keyset order.
func (workflowStore *Store) ListNodeRuns(
	ctx context.Context,
	workflowRunID string,
	cursor NodeRunCursor,
	limit int,
) ([]NodeRunRecord, error) {
	limit = boundedLimit(limit)
	var (
		rows *sql.Rows
		err  error
	)
	const columns = `SELECT
		current.workflow_run_id,current.node_id,current.attempt,current.kind,
		current.agent_run_id,current.input_handoff_ids_json,current.output_handoff_id,
		current.status,current.error_code,current.input_tokens,current.output_tokens,
		current.reasoning_tokens,current.total_tokens,current.tool_call_count,
		current.cost_micros,current.retry_count,current.started_at,
		(SELECT MIN(first.started_at) FROM workflow_node_runs first
			WHERE first.workflow_run_id=current.workflow_run_id
			AND first.node_id=current.node_id),
		current.ended_at
		FROM workflow_node_runs current`
	if cursor.NodeID == "" {
		rows, err = workflowStore.db.QueryContext(
			ctx,
			columns+`
			WHERE current.workflow_run_id=?
			ORDER BY current.node_id,current.attempt LIMIT ?`,
			workflowRunID,
			limit,
		)
	} else {
		rows, err = workflowStore.db.QueryContext(
			ctx,
			columns+`
			WHERE current.workflow_run_id=?
			AND (current.node_id>? OR (current.node_id=? AND current.attempt>?))
			ORDER BY current.node_id,current.attempt LIMIT ?`,
			workflowRunID,
			cursor.NodeID,
			cursor.NodeID,
			cursor.Attempt,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list workflow node runs %q: %w", workflowRunID, err)
	}
	defer rows.Close()
	runs := make([]NodeRunRecord, 0, limit)
	for rows.Next() {
		run, err := scanNodeRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan workflow node run %q: %w", workflowRunID, err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow node runs %q: %w", workflowRunID, err)
	}
	return runs, nil
}

// ListActiveRuns pages only resumable run identities below a startup cutoff.
func (workflowStore *Store) ListActiveRuns(
	ctx context.Context,
	startedBefore time.Time,
	cursor ActiveRunCursor,
	limit int,
) ([]ActiveRunRef, error) {
	limit = boundedLimit(limit)
	var (
		rows *sql.Rows
		err  error
	)
	if cursor.ID == "" {
		rows, err = workflowStore.db.QueryContext(ctx, `SELECT id,started_at
			FROM workflow_runs
			WHERE status=? AND started_at<?
			ORDER BY started_at,id LIMIT ?`,
			RunRunning,
			store.DatabaseTime(startedBefore.UTC().Format(time.RFC3339Nano)),
			limit,
		)
	} else {
		rows, err = workflowStore.db.QueryContext(ctx, `SELECT id,started_at
			FROM workflow_runs
			WHERE status=? AND started_at<?
			AND (started_at>? OR (started_at=? AND id>?))
			ORDER BY started_at,id LIMIT ?`,
			RunRunning,
			store.DatabaseTime(startedBefore.UTC().Format(time.RFC3339Nano)),
			store.DatabaseTime(cursor.StartedAt.UTC().Format(time.RFC3339Nano)),
			store.DatabaseTime(cursor.StartedAt.UTC().Format(time.RFC3339Nano)),
			cursor.ID,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list active workflow runs: %w", err)
	}
	defer rows.Close()
	runs := make([]ActiveRunRef, 0, limit)
	for rows.Next() {
		var run ActiveRunRef
		if err := rows.Scan(&run.ID, &run.StartedAt); err != nil {
			return nil, fmt.Errorf("scan active workflow run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active workflow runs: %w", err)
	}
	return runs, nil
}

func (workflowStore *Store) ListHandoffs(ctx context.Context, workflowRunID string, cursor HandoffCursor, limit int) ([]Handoff, error) {
	limit = boundedLimit(limit)
	var (
		rows *sql.Rows
		err  error
	)
	if cursor.ID == "" {
		rows, err = workflowStore.db.QueryContext(ctx, `SELECT
			id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
			payload_json,references_json,evidence_units_json,evidence_conflicts_json,
			completeness,content_hash,created_at
			FROM handoff_artifacts
			WHERE workflow_run_id=?
			ORDER BY created_at,id LIMIT ?`,
			workflowRunID, limit,
		)
	} else {
		rows, err = workflowStore.db.QueryContext(ctx, `SELECT
			id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
			payload_json,references_json,evidence_units_json,evidence_conflicts_json,
			completeness,content_hash,created_at
			FROM handoff_artifacts
			WHERE workflow_run_id=? AND (created_at>? OR (created_at=? AND id>?))
			ORDER BY created_at,id LIMIT ?`,
			workflowRunID,
			store.DatabaseTime(cursor.CreatedAt.UTC().Format(time.RFC3339Nano)),
			store.DatabaseTime(cursor.CreatedAt.UTC().Format(time.RFC3339Nano)),
			cursor.ID,
			limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list handoffs %q: %w", workflowRunID, err)
	}
	defer rows.Close()
	handoffs := make([]Handoff, 0, limit)
	for rows.Next() {
		handoff, err := scanHandoff(rows)
		if err != nil {
			return nil, fmt.Errorf("scan handoff: %w", err)
		}
		handoffs = append(handoffs, handoff)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate handoffs: %w", err)
	}
	return handoffs, nil
}

func scanHandoff(row rowScanner) (Handoff, error) {
	var handoff Handoff
	var references, evidenceUnits, evidenceConflicts []byte
	if err := row.Scan(
		&handoff.ID, &handoff.WorkflowRunID, &handoff.ProducerNodeID,
		&handoff.ProducerRunID, &handoff.Schema.ID, &handoff.Schema.Version,
		&handoff.Payload, &references, &evidenceUnits, &evidenceConflicts,
		&handoff.Completeness, &handoff.ContentHash, &handoff.CreatedAt,
	); err != nil {
		return Handoff{}, err
	}
	if err := json.Unmarshal(references, &handoff.References); err != nil {
		return Handoff{}, fmt.Errorf("decode handoff references: %w", err)
	}
	if err := json.Unmarshal(evidenceUnits, &handoff.EvidenceUnits); err != nil {
		return Handoff{}, fmt.Errorf("decode handoff evidence units: %w", err)
	}
	if err := json.Unmarshal(evidenceConflicts, &handoff.EvidenceConflicts); err != nil {
		return Handoff{}, fmt.Errorf("decode handoff evidence conflicts: %w", err)
	}
	return handoff, nil
}

func scanNodeRun(row rowScanner) (NodeRunRecord, error) {
	var run NodeRunRecord
	var inputs []byte
	var endedAt sql.NullTime
	if err := row.Scan(
		&run.WorkflowRunID,
		&run.NodeID,
		&run.Attempt,
		&run.Kind,
		&run.AgentRunID,
		&inputs,
		&run.OutputHandoffID,
		&run.Status,
		&run.ErrorCode,
		&run.Usage.InputTokens,
		&run.Usage.OutputTokens,
		&run.Usage.ReasoningTokens,
		&run.Usage.TotalTokens,
		&run.Usage.ToolCalls,
		&run.Usage.CostMicros,
		&run.Usage.Retries,
		&run.StartedAt,
		&run.FirstStartedAt,
		&endedAt,
	); err != nil {
		return NodeRunRecord{}, err
	}
	if err := json.Unmarshal(inputs, &run.InputHandoffIDs); err != nil {
		return NodeRunRecord{}, fmt.Errorf("decode input handoffs: %w", err)
	}
	if endedAt.Valid {
		ended := endedAt.Time
		run.EndedAt = &ended
	}
	return run, nil
}

func scanRecord(row rowScanner, run *RunRecord) error {
	var selection, budget, actorPermissions, scenarioPermissions []byte
	var endedAt sql.NullTime
	if err := row.Scan(
		&run.ID, &run.ParentRunID, &run.Round, &run.BaseDepth,
		&run.WorkflowID, &run.WorkflowVersion, &run.WorkflowHash,
		&selection, &run.InputHash, &run.ActorUserID, &run.ActorTenantID, &actorPermissions,
		&run.Scenario, &scenarioPermissions, &run.Status, &budget,
		&run.Usage.InputTokens, &run.Usage.OutputTokens, &run.Usage.ReasoningTokens,
		&run.Usage.TotalTokens, &run.Usage.ToolCalls, &run.Usage.CostMicros,
		&run.Usage.Retries, &run.ErrorCode, &run.StopReason,
		&run.StartedAt, &endedAt,
	); err != nil {
		return err
	}
	if err := json.Unmarshal(selection, &run.Selection); err != nil {
		return fmt.Errorf("decode workflow definition selection: %w", err)
	}
	if err := json.Unmarshal(budget, &run.Budget); err != nil {
		return fmt.Errorf("decode budget: %w", err)
	}
	if err := json.Unmarshal(actorPermissions, &run.ActorPermissions); err != nil {
		return fmt.Errorf("decode actor permissions: %w", err)
	}
	if err := json.Unmarshal(scenarioPermissions, &run.ScenarioPermissions); err != nil {
		return fmt.Errorf("decode scenario permissions: %w", err)
	}
	if endedAt.Valid {
		ended := endedAt.Time
		run.EndedAt = &ended
	}
	return nil
}
