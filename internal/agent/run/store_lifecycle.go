package run

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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

// RecoverInterrupted recovers work owned by a previous process. Legacy
// non-fenced stores retain the historical abort-and-settle behavior; durable
// fenced stores claim expired roots and enqueue replay work without executing
// model calls during database startup.
func (rs *Store) RecoverInterrupted() (int64, error) {
	if rs != nil && rs.fencingEnabled {
		return rs.recoverDurable()
	}
	ctx := context.Background()
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	tasks, err := loadInterruptedDelegationTasks(ctx, tx, rs.fencingEnabled)
	if err != nil {
		return 0, err
	}
	recoveredAt := store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano))
	query := `UPDATE agent_runs SET status=?,error_code=?,ended_at=? WHERE run_kind=? AND status IN (?,?)`
	if rs.fencingEnabled {
		query = `UPDATE agent_runs r LEFT JOIN agent_run_budget_ledger l ON l.root_run_id=r.id SET r.status=?,r.error_code=?,r.ended_at=? WHERE r.run_kind=? AND r.status IN (?,?) AND (l.root_run_id IS NULL OR l.lease_owner='' OR l.lease_expires_at IS NULL OR l.lease_expires_at<=UTC_TIMESTAMP())`
	}
	result, err := tx.ExecContext(
		ctx,
		query,
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
	// Startup recovery is intentionally conservative: a process-local run is
	// aborted rather than automatically re-entering the model loop. Close any
	// physical attempt left running so later replay cannot mistake it for live
	// work or create an unbounded retry chain.
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_delegation_attempts a
		JOIN agent_delegation_tasks t ON t.parent_run_id=a.parent_run_id
		  AND t.delegation_id=a.delegation_id AND t.task_index=a.task_index
		SET a.status=?,a.retryable=FALSE,a.error_code=?,
			a.error_message=?,a.ended_at=?,a.next_attempt_at=NULL
		WHERE t.admitted=TRUE AND t.settled_usage_json IS NULL
		  AND a.status=?`,
		DelegationAttemptInterrupted, interruptedErrorCode,
		"delegation attempt was interrupted during process recovery", recoveredAt,
		DelegationAttemptRunning,
	); err != nil {
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agent_delegation_checkpoints(
				parent_run_id,delegation_id,task_index,invocation_id,request_hash,status,
				child_run_id,report_artifact_id,error_code,error_message,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE
				status=VALUES(status),child_run_id=VALUES(child_run_id),
				report_artifact_id=VALUES(report_artifact_id),error_code=VALUES(error_code),
				error_message=VALUES(error_message),updated_at=VALUES(updated_at)`,
			task.ParentRunID, task.DelegationID, task.TaskIndex, "", "",
			DelegationCheckpointInterrupted, task.ChildRunID, artifact.ID,
			interruptedErrorCode,
			"delegation was interrupted during process recovery", recoveredAt, recoveredAt,
		); err != nil {
			return 0, fmt.Errorf(
				"persist interrupted delegation checkpoint for child %q: %w",
				task.ChildRunID, err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return recovered, nil
}

type durableRecoveryWork struct {
	RunID           string `json:"run_id"`
	CheckpointStep  int    `json:"checkpoint_step"`
	CheckpointPhase string `json:"checkpoint_phase"`
	LeaseFence      int64  `json:"lease_fence"`
}

// recoverDurable performs the only startup-safe part of recovery: a fenced
// claim. It deliberately does not call the model or mark a resumable parent as
// aborted. The runtime worker can later consume parent_resume and execute the
// checkpoint with the newly claimed fence.
func (rs *Store) recoverDurable() (int64, error) {
	if rs == nil || rs.db == nil || rs.budgetLeaseOwner == "" {
		return 0, fmt.Errorf("agent/runstore: durable recovery requires database and owner")
	}
	ctx := context.Background()
	now := time.Now().UTC()
	ttl := minimumRecoveryLeaseTTL
	expires := now.Add(ttl)
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT r.id,l.lease_fence,c.step_no,c.phase,c.state_json
		FROM agent_runs r
		JOIN agent_run_budget_ledger l ON l.root_run_id=r.id
		LEFT JOIN agent_run_checkpoints c ON c.run_id=r.id
		WHERE r.run_kind=? AND r.status IN (?,?)
		  AND (l.lease_owner='' OR l.lease_expires_at IS NULL OR l.lease_expires_at<=UTC_TIMESTAMP())
		ORDER BY r.started_at
		FOR UPDATE`, KindAgent, StatusRunning, StatusPaused)
	if err != nil {
		return 0, err
	}
	type recoveryCandidate struct {
		runID    string
		oldFence int64
		step     int
		phase    string
	}
	var candidates []recoveryCandidate
	for rows.Next() {
		var runID string
		var oldFence int64
		var step sql.NullInt64
		var phase, state sql.NullString
		if err := rows.Scan(&runID, &oldFence, &step, &phase, &state); err != nil {
			rows.Close()
			return 0, err
		}
		// No valid checkpoint means there is no deterministic replay boundary;
		// leave the run for an explicit operator policy instead of fabricating
		// a new model turn. It must not be silently converted to success.
		if !step.Valid || !phase.Valid || !state.Valid || strings.TrimSpace(state.String) == "" || !json.Valid([]byte(state.String)) || oldFence < 0 {
			continue
		}
		candidates = append(candidates, recoveryCandidate{
			runID: runID, oldFence: oldFence, step: int(step.Int64), phase: phase.String,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	// go-sql-driver/mysql does not permit another statement while a result set
	// is active on the same transaction connection. Closing the fully-buffered
	// candidate rows keeps the SELECT ... FOR UPDATE locks until commit while
	// allowing the fenced UPDATE/enqueue statements below to execute.
	if err := rows.Close(); err != nil {
		return 0, err
	}

	var claimed int64
	for _, candidate := range candidates {
		newFence := candidate.oldFence + 1
		if newFence <= 0 {
			return 0, fmt.Errorf("durable recovery fence overflow for run %q", candidate.runID)
		}
		claimedRoot, err := tx.ExecContext(ctx, `
			UPDATE agent_run_budget_ledger
			SET lease_owner=?,lease_expires_at=?,lease_fence=?,version=version+1,updated_at=?
			WHERE root_run_id=? AND (lease_owner='' OR lease_expires_at IS NULL OR lease_expires_at<=UTC_TIMESTAMP())`,
			rs.budgetLeaseOwner, store.DatabaseTime(expires.Format(time.RFC3339Nano)), newFence,
			store.DatabaseTime(now.Format(time.RFC3339Nano)), candidate.runID)
		if err != nil {
			return 0, err
		}
		if affected, err := claimedRoot.RowsAffected(); err != nil {
			return 0, err
		} else if affected != 1 {
			// The predicate is repeated after SELECT ... FOR UPDATE so a
			// concurrent recovery attempt can never enqueue work for a lease it
			// did not actually acquire.
			continue
		}
		payload, err := json.Marshal(durableRecoveryWork{RunID: candidate.runID, CheckpointStep: candidate.step, CheckpointPhase: candidate.phase, LeaseFence: newFence})
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO agent_work_items(work_id,run_id,parent_run_id,delegation_id,task_index,attempt_no,kind,payload_json,state,available_at,attempt_count,last_error,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE
				payload_json=CASE WHEN state=? THEN payload_json
				                  WHEN state=? AND lease_expires_at>UTC_TIMESTAMP() THEN payload_json
				                  ELSE VALUES(payload_json) END,
				state=CASE WHEN state=? THEN state
				           WHEN state=? AND lease_expires_at>UTC_TIMESTAMP() THEN state
				           ELSE VALUES(state) END,
				available_at=CASE WHEN state=? THEN available_at
				                 WHEN state=? AND lease_expires_at>UTC_TIMESTAMP() THEN available_at
				                 ELSE VALUES(available_at) END,
				updated_at=VALUES(updated_at)`,
			parentResumeWorkID(candidate.runID), candidate.runID, "", "", 0, 1, WorkParentResume, payload, WorkReady, store.DatabaseTime(now.Format(time.RFC3339Nano)), 0, "", store.DatabaseTime(now.Format(time.RFC3339Nano)), store.DatabaseTime(now.Format(time.RFC3339Nano)), WorkSucceeded, WorkRunning, WorkSucceeded, WorkRunning, WorkSucceeded, WorkRunning); err != nil {
			return 0, err
		}
		claimed++
	}
	// Expired child worker leases are safe to replay because ClaimWorkItem
	// increments the work fence and child task persistence is idempotent.
	if _, err := tx.ExecContext(ctx, `
		UPDATE agent_work_items
		SET state=?,lease_owner='',lease_expires_at=NULL,available_at=?,last_error=?,updated_at=?
		WHERE state=? AND lease_expires_at IS NOT NULL AND lease_expires_at<=UTC_TIMESTAMP()`,
		WorkReady, store.DatabaseTime(now.Format(time.RFC3339Nano)), "worker lease expired during recovery", store.DatabaseTime(now.Format(time.RFC3339Nano)), WorkRunning); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return claimed, nil
}

