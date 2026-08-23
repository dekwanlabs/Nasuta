package investigation

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// MySQLRunStore keeps one JSON snapshot per run plus an append-only event log in
// the platform-owned MySQL pool. It reuses the same mutation functions as the
// in-memory and SQLite stores, so lifecycle invariants are enforced once.
type MySQLRunStore struct {
	mu sync.Mutex
	db *sql.DB
}

// NewMySQLRunStore binds the investigation  run store to an existing MySQL
// pool. Tables are created by the platform dbschema migration, not here.
func NewMySQLRunStore(db *sql.DB) (*MySQLRunStore, error) {
	if db == nil {
		return nil, fmt.Errorf("mysql run store: database is required")
	}
	return &MySQLRunStore{db: db}, nil
}

func (store *MySQLRunStore) Create(run InvestigationRun) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("run store is required")
	}
	next, event, err := mutateCreate(run)
	if err != nil {
		return err
	}
	return store.persist(next, event, true)
}

func (store *MySQLRunStore) Get(id string) (InvestigationRun, error) {
	if store == nil || store.db == nil {
		return InvestigationRun{}, fmt.Errorf("run store is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	payload, err := store.loadPayload(id)
	if err != nil {
		return InvestigationRun{}, err
	}
	return cloneRun(payload), nil
}

func (store *MySQLRunStore) Transition(id string, next RunStatus) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateTransition(run, next)
	})
}

func (store *MySQLRunStore) SavePlan(id string, plan PlanRevision) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSavePlan(run, plan)
	})
}

func (store *MySQLRunStore) SaveBudget(id string, budget BudgetSnapshot) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveBudget(run, budget)
	})
}

func (store *MySQLRunStore) SaveTask(id string, task ExecutableTask) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveTask(run, task)
	})
}

func (store *MySQLRunStore) SaveResult(id string, record TaskExecutionRecord) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveResult(run, record)
	})
}

func (store *MySQLRunStore) SaveEvidence(id string, units []EvidenceUnit) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveEvidence(run, units)
	})
}

func (store *MySQLRunStore) SaveClaims(id string, claims []VerifiedClaim) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveClaims(run, claims)
	})
}

func (store *MySQLRunStore) SaveReport(id string, report InvestigationReport) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveReport(run, report)
	})
}

func (store *MySQLRunStore) SaveMetrics(id string, metrics RunMetrics) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveMetrics(run, metrics)
	})
}

func (store *MySQLRunStore) SaveDelivery(id string, delivery DeliveryResult) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateSaveDelivery(run, delivery)
	})
}

func (store *MySQLRunStore) Fail(id string, failure RunFailure, status RunStatus) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateFail(run, failure, status)
	})
}

func (store *MySQLRunStore) AppendEvent(id, eventType, message string) error {
	return store.apply(id, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateAppendEvent(run, eventType, message)
	})
}

func (store *MySQLRunStore) Events(id string) ([]RunEvent, error) {
	if store == nil || store.db == nil {
		return nil, fmt.Errorf("run store is required")
	}
	rows, err := store.db.Query(
		`SELECT sequence, run_id, type, status, message, created_at FROM investigation_events WHERE run_id = ? ORDER BY sequence`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]RunEvent, 0)
	for rows.Next() {
		var event RunEvent
		var createdAt int64
		if err := rows.Scan(&event.Sequence, &event.RunID, &event.Type, &event.Status, &event.Message, &createdAt); err != nil {
			return nil, err
		}
		event.CreatedAt = time.UnixMilli(createdAt).UTC()
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(events) == 0 {
		if _, err := store.loadPayload(id); err != nil {
			return nil, err
		}
	}
	return events, nil
}

func (store *MySQLRunStore) apply(id string, mutate runMutation) error {
	return store.applyWithToken(id, 0, mutate)
}

func (store *MySQLRunStore) applyFenced(id, owner string, token uint64, mutate runMutation) error {
	if owner == "" || token == 0 {
		return fmt.Errorf("%w: lease owner and fencing token are required", ErrLeaseFenced)
	}
	return store.applyWithFence(id, owner, token, mutate)
}

func (store *MySQLRunStore) applyWithToken(id string, token uint64, mutate runMutation) error {
	return store.applyWithFence(id, "", token, mutate)
}

func (store *MySQLRunStore) applyWithFence(id, owner string, token uint64, mutate runMutation) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("run store is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.applyLocked(id, owner, token, mutate)
}

func (store *MySQLRunStore) applyLocked(id, owner string, token uint64, mutate runMutation) error {
	query := `SELECT payload FROM investigation_runs WHERE id = ?`
	if token > 0 {
		query = `SELECT payload FROM investigation_runs WHERE id = ? AND fencing_token = ?`
	}
	args := []any{id}
	if token > 0 {
		args = append(args, token)
	}
	var payload string
	if err := store.db.QueryRow(query, args...).Scan(&payload); err != nil {
		if token > 0 && err == sql.ErrNoRows {
			return fmt.Errorf("%w: run %q fencing token %d is stale", ErrLeaseFenced, id, token)
		}
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return err
	}
	var run InvestigationRun
	if err := json.Unmarshal([]byte(payload), &run); err != nil {
		return fmt.Errorf("decode run %q: %w", id, err)
	}
	run = normalizeStoredRun(run)
	next, event, err := mutate(run)
	if err != nil {
		return err
	}
	return store.persistLocked(next, event, false, runFence{owner: owner, token: token})
}

