package run

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

const interruptedErrorCode = "interrupted"

type interruptedDelegationTask struct {
	ParentRunID  string
	DelegationID string
	TaskIndex    int
	ChildRunID   string
	CapabilityID string
	Usage        agentapi.Usage
	ToolCalls    int64
}

// RecoverInterrupted closes process-local Agent Runs and settles delegation
// reservations left active by a prior process.
func (rs *Store) RecoverInterrupted() (int64, error) {
	ctx := context.Background()
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	tasks, err := loadInterruptedDelegationTasks(ctx, tx)
	if err != nil {
		return 0, err
	}
	recoveredAt := store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano))
	result, err := tx.ExecContext(
		ctx,
		`UPDATE agent_runs SET status=?,error_code=?,ended_at=?
		 WHERE run_kind=? AND status IN (?,?)`,
		StatusAborted,
		interruptedErrorCode,
		recoveredAt,
		KindAgent,
		StatusRunning,
		StatusPaused,
	)
	if err != nil {
		return 0, err
	}
	recovered, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	for _, task := range tasks {
		artifact, err := interruptedDelegationReportArtifact(task)
		if err != nil {
			return 0, err
		}
		if err := insertRunArtifact(ctx, tx, artifact); err != nil {
			return 0, fmt.Errorf(
				"persist interrupted delegation report for child %q: %w",
				task.ChildRunID,
				err,
			)
		}
		usageRaw, err := json.Marshal(task.Usage)
		if err != nil {
			return 0, fmt.Errorf(
				"marshal interrupted delegation usage for child %q: %w",
				task.ChildRunID,
				err,
			)
		}
		settled, err := tx.ExecContext(
			ctx,
			`UPDATE agent_delegation_tasks
			 SET settled_usage_json=?,report_artifact_id=?,settled_at=?
			 WHERE parent_run_id=? AND delegation_id=? AND task_index=?
			   AND child_run_id=? AND admitted=TRUE
			   AND settled_usage_json IS NULL`,
			usageRaw,
			artifact.ID,
			recoveredAt,
			task.ParentRunID,
			task.DelegationID,
			task.TaskIndex,
			task.ChildRunID,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"settle interrupted delegation child %q: %w",
				task.ChildRunID,
				err,
			)
		}
		affected, err := settled.RowsAffected()
		if err != nil {
			return 0, err
		}
		if affected != 1 {
			return 0, ErrDelegationTaskConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return recovered, nil
}

