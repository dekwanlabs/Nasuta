package run

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// UpsertDelegationCheckpoint persists the parent-side handoff boundary. The
// operation is idempotent for one logical task and intentionally does not
// overwrite a completed checkpoint with a later pending update.
func (rs *Store) UpsertDelegationCheckpoint(
	ctx context.Context,
	checkpoint DelegationCheckpoint,
) error {
	if checkpoint.ParentRunID == "" || checkpoint.DelegationID == "" || checkpoint.TaskIndex < 0 || checkpoint.Status == "" {
		return fmt.Errorf("invalid delegation checkpoint")
	}
	// Checkpoint timestamps are required by the schema. Complete missing values
	// at the storage boundary so recovery callers cannot write NULL accidentally.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if checkpoint.CreatedAt == "" {
		checkpoint.CreatedAt = now
	}
	if checkpoint.UpdatedAt == "" {
		checkpoint.UpdatedAt = now
	}
	_, err := rs.db.ExecContext(ctx, `
		INSERT INTO agent_delegation_checkpoints(
			parent_run_id,delegation_id,task_index,invocation_id,request_hash,status,
			child_run_id,report_artifact_id,error_code,error_message,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			invocation_id=VALUES(invocation_id),request_hash=VALUES(request_hash),
			status=CASE WHEN status='completed' AND VALUES(status)='pending' THEN status ELSE VALUES(status) END,
			child_run_id=VALUES(child_run_id),report_artifact_id=VALUES(report_artifact_id),
			error_code=VALUES(error_code),error_message=VALUES(error_message),updated_at=VALUES(updated_at)`,
		checkpoint.ParentRunID, checkpoint.DelegationID, checkpoint.TaskIndex,
		checkpoint.InvocationID, checkpoint.RequestHash, checkpoint.Status,
		checkpoint.ChildRunID, checkpoint.ReportArtifactID, checkpoint.ErrorCode,
		checkpoint.ErrorMessage, store.DatabaseTime(checkpoint.CreatedAt), store.DatabaseTime(checkpoint.UpdatedAt),
	)
	return err
}

// GetDelegationCheckpoint loads one parent recovery boundary.
func (rs *Store) GetDelegationCheckpoint(
	ctx context.Context,
	parentRunID, delegationID string,
	taskIndex int,
) (DelegationCheckpoint, error) {
	row := rs.db.QueryRowContext(ctx, `
		SELECT parent_run_id,delegation_id,task_index,invocation_id,request_hash,status,
			child_run_id,report_artifact_id,error_code,error_message,created_at,updated_at
		FROM agent_delegation_checkpoints
		WHERE parent_run_id=? AND delegation_id=? AND task_index=?`,
		parentRunID, delegationID, taskIndex,
	)
	var (
		checkpoint           DelegationCheckpoint
		createdAt, updatedAt sql.NullTime
	)
	if err := row.Scan(
		&checkpoint.ParentRunID, &checkpoint.DelegationID, &checkpoint.TaskIndex,
		&checkpoint.InvocationID, &checkpoint.RequestHash, &checkpoint.Status,
		&checkpoint.ChildRunID, &checkpoint.ReportArtifactID, &checkpoint.ErrorCode,
		&checkpoint.ErrorMessage, &createdAt, &updatedAt,
	); err != nil {
		return DelegationCheckpoint{}, err
	}
	checkpoint.CreatedAt = store.FormatDatabaseTime(createdAt)
	checkpoint.UpdatedAt = store.FormatDatabaseTime(updatedAt)
	return checkpoint, nil
}