func (store *MySQLRunStore) loadPayload(id string) (InvestigationRun, error) {
	var payload string
	if err := store.db.QueryRow(`SELECT payload FROM investigation_runs WHERE id = ?`, id).Scan(&payload); err != nil {
		if err == sql.ErrNoRows {
			return InvestigationRun{}, fmt.Errorf("%w: %q", ErrNotFound, id)
		}
		return InvestigationRun{}, err
	}
	var run InvestigationRun
	if err := json.Unmarshal([]byte(payload), &run); err != nil {
		return InvestigationRun{}, fmt.Errorf("decode run %q: %w", id, err)
	}
	return normalizeStoredRun(run), nil
}

func (store *MySQLRunStore) persist(next InvestigationRun, event RunEvent, create bool) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("run store is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.persistLocked(next, event, create)
}

func (store *MySQLRunStore) createFenced(run InvestigationRun, owner string, token uint64) error {
	if owner == "" || token == 0 {
		return fmt.Errorf("%w: lease owner and fencing token are required", ErrLeaseFenced)
	}
	next, event, err := mutateCreate(run)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.persistLocked(next, event, true, runFence{owner: owner, token: token})
}

type runFence struct {
	owner string
	token uint64
}

func (store *MySQLRunStore) persistLocked(next InvestigationRun, event RunEvent, create bool, fences ...runFence) error {
	payload, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode run %q: %w", next.ID, err)
	}
	var fence runFence
	if len(fences) > 0 {
		fence = fences[0]
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if fence.token > 0 {
		var leaseToken uint64
		var expiresAt int64
		if err := tx.QueryRow(`SELECT fencing_token, expires_at FROM investigation_leases WHERE run_id = ? AND owner = ? FOR UPDATE`, next.ID, fence.owner).Scan(&leaseToken, &expiresAt); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("%w: run %q lease is not owned by %q", ErrLeaseFenced, next.ID, fence.owner)
			}
			return fmt.Errorf("validate run %q lease: %w", next.ID, err)
		}
		if leaseToken != fence.token || !time.UnixMilli(expiresAt).After(time.Now().UTC()) {
			return fmt.Errorf("%w: run %q fencing token %d is stale", ErrLeaseFenced, next.ID, fence.token)
		}
	}
	if create {
		if fence.token > 0 {
			if _, err := tx.Exec(`INSERT INTO investigation_runs (id, payload, updated_at, fencing_token) VALUES (?, ?, ?, ?)`, next.ID, string(payload), next.UpdatedAt.UnixMilli(), fence.token); err != nil {
				return fmt.Errorf("insert run %q: %w", next.ID, err)
			}
		} else if _, err := tx.Exec(`INSERT INTO investigation_runs (id, payload, updated_at) VALUES (?, ?, ?)`, next.ID, string(payload), next.UpdatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("insert run %q: %w", next.ID, err)
		}
	} else {
		var result sql.Result
		if fence.token > 0 {
			result, err = tx.Exec(`UPDATE investigation_runs SET payload = ?, updated_at = ? WHERE id = ? AND fencing_token = ? AND EXISTS (SELECT 1 FROM investigation_leases WHERE run_id = ? AND owner = ? AND fencing_token = ? AND expires_at > ?)`, string(payload), next.UpdatedAt.UnixMilli(), next.ID, fence.token, next.ID, fence.owner, fence.token, time.Now().UTC().UnixMilli())
		} else {
			result, err = tx.Exec(`UPDATE investigation_runs SET payload = ?, updated_at = ? WHERE id = ?`, string(payload), next.UpdatedAt.UnixMilli(), next.ID)
		}
		if err != nil {
			return fmt.Errorf("update run %q: %w", next.ID, err)
		}
		if fence.token > 0 {
			affected, affectedErr := result.RowsAffected()
			if affectedErr != nil {
				return fmt.Errorf("check fenced update for run %q: %w", next.ID, affectedErr)
			}
			if affected != 1 {
				return fmt.Errorf("%w: run %q fencing token %d is stale", ErrLeaseFenced, next.ID, fence.token)
			}
		}
	}
	if event.Type != "" {
		if _, err := tx.Exec(
			`INSERT INTO investigation_events (run_id, type, status, message, created_at) VALUES (?, ?, ?, ?, ?)`,
			next.ID, event.Type, string(event.Status), event.Message, time.Now().UTC().UnixMilli(),
		); err != nil {
			return fmt.Errorf("insert run %q event: %w", next.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}
