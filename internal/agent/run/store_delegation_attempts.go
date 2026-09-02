package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// StartDelegationAttempt persists an idempotent running attempt before the
// child runtime is invoked. It is safe to replay after a process crash.
func (rs *Store) StartDelegationAttempt(
	ctx context.Context,
	start DelegationAttemptStart,
) (DelegationAttemptRecord, error) {
	if err := validateAttemptStart(start); err != nil {
		return DelegationAttemptRecord{}, err
	}
	_, err := rs.db.ExecContext(ctx, `
		INSERT INTO agent_delegation_attempts(
			parent_run_id,delegation_id,task_index,attempt_no,attempt_id,child_run_id,
			status,retryable,error_code,error_message,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE attempt_id=attempt_id`,
		start.ParentRunID, start.DelegationID, start.TaskIndex, start.AttemptNo,
		start.AttemptID, start.ChildRunID, DelegationAttemptRunning, false, "", "",
		store.DatabaseTime(start.StartedAt),
	)
	if err != nil {
		return DelegationAttemptRecord{}, err
	}
	record, err := rs.getDelegationAttempt(ctx, start.ParentRunID, start.DelegationID, start.TaskIndex, start.AttemptNo)
	if err != nil {
		return DelegationAttemptRecord{}, err
	}
	if record.AttemptID != start.AttemptID || record.ChildRunID != start.ChildRunID {
		return DelegationAttemptRecord{}, ErrDelegationTaskConflict
	}
	record.Existing = record.Status != DelegationAttemptRunning
	return record, nil
}

// FinishDelegationAttempt closes one durable attempt. Repeating the same finish
// is idempotent; a different finish for the same attempt is rejected.
func (rs *Store) FinishDelegationAttempt(
	ctx context.Context,
	finish DelegationAttemptFinish,
) (DelegationAttemptRecord, error) {
	if err := validateAttemptFinish(finish); err != nil {
		return DelegationAttemptRecord{}, err
	}
	usageRaw, err := marshalOptionalUsage(finish.Usage)
	if err != nil {
		return DelegationAttemptRecord{}, err
	}
	result, err := rs.db.ExecContext(ctx, `
		UPDATE agent_delegation_attempts
		SET status=?,retryable=?,error_code=?,error_message=?,ended_at=?,next_attempt_at=?,
			usage_json=?,report_artifact_id=?
		WHERE parent_run_id=? AND delegation_id=? AND task_index=? AND attempt_no=?
		  AND attempt_id=? AND status=?`,
		finish.Status, finish.Retryable, finish.ErrorCode, finish.ErrorMessage,
		store.DatabaseTime(finish.EndedAt), store.DatabaseTime(finish.NextAttemptAt),
		usageRaw, finish.ReportArtifactID,
		finish.ParentRunID, finish.DelegationID, finish.TaskIndex, finish.AttemptNo,
		finish.AttemptID, DelegationAttemptRunning,
	)
	if err != nil {
		return DelegationAttemptRecord{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return DelegationAttemptRecord{}, err
	}
	if affected == 0 {
		record, loadErr := rs.getDelegationAttempt(ctx, finish.ParentRunID, finish.DelegationID, finish.TaskIndex, finish.AttemptNo)
		if loadErr != nil {
			return DelegationAttemptRecord{}, loadErr
		}
		if !sameAttemptFinish(record, finish) {
			return DelegationAttemptRecord{}, ErrDelegationTaskConflict
		}
		return record, nil
	}
	return rs.getDelegationAttempt(ctx, finish.ParentRunID, finish.DelegationID, finish.TaskIndex, finish.AttemptNo)
}

// GetLatestDelegationAttempt returns the most recent attempt for recovery.
func (rs *Store) GetLatestDelegationAttempt(
	ctx context.Context,
	parentRunID, delegationID string,
	taskIndex int,
) (DelegationAttemptRecord, error) {
	if strings.TrimSpace(parentRunID) == "" || strings.TrimSpace(delegationID) == "" || taskIndex < 0 {
		return DelegationAttemptRecord{}, fmt.Errorf("invalid delegation attempt lookup")
	}
	return rs.getDelegationAttempt(ctx, parentRunID, delegationID, taskIndex, 0)
}

// LinkDelegationAttemptChild switches the task's current report owner to a
// retry attempt. The immutable reservation child identity remains in JSON.
func (rs *Store) LinkDelegationAttemptChild(
	ctx context.Context,
	parentRunID, delegationID string,
	taskIndex, attemptNo int,
	childRunID string,
) error {
	if parentRunID == "" || delegationID == "" || taskIndex < 0 || attemptNo <= 0 || childRunID == "" {
		return fmt.Errorf("invalid delegation attempt child link")
	}
	result, err := rs.db.ExecContext(ctx, `
		UPDATE agent_delegation_tasks t
		JOIN agent_delegation_attempts a ON a.parent_run_id=t.parent_run_id
		  AND a.delegation_id=t.delegation_id AND a.task_index=t.task_index
		  AND a.attempt_no=? AND a.attempt_id IS NOT NULL
		SET t.child_run_id=?
		WHERE t.parent_run_id=? AND t.delegation_id=? AND t.task_index=?
		  AND t.admitted=TRUE AND t.settled_usage_json IS NULL AND a.child_run_id=?`,
		attemptNo, childRunID, parentRunID, delegationID, taskIndex, childRunID,
	)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 1 {
		return nil
	}
	var current string
	if err := rs.db.QueryRowContext(ctx, `
		SELECT child_run_id FROM agent_delegation_tasks
		WHERE parent_run_id=? AND delegation_id=? AND task_index=? AND admitted=TRUE`,
		parentRunID, delegationID, taskIndex).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDelegationTaskConflict
		}
		return err
	}
	if current != childRunID {
		return ErrDelegationTaskConflict
	}
	return nil
}