const minimumRecoveryLeaseTTL = time.Minute

const WorkParentResume = "parent_resume"

func parentResumeWorkID(runID string) string { return "parent_resume:" + runID }

func loadInterruptedDelegationTasks(
	ctx context.Context,
	tx *sql.Tx,
	filterExpiredLease bool,
) ([]interruptedDelegationTask, error) {
	query := `SELECT
			t.parent_run_id,t.delegation_id,t.task_index,t.child_run_id,t.capability_id,
			COALESCE(r.input_tokens,0),COALESCE(r.output_tokens,0),
			COALESCE(r.reasoning_tokens,0),COALESCE(r.total_tokens,0),
			COALESCE(r.cost_micros,0),COALESCE(r.tool_call_count,0)
		 FROM agent_delegation_tasks t
		 LEFT JOIN agent_runs r ON r.id=t.child_run_id
		 WHERE t.admitted=TRUE AND t.settled_usage_json IS NULL`
	if filterExpiredLease {
		query += ` AND (NOT EXISTS (SELECT 1 FROM agent_run_budget_ledger l WHERE l.root_run_id=t.parent_run_id AND l.lease_owner<>'' AND l.lease_expires_at>UTC_TIMESTAMP()))`
	}
	query += ` FOR UPDATE`
	rows, err := tx.QueryContext(ctx, query)
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

// CompleteFenced atomically transitions a run only while the caller still owns
// the current root lease fence. A paused process cannot publish a terminal
// outcome after another instance has reclaimed the run.
func (rs *Store) CompleteFenced(id, owner string, fence int64, outcome Outcome) error {
	if owner == "" || fence <= 0 || !outcome.Status.Terminal() {
		return fmt.Errorf("invalid fenced run completion")
	}
	result, err := rs.db.Exec(
		`UPDATE agent_runs r JOIN agent_run_budget_ledger l ON l.root_run_id=r.id
		 SET r.status=?,r.error_code=?,r.step_count=?,r.token_used=?,r.evidence_status=?,r.forced_conclusion=?,
		 r.evidence_result_count=?,r.tool_call_count=?,r.tool_failure_count=?,r.partial_result_count=?,
		 r.omitted_evidence_count=?,r.ended_at=?
		 WHERE r.id=? AND r.status IN (?,?) AND l.lease_owner=? AND l.lease_fence=?
		   AND l.lease_expires_at>UTC_TIMESTAMP()`,
		outcome.Status, outcome.ErrorCode, outcome.StepCount, outcome.TokenUsed, outcome.Evidence.Status,
		outcome.Evidence.ForcedConclusion, outcome.Evidence.ResultCount, outcome.Evidence.ToolCallCount,
		outcome.Evidence.ToolFailureCount, outcome.Evidence.PartialResultCount, outcome.Evidence.OmittedItemCount,
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339)), id, StatusRunning, StatusPaused, owner, fence,
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