func loadInterruptedDelegationTasks(
	ctx context.Context,
	tx *sql.Tx,
) ([]interruptedDelegationTask, error) {
	rows, err := tx.QueryContext(
		ctx,
		`SELECT
			t.parent_run_id,t.delegation_id,t.task_index,t.child_run_id,t.capability_id,
			COALESCE(r.input_tokens,0),COALESCE(r.output_tokens,0),
			COALESCE(r.reasoning_tokens,0),COALESCE(r.total_tokens,0),
			COALESCE(r.cost_micros,0),COALESCE(r.tool_call_count,0)
		 FROM agent_delegation_tasks t
		 LEFT JOIN agent_runs r ON r.id=t.child_run_id
		 WHERE t.admitted=TRUE AND t.settled_usage_json IS NULL
		 FOR UPDATE`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []interruptedDelegationTask
	for rows.Next() {
		var task interruptedDelegationTask
		if err := rows.Scan(
			&task.ParentRunID,
			&task.DelegationID,
			&task.TaskIndex,
			&task.ChildRunID,
			&task.CapabilityID,
			&task.Usage.InputTokens,
			&task.Usage.OutputTokens,
			&task.Usage.ReasoningTokens,
			&task.Usage.TotalTokens,
			&task.Usage.CostMicros,
			&task.ToolCalls,
		); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func interruptedDelegationReportArtifact(
	task interruptedDelegationTask,
) (DelegationArtifact, error) {
	if task.ChildRunID == "" || task.CapabilityID == "" {
		return DelegationArtifact{}, fmt.Errorf(
			"invalid interrupted delegation task %q/%q/%d",
			task.ParentRunID,
			task.DelegationID,
			task.TaskIndex,
		)
	}
	if task.CapabilityID == "evidence.semantic.verify" {
		return interruptedDelegationVerificationArtifact(task)
	}
	reportID := stableDelegationReportID(task.ChildRunID)
	report := agentapi.DelegationReport{
		RunID:        task.ChildRunID,
		ReportID:     reportID,
		Capability:   task.CapabilityID,
		Status:       agentapi.DelegationInterrupted,
		Completeness: agentapi.DelegationIncomplete,
		Usage: agentapi.DelegationUsage{
			ToolCalls:       task.ToolCalls,
			InputTokens:     task.Usage.InputTokens,
			OutputTokens:    task.Usage.OutputTokens,
			ReasoningTokens: task.Usage.ReasoningTokens,
			TotalTokens:     task.Usage.TotalTokens,
			CostMicros:      task.Usage.CostMicros,
		},
		Error: &agentapi.RunError{
			Code:    interruptedErrorCode,
			Message: "delegation execution was interrupted before a durable report was available",
		},
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return DelegationArtifact{}, err
	}
	sum := sha256.Sum256(raw)
	return DelegationArtifact{
		ID:          delegationReportArtifactID(reportID),
		RunID:       task.ChildRunID,
		Kind:        DelegationReportArtifactKind,
		Schema:      agentapi.SchemaRef{ID: "delegation.report", Version: 1},
		ContentHash: fmt.Sprintf("%x", sum[:]),
		Content:     raw,
	}, nil
}

func interruptedDelegationVerificationArtifact(
	task interruptedDelegationTask,
) (DelegationArtifact, error) {
	verificationID := stableDelegationVerificationID(task.ChildRunID)
	verification := agentapi.DelegationVerification{
		RunID: task.ChildRunID, VerificationID: verificationID,
		Status: agentapi.DelegationInterrupted,
		Usage: agentapi.DelegationUsage{
			ToolCalls:       task.ToolCalls,
			InputTokens:     task.Usage.InputTokens,
			OutputTokens:    task.Usage.OutputTokens,
			ReasoningTokens: task.Usage.ReasoningTokens,
			TotalTokens:     task.Usage.TotalTokens,
			CostMicros:      task.Usage.CostMicros,
		},
		Error: &agentapi.RunError{
			Code:    interruptedErrorCode,
			Message: "semantic verification was interrupted before a durable result was available",
		},
	}
	raw, err := json.Marshal(verification)
	if err != nil {
		return DelegationArtifact{}, err
	}
	sum := sha256.Sum256(raw)
	return DelegationArtifact{
		ID:          stableDelegationArtifactID(verificationID),
		RunID:       task.ChildRunID,
		Kind:        DelegationVerificationArtifactKind,
		Schema:      agentapi.SchemaRef{ID: "delegation.verification.artifact", Version: 1},
		ContentHash: fmt.Sprintf("%x", sum[:]),
		Content:     raw,
	}, nil
}

func stableDelegationReportID(childRunID string) string {
	sum := sha256.Sum256([]byte(childRunID))
	return "report_" + fmt.Sprintf("%x", sum[:12])
}

func stableDelegationVerificationID(childRunID string) string {
	sum := sha256.Sum256([]byte(childRunID))
	return "verification_" + fmt.Sprintf("%x", sum[:12])
}

func stableDelegationArtifactID(referenceID string) string {
	sum := sha256.Sum256([]byte(referenceID))
	return "artifact_" + fmt.Sprintf("%x", sum[:12])
}

func (rs *Store) Create(r Record) error {
	if r.RunKind == "" {
		r.RunKind = KindAgent
	}
	if r.StartedAt == "" {
		r.StartedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if r.Status == "" {
		r.Status = StatusRunning
	}
	selectionJSON, err := json.Marshal(r.Selection)
	if err != nil {
		return fmt.Errorf("marshal run %q selection: %w", r.ID, err)
	}
	limitsJSON, err := json.Marshal(r.RunLimits)
	if err != nil {
		return fmt.Errorf("marshal run %q limits: %w", r.ID, err)
	}
	_, err = rs.db.Exec(
		`INSERT INTO agent_runs(
			id,run_kind,user_id,session_id,agent_id,definition_version,definition_hash,selection_json,tool_snapshot_id,
			input_schema_version,output_schema_version,parent_run_id,capability_id,capability_version,
			capability_content_hash,delegation_id,delegation_depth,run_limits_json,capability_registry_revision,
			workflow_run_id,workflow_node_id,
			question,status,error_code,mode,max_steps,step_count,token_used,started_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.RunKind, r.UserID, r.SessionID, r.AgentID, r.DefinitionVersion, r.DefinitionHash,
		selectionJSON, r.ToolSnapshotID,
		r.InputSchemaVersion, r.OutputSchemaVersion, r.ParentRunID,
		r.CapabilityID, r.CapabilityVersion, r.CapabilityHash,
		r.DelegationID, r.DelegationDepth, limitsJSON, r.CapabilityRevision,
		r.WorkflowRunID, r.WorkflowNodeID,
		r.Question, r.Status, r.ErrorCode, r.Mode, r.MaxSteps, 0, 0, store.DatabaseTime(r.StartedAt))
	return err
}

// Complete atomically moves an active Run to one terminal state.
func (rs *Store) Complete(id string, outcome Outcome) error {
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
		StatusRunning, StatusPaused,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrNotActive
	}
	return nil
}

// TransitionControl updates the persisted pause state without changing terminal fields.
func (rs *Store) TransitionControl(id string, from, to Status) error {
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
		return ErrNotActive
	}
	return nil
}

// SetMaxSteps records the plan-specific loop bound resolved after routing.
func (rs *Store) SetMaxSteps(id string, maxSteps int) error {
	_, err := rs.db.Exec(`UPDATE agent_runs SET max_steps=? WHERE id=?`, maxSteps, id)
	return err
}

func (rs *Store) DeleteBySession(sessionID string, userID int64) error {
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