func (rs *Store) getDelegationAttempt(
	ctx context.Context,
	parentRunID, delegationID string,
	taskIndex, attemptNo int,
) (DelegationAttemptRecord, error) {
	query := `SELECT parent_run_id,delegation_id,task_index,attempt_no,attempt_id,child_run_id,
		status,retryable,error_code,error_message,started_at,ended_at,next_attempt_at,
		usage_json,report_artifact_id
		FROM agent_delegation_attempts WHERE parent_run_id=? AND delegation_id=? AND task_index=?`
	args := []any{parentRunID, delegationID, taskIndex}
	if attemptNo > 0 {
		query += " AND attempt_no=?"
		args = append(args, attemptNo)
	} else {
		query += " ORDER BY attempt_no DESC LIMIT 1"
	}
	row := rs.db.QueryRowContext(ctx, query, args...)
	return scanDelegationAttempt(row)
}

func scanDelegationAttempt(row rowScanner) (DelegationAttemptRecord, error) {
	var (
		record      DelegationAttemptRecord
		startedAt   sql.NullTime
		endedAt     sql.NullTime
		nextAttempt sql.NullTime
		usageRaw    []byte
		reportID    sql.NullString
	)
	if err := row.Scan(
		&record.ParentRunID, &record.DelegationID, &record.TaskIndex, &record.AttemptNo,
		&record.AttemptID, &record.ChildRunID, &record.Status, &record.Retryable,
		&record.ErrorCode, &record.ErrorMessage, &startedAt, &endedAt, &nextAttempt,
		&usageRaw, &reportID,
	); err != nil {
		return record, err
	}
	record.StartedAt = store.FormatDatabaseTime(startedAt)
	record.EndedAt = store.FormatDatabaseTime(endedAt)
	record.NextAttemptAt = store.FormatDatabaseTime(nextAttempt)
	if reportID.Valid {
		record.ReportArtifactID = reportID.String
	}
	if len(usageRaw) > 0 {
		var usage agentapi.Usage
		if err := json.Unmarshal(usageRaw, &usage); err != nil {
			return record, fmt.Errorf("decode delegation attempt usage: %w", err)
		}
		record.Usage = &usage
	}
	return record, nil
}

func validateAttemptStart(start DelegationAttemptStart) error {
	if start.ParentRunID == "" || start.DelegationID == "" || start.TaskIndex < 0 ||
		start.AttemptNo <= 0 || start.AttemptID == "" || start.ChildRunID == "" || start.StartedAt == "" {
		return fmt.Errorf("invalid delegation attempt start")
	}
	return nil
}

func validateAttemptFinish(finish DelegationAttemptFinish) error {
	if finish.ParentRunID == "" || finish.DelegationID == "" || finish.TaskIndex < 0 ||
		finish.AttemptNo <= 0 || finish.AttemptID == "" || finish.Status == DelegationAttemptRunning ||
		finish.EndedAt == "" {
		return fmt.Errorf("invalid delegation attempt finish")
	}
	return nil
}

func marshalOptionalUsage(usage *agentapi.Usage) ([]byte, error) {
	if usage == nil {
		return nil, nil
	}
	return json.Marshal(usage)
}

func sameAttemptFinish(record DelegationAttemptRecord, finish DelegationAttemptFinish) bool {
	if record.AttemptID != finish.AttemptID || record.Status != finish.Status ||
		record.Retryable != finish.Retryable || record.ErrorCode != finish.ErrorCode ||
		record.ErrorMessage != finish.ErrorMessage || record.ReportArtifactID != finish.ReportArtifactID {
		return false
	}
	if (record.Usage == nil) != (finish.Usage == nil) {
		return false
	}
	return record.Usage == nil || *record.Usage == *finish.Usage
}
