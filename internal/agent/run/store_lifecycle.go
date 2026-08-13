package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

const (
	qaParentStreamKind        = "qa_parent"
	qaParentTerminalEventKind = "run_finished"
	qaParentTerminalEventSeq  = int64(1)
)

// RecoverInterrupted closes process-local Agent Runs left active by a prior process.
func (rs *RunStore) RecoverInterrupted() (int64, error) {
	result, err := rs.db.Exec(
		`UPDATE agent_runs SET status=?,ended_at=? WHERE run_kind=? AND status IN (?,?)`,
		RunStatusAborted, store.DatabaseTime(time.Now().UTC().Format(time.RFC3339)),
		RunKindAgent, RunStatusRunning, RunStatusPaused,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (rs *RunStore) Create(r RunRecord) error {
	if r.RunKind == "" {
		r.RunKind = RunKindAgent
	}
	if r.StartedAt == "" {
		r.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if r.Status == "" {
		r.Status = RunStatusRunning
	}
	selectionJSON, err := json.Marshal(r.Selection)
	if err != nil {
		return fmt.Errorf("marshal run %q selection: %w", r.ID, err)
	}
	_, err = rs.db.Exec(
		`INSERT INTO agent_runs(
			id,run_kind,user_id,session_id,agent_id,definition_version,definition_hash,selection_json,tool_snapshot_id,
			input_schema_version,output_schema_version,parent_run_id,workflow_run_id,workflow_node_id,
			question,status,error_code,mode,max_steps,step_count,token_used,started_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.RunKind, r.UserID, r.SessionID, r.AgentID, r.DefinitionVersion, r.DefinitionHash,
		selectionJSON, r.ToolSnapshotID,
		r.InputSchemaVersion, r.OutputSchemaVersion, r.ParentRunID, r.WorkflowRunID, r.WorkflowNodeID,
		r.Question, r.Status, r.ErrorCode, r.Mode, r.MaxSteps, 0, 0, store.DatabaseTime(r.StartedAt))
	return err
}

// Complete atomically moves an active Run to one terminal state.
func (rs *RunStore) Complete(id string, outcome RunOutcome) error {
	if !outcome.Status.Terminal() {
		return fmt.Errorf("agent: complete run with non-terminal status %q", outcome.Status)
	}
	result, err := rs.db.Exec(
		`UPDATE agent_runs
		 SET status=?,error_code=?,step_count=?,token_used=?,evidence_status=?,forced_conclusion=?,
			evidence_result_count=?,tool_call_count=?,tool_failure_count=?,partial_result_count=?,
			omitted_evidence_count=?,ended_at=?
		 WHERE id=? AND status IN (?,?)`,
		outcome.Status, outcome.ErrorCode, outcome.StepCount, outcome.TokenUsed, outcome.Evidence.Status,
		outcome.Evidence.ForcedConclusion, outcome.Evidence.ResultCount,
		outcome.Evidence.ToolCallCount, outcome.Evidence.ToolFailureCount,
		outcome.Evidence.PartialResultCount, outcome.Evidence.OmittedItemCount,
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339)), id,
		RunStatusRunning, RunStatusPaused,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrRunNotActive
	}
	return nil
}

// CompleteQAParent commits the Parent state and its recoverable result together.
func (rs *RunStore) CompleteQAParent(
	ctx context.Context,
	id string,
	outcome RunOutcome,
) (RunOutcome, error) {
	if !outcome.Status.Terminal() {
		return RunOutcome{}, fmt.Errorf(
			"agent: complete QA parent with non-terminal status %q",
			outcome.Status,
		)
	}
	terminal := terminalFromOutcome(id, outcome)
	detail, err := json.Marshal(terminal)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("marshal QA parent %q terminal result: %w", id, err)
	}
	completedAt := time.Now().UTC()
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("begin QA parent %q completion: %w", id, err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(
		ctx,
		`UPDATE agent_runs
		 SET status=?,error_code=?,step_count=?,token_used=?,evidence_status=?,forced_conclusion=?,
			evidence_result_count=?,tool_call_count=?,tool_failure_count=?,partial_result_count=?,
			omitted_evidence_count=?,ended_at=?
		 WHERE id=? AND run_kind=? AND status IN (?,?)`,
		outcome.Status, outcome.ErrorCode, outcome.StepCount, outcome.TokenUsed, outcome.Evidence.Status,
		outcome.Evidence.ForcedConclusion, outcome.Evidence.ResultCount,
		outcome.Evidence.ToolCallCount, outcome.Evidence.ToolFailureCount,
		outcome.Evidence.PartialResultCount, outcome.Evidence.OmittedItemCount,
		store.DatabaseTime(completedAt.Format(time.RFC3339Nano)), id, RunKindQAParent,
		RunStatusRunning, RunStatusPaused,
	)
	if err != nil {
		return RunOutcome{}, fmt.Errorf("complete QA parent %q: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return RunOutcome{}, fmt.Errorf("read QA parent %q completion result: %w", id, err)
	}
	if affected == 0 {
		persisted, err := loadPersistedQAParentTerminal(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return RunOutcome{}, ErrRunNotActive
		}
		if err != nil {
			return RunOutcome{}, err
		}
		return outcomeFromTerminal(persisted), nil
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO runtime_events(
			stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		qaParentStreamKind, id, qaParentTerminalEventSeq, qaParentTerminalEventKind,
		"", string(outcome.Status), detail,
		store.DatabaseTime(completedAt.Format(time.RFC3339Nano)),
	); err != nil {
		return RunOutcome{}, fmt.Errorf("append QA parent %q terminal event: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return RunOutcome{}, fmt.Errorf("commit QA parent %q completion: %w", id, err)
	}
	return outcomeFromTerminal(terminal), nil
}

func loadPersistedQAParentTerminal(
	ctx context.Context,
	tx *sql.Tx,
	runID string,
) (RunTerminal, error) {
	var (
		status RunStatus
		detail []byte
	)
	err := tx.QueryRowContext(
		ctx,
		`SELECT r.status,e.detail_json
		 FROM agent_runs r
		 LEFT JOIN runtime_events e
		   ON e.stream_kind=? AND e.stream_id=r.id AND e.seq=? AND e.kind=?
		 WHERE r.id=? AND r.run_kind=? LIMIT 1`,
		qaParentStreamKind,
		qaParentTerminalEventSeq,
		qaParentTerminalEventKind,
		runID,
		RunKindQAParent,
	).Scan(&status, &detail)
	if err != nil {
		return RunTerminal{}, err
	}
	if !status.Terminal() {
		return RunTerminal{}, ErrRunNotActive
	}
	if len(detail) == 0 {
		return RunTerminal{}, fmt.Errorf(
			"QA parent %q is terminal without a durable terminal event",
			runID,
		)
	}
	terminal, err := decodeQAParentTerminal(runID, detail)
	if err != nil {
		return RunTerminal{}, err
	}
	if terminal.Status != status {
		return RunTerminal{}, fmt.Errorf(
			"QA parent %q event status %q does not match run status %q",
			runID,
			terminal.Status,
			status,
		)
	}
	return terminal, nil
}

// TransitionControl updates the persisted pause state without changing terminal fields.
func (rs *RunStore) TransitionControl(id string, from, to RunStatus) error {
	if !validControlTransition(from, to) {
		return fmt.Errorf("agent: invalid run control transition %q -> %q", from, to)
	}
	result, err := rs.db.Exec(`UPDATE agent_runs SET status=? WHERE id=? AND status=?`, to, id, from)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrRunNotActive
	}
	return nil
}

// SetMaxSteps records the plan-specific loop bound resolved after routing.
func (rs *RunStore) SetMaxSteps(id string, maxSteps int) error {
	_, err := rs.db.Exec(`UPDATE agent_runs SET max_steps=? WHERE id=?`, maxSteps, id)
	return err
}

func (rs *RunStore) DeleteBySession(sessionID string, userID int64) error {
	tx, err := rs.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`DELETE c FROM agent_llm_calls c JOIN agent_runs r ON c.run_id = r.id WHERE r.session_id = ? AND r.user_id=?`,
		sessionID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE s FROM agent_steps s JOIN agent_runs r ON s.run_id = r.id WHERE r.session_id = ? AND r.user_id=?`,
		sessionID, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agent_runs WHERE session_id = ? AND user_id=?`, sessionID, userID); err != nil {
		return err
	}
	return tx.Commit()
}
