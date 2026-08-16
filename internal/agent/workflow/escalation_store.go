package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	platformstore "github.com/dekwanlabs/nasuta/internal/platform/store"
)

// EscalationStore persists idempotency receipts independently from Workflow runs.
type EscalationStore struct {
	db *sql.DB
}

func NewEscalationStore(db *sql.DB) (*EscalationStore, error) {
	if db == nil {
		return nil, fmt.Errorf("workflow escalation store database is required")
	}
	return &EscalationStore{db: db}, nil
}

func (store *EscalationStore) LoadWorkflowEscalation(
	ctx context.Context,
	parentRunID,
	requestID string,
) (WorkflowEscalationRecord, error) {
	if store == nil || store.db == nil {
		return WorkflowEscalationRecord{}, ErrUnavailable
	}
	record, err := scanWorkflowEscalation(store.db.QueryRowContext(
		ctx,
		`SELECT parent_run_id,request_id,request_hash,workflow_run_id,
			binding_id,binding_version,status,error_code
		 FROM workflow_escalations
		 WHERE parent_run_id=? AND request_id=?`,
		parentRunID,
		requestID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return WorkflowEscalationRecord{}, ErrNotFound
	}
	return record, err
}

func (store *EscalationStore) ReserveWorkflowEscalation(
	ctx context.Context,
	record WorkflowEscalationRecord,
) (WorkflowEscalationRecord, bool, error) {
	if store == nil || store.db == nil {
		return WorkflowEscalationRecord{}, false, ErrUnavailable
	}
	if err := validateEscalationRecord(record, escalationReceiptStarting); err != nil {
		return WorkflowEscalationRecord{}, false, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkflowEscalationRecord{}, false, err
	}
	defer tx.Rollback()

	existing, err := scanWorkflowEscalation(tx.QueryRowContext(
		ctx,
		`SELECT parent_run_id,request_id,request_hash,workflow_run_id,
			binding_id,binding_version,status,error_code
		 FROM workflow_escalations
		 WHERE parent_run_id=? AND request_id=? FOR UPDATE`,
		record.ParentRunID,
		record.RequestID,
	))
	if err == nil {
		if err := tx.Commit(); err != nil {
			return WorkflowEscalationRecord{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return WorkflowEscalationRecord{}, false, err
	}
	now := platformstore.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano))
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO workflow_escalations(
			parent_run_id,request_id,request_hash,workflow_run_id,
			binding_id,binding_version,status,error_code,created_at,updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		record.ParentRunID,
		record.RequestID,
		record.RequestHash,
		record.WorkflowRunID,
		record.BindingID,
		record.BindingVersion,
		record.Status,
		record.ErrorCode,
		now,
		now,
	); err != nil {
		return WorkflowEscalationRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return WorkflowEscalationRecord{}, false, err
	}
	return record, true, nil
}

func (store *EscalationStore) FinishWorkflowEscalation(
	ctx context.Context,
	record WorkflowEscalationRecord,
) error {
	if store == nil || store.db == nil {
		return ErrUnavailable
	}
	if record.Status != agentapi.EscalationAccepted &&
		record.Status != agentapi.EscalationRejected {
		return fmt.Errorf("workflow escalation terminal status %q is invalid", record.Status)
	}
	if err := validateEscalationRecord(record, record.Status); err != nil {
		return err
	}
	result, err := store.db.ExecContext(
		ctx,
		`UPDATE workflow_escalations
		 SET status=?,error_code=?,updated_at=?
		 WHERE parent_run_id=? AND request_id=? AND request_hash=? AND
			(status=? OR (status=? AND error_code=?))`,
		record.Status,
		record.ErrorCode,
		platformstore.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
		record.ParentRunID,
		record.RequestID,
		record.RequestHash,
		escalationReceiptStarting,
		record.Status,
		record.ErrorCode,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrConflict
	}
	return nil
}

func scanWorkflowEscalation(scanner rowScanner) (WorkflowEscalationRecord, error) {
	var record WorkflowEscalationRecord
	err := scanner.Scan(
		&record.ParentRunID,
		&record.RequestID,
		&record.RequestHash,
		&record.WorkflowRunID,
		&record.BindingID,
		&record.BindingVersion,
		&record.Status,
		&record.ErrorCode,
	)
	return record, err
}

func validateEscalationRecord(
	record WorkflowEscalationRecord,
	status agentapi.WorkflowEscalationStatus,
) error {
	if record.ParentRunID == "" || record.RequestID == "" ||
		!validContentHash(record.RequestHash) ||
		record.WorkflowRunID == "" ||
		record.Status != status {
		return fmt.Errorf("workflow escalation receipt is invalid")
	}
	if (record.BindingID == "") != (record.BindingVersion == 0) ||
		record.BindingVersion < 0 {
		return fmt.Errorf("workflow escalation binding identity is invalid")
	}
	if record.Status == agentapi.EscalationAccepted &&
		(record.BindingID == "" || record.BindingVersion <= 0 || record.ErrorCode != "") {
		return fmt.Errorf("accepted workflow escalation receipt is invalid")
	}
	if record.Status == agentapi.EscalationRejected && record.ErrorCode == "" {
		return fmt.Errorf("rejected workflow escalation error code is required")
	}
	return nil
}
