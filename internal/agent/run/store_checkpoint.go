package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

// LogicalCheckpoint is the replay boundary for a parent logical loop. State is
// intentionally opaque to the run store so the execution package can evolve
// its continuation schema without changing the persistence contract.
type LogicalCheckpoint struct {
	RunID      string
	StepNo     int
	Phase      string
	InputHash  string
	PromptHash string
	State      json.RawMessage
	LeaseOwner string
	LeaseFence int64
	CreatedAt  string
	UpdatedAt  string
}

func (rs *Store) SaveLogicalCheckpoint(ctx context.Context, checkpoint LogicalCheckpoint) error {
	if rs == nil || rs.db == nil {
		return fmt.Errorf("agent/runstore: database is required")
	}
	if checkpoint.RunID == "" || checkpoint.Phase == "" || checkpoint.LeaseOwner == "" || checkpoint.LeaseFence <= 0 || len(checkpoint.State) == 0 {
		return fmt.Errorf("invalid logical checkpoint")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if checkpoint.CreatedAt == "" {
		checkpoint.CreatedAt = now
	}
	if checkpoint.UpdatedAt == "" {
		checkpoint.UpdatedAt = now
	}
	var valid int
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_run_budget_ledger WHERE root_run_id=? AND lease_owner=? AND lease_fence=? AND lease_expires_at>?`, checkpoint.RunID, checkpoint.LeaseOwner, checkpoint.LeaseFence, store.DatabaseTime(now)).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return fmt.Errorf("logical checkpoint lease is not active")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_run_checkpoints(run_id,step_no,phase,input_hash,prompt_hash,state_json,lease_fence,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE step_no=VALUES(step_no),phase=VALUES(phase),input_hash=VALUES(input_hash),prompt_hash=VALUES(prompt_hash),state_json=VALUES(state_json),lease_fence=VALUES(lease_fence),updated_at=VALUES(updated_at)`, checkpoint.RunID, checkpoint.StepNo, checkpoint.Phase, checkpoint.InputHash, checkpoint.PromptHash, checkpoint.State, checkpoint.LeaseFence, store.DatabaseTime(checkpoint.CreatedAt), store.DatabaseTime(checkpoint.UpdatedAt))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (rs *Store) GetLogicalCheckpoint(ctx context.Context, runID string) (LogicalCheckpoint, error) {
	if rs == nil || rs.db == nil {
		return LogicalCheckpoint{}, fmt.Errorf("agent/runstore: database is required")
	}
	var c LogicalCheckpoint
	var created, updated sql.NullTime
	err := rs.db.QueryRowContext(ctx, `SELECT run_id,step_no,phase,input_hash,prompt_hash,state_json,lease_fence,created_at,updated_at FROM agent_run_checkpoints WHERE run_id=?`, runID).Scan(&c.RunID, &c.StepNo, &c.Phase, &c.InputHash, &c.PromptHash, &c.State, &c.LeaseFence, &created, &updated)
	if err != nil {
		return LogicalCheckpoint{}, err
	}
	c.CreatedAt = store.FormatDatabaseTime(created)
	c.UpdatedAt = store.FormatDatabaseTime(updated)
	return c, nil
}
