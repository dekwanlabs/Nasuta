package run

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

const (
	WorkReady     = "ready"
	WorkRunning   = "running"
	WorkSucceeded = "succeeded"
	WorkFailed    = "failed"
)

type WorkItem struct {
	WorkID         string
	RunID          string
	ParentRunID    string
	DelegationID   string
	TaskIndex      int
	AttemptNo      int
	Kind           string
	Payload        json.RawMessage
	State          string
	LeaseOwner     string
	LeaseFence     int64
	LeaseExpiresAt string
	AvailableAt    string
	AttemptCount   int
	LastError      string
}

func (rs *Store) EnqueueWorkItem(ctx context.Context, item WorkItem) error {
	if rs == nil || rs.db == nil {
		return fmt.Errorf("agent/runstore: database is required")
	}
	if item.WorkID == "" || item.RunID == "" || item.Kind == "" || len(item.Payload) == 0 {
		return fmt.Errorf("invalid work item")
	}
	if item.AttemptNo <= 0 {
		item.AttemptNo = 1
	}
	if item.State == "" {
		item.State = WorkReady
	}
	now := time.Now().UTC()
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// WorkID and the logical delegation tuple are immutable identity. A
	// redelivery may safely call Enqueue again, but it must never silently
	// replace the payload or kind of an existing queue item: doing so could
	// execute a different child under a previously admitted reservation. JSON
	// equality is semantic because MySQL normalizes JSON representation.
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_work_items(work_id,run_id,parent_run_id,delegation_id,task_index,attempt_no,kind,payload_json,state,available_at,attempt_count,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE work_id=work_id`, item.WorkID, item.RunID, item.ParentRunID, item.DelegationID, item.TaskIndex, item.AttemptNo, item.Kind, item.Payload, item.State, store.DatabaseTime(now.Format(time.RFC3339Nano)), item.AttemptCount, item.LastError, store.DatabaseTime(now.Format(time.RFC3339Nano)), store.DatabaseTime(now.Format(time.RFC3339Nano)))
	if err != nil {
		return err
	}
	var existing WorkItem
	var payload []byte
	if err := tx.QueryRowContext(ctx, `SELECT run_id,parent_run_id,delegation_id,task_index,attempt_no,kind,payload_json FROM agent_work_items WHERE work_id=? FOR UPDATE`, item.WorkID).Scan(&existing.RunID, &existing.ParentRunID, &existing.DelegationID, &existing.TaskIndex, &existing.AttemptNo, &existing.Kind, &payload); err != nil {
		return err
	}
	if existing.RunID != item.RunID || existing.ParentRunID != item.ParentRunID ||
		existing.DelegationID != item.DelegationID || existing.TaskIndex != item.TaskIndex ||
		existing.AttemptNo != item.AttemptNo || existing.Kind != item.Kind ||
		!equalJSONPayload(payload, item.Payload) {
		return fmt.Errorf("%w: work_id=%q", ErrWorkItemConflict, item.WorkID)
	}
	return tx.Commit()
}

func equalJSONPayload(left, right []byte) bool {
	if bytes.Equal(left, right) {
		return true
	}
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		return value, nil
	}
	leftValue, leftErr := decode(left)
	rightValue, rightErr := decode(right)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftValue, rightValue)
}

// ClaimWorkItem is the worker-lease boundary. The row lock and claim update
// share one transaction, so two instances cannot claim the same attempt.
func (rs *Store) ClaimWorkItem(ctx context.Context, owner string, now time.Time, ttl time.Duration) (WorkItem, error) {
	return rs.claimWorkItem(ctx, owner, now, ttl, "", "")
}

// ClaimWorkItemByKind restricts a worker to one durable work class. This keeps
// parent recovery jobs out of child workers and makes queue ownership explicit.
func (rs *Store) ClaimWorkItemByKind(ctx context.Context, kind, owner string, now time.Time, ttl time.Duration) (WorkItem, error) {
	if kind == "" {
		return WorkItem{}, fmt.Errorf("work kind is required")
	}
	return rs.claimWorkItem(ctx, owner, now, ttl, "", kind)
}

// ClaimWorkItemByID claims exactly one logical work item. It is used by a
// dispatcher that has already selected a parent task; unlike the generic
// claim, it cannot accidentally steal work belonging to another delegation.
func (rs *Store) ClaimWorkItemByID(ctx context.Context, workID, owner string, now time.Time, ttl time.Duration) (WorkItem, error) {
	if workID == "" {
		return WorkItem{}, fmt.Errorf("work id is required")
	}
	return rs.claimWorkItem(ctx, owner, now, ttl, workID, "")
}

func (rs *Store) claimWorkItem(ctx context.Context, owner string, now time.Time, ttl time.Duration, workID, kind string) (WorkItem, error) {
	if rs == nil || rs.db == nil {
		return WorkItem{}, fmt.Errorf("agent/runstore: database is required")
	}
	if owner == "" || ttl <= 0 {
		return WorkItem{}, fmt.Errorf("invalid worker lease")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItem{}, err
	}
	defer tx.Rollback()
	var item WorkItem
	var payload []byte
	var expires, available sql.NullTime
	query := `SELECT work_id,run_id,parent_run_id,delegation_id,task_index,attempt_no,kind,payload_json,state,lease_owner,lease_fence,lease_expires_at,available_at,attempt_count,last_error FROM agent_work_items WHERE ((state=? AND available_at<=?) OR (state=? AND lease_expires_at IS NOT NULL AND lease_expires_at<=?))`
	args := []any{WorkReady, store.DatabaseTime(now.Format(time.RFC3339Nano)), WorkRunning, store.DatabaseTime(now.Format(time.RFC3339Nano))}
	if workID != "" {
		query += ` AND work_id=?`
		args = append(args, workID)
	}
	if kind != "" {
		query += ` AND kind=?`
		args = append(args, kind)
	}
	query += ` ORDER BY available_at,created_at LIMIT 1 FOR UPDATE`
	err = tx.QueryRowContext(ctx, query, args...).Scan(&item.WorkID, &item.RunID, &item.ParentRunID, &item.DelegationID, &item.TaskIndex, &item.AttemptNo, &item.Kind, &payload, &item.State, &item.LeaseOwner, &item.LeaseFence, &expires, &available, &item.AttemptCount, &item.LastError)
	if err != nil {
		return WorkItem{}, err
	}
	item.Payload = payload
	item.LeaseFence++
	item.LeaseOwner = owner
	item.LeaseExpiresAt = now.Add(ttl).Format(time.RFC3339Nano)
	item.State = WorkRunning
	item.AttemptCount++
	update := `UPDATE agent_work_items SET state=?,lease_owner=?,lease_fence=?,lease_expires_at=?,attempt_count=?,updated_at=? WHERE work_id=? AND state IN (?,?) AND (state=? OR lease_expires_at<=?)`
	claimed, err := tx.ExecContext(ctx, update, WorkRunning, owner, item.LeaseFence, store.DatabaseTime(item.LeaseExpiresAt), item.AttemptCount, store.DatabaseTime(now.Format(time.RFC3339Nano)), item.WorkID, WorkReady, WorkRunning, WorkReady, store.DatabaseTime(now.Format(time.RFC3339Nano)))
	if err != nil {
		return WorkItem{}, err
	}
	if affected, err := claimed.RowsAffected(); err != nil {
		return WorkItem{}, err
	} else if affected != 1 {
		return WorkItem{}, fmt.Errorf("worker lease claim lost")
	}
	if err = tx.Commit(); err != nil {
		return WorkItem{}, err
	}
	return item, nil
}

func (rs *Store) RenewWorkItem(ctx context.Context, workID, owner string, fence int64, now time.Time, ttl time.Duration) error {
	if workID == "" || owner == "" || fence <= 0 || ttl <= 0 {
		return fmt.Errorf("invalid worker lease renewal")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := rs.db.ExecContext(ctx, `UPDATE agent_work_items SET lease_expires_at=?,updated_at=? WHERE work_id=? AND state=? AND lease_owner=? AND lease_fence=? AND lease_expires_at>?`, store.DatabaseTime(now.Add(ttl).Format(time.RFC3339Nano)), store.DatabaseTime(now.Format(time.RFC3339Nano)), workID, WorkRunning, owner, fence, store.DatabaseTime(now.Format(time.RFC3339Nano)))
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return fmt.Errorf("worker lease lost")
	}
	return nil
}

func (rs *Store) CompleteWorkItem(ctx context.Context, workID, owner string, fence int64, state, lastError string) error {
	if state != WorkSucceeded && state != WorkFailed {
		return fmt.Errorf("invalid terminal work state")
	}
	result, err := rs.db.ExecContext(ctx, `UPDATE agent_work_items SET state=?,lease_owner='',lease_expires_at=NULL,last_error=?,updated_at=? WHERE work_id=? AND state=? AND lease_owner=? AND lease_fence=?`, state, lastError, store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)), workID, WorkRunning, owner, fence)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return fmt.Errorf("worker lease lost")
	}
	return nil
}

func (rs *Store) RequeueExpiredWork(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := rs.db.ExecContext(ctx, `UPDATE agent_work_items SET state=?,lease_owner='',lease_expires_at=NULL,available_at=?,last_error=?,updated_at=? WHERE state=? AND lease_expires_at IS NOT NULL AND lease_expires_at<=?`, WorkReady, store.DatabaseTime(now.Format(time.RFC3339Nano)), "worker lease expired", store.DatabaseTime(now.Format(time.RFC3339Nano)), WorkRunning, store.DatabaseTime(now.Format(time.RFC3339Nano)))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
