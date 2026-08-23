package investigation

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MySQLLeaseStore fences runs across coordinator processes using a shared
// platform-owned table. A takeover increments fencing_token so late writes fail.
type MySQLLeaseStore struct {
	db *sql.DB
}

// NewMySQLLeaseStore binds the lease store to an existing MySQL pool.
func NewMySQLLeaseStore(db *sql.DB) (*MySQLLeaseStore, error) {
	if db == nil {
		return nil, fmt.Errorf("mysql lease store: database is required")
	}
	return &MySQLLeaseStore{db: db}, nil
}

func (store *MySQLLeaseStore) AcquireLease(
	ctx context.Context,
	runID string,
	owner string,
	ttl time.Duration,
) error {
	_, err := store.AcquireLeaseWithToken(ctx, runID, owner, ttl)
	return err
}

func (store *MySQLLeaseStore) AcquireLeaseWithToken(
	ctx context.Context,
	runID string,
	owner string,
	ttl time.Duration,
) (Lease, error) {
	if err := validateLeaseInput(store, runID, owner, ttl); err != nil {
		return Lease{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, fmt.Errorf("begin lease transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	var existingOwner string
	var expiresAt int64
	var currentToken uint64
	err = tx.QueryRowContext(
		ctx,
		`SELECT owner, expires_at, fencing_token FROM investigation_leases WHERE run_id = ? FOR UPDATE`,
		runID,
	).Scan(&existingOwner, &expiresAt, &currentToken)
	if err != nil && err != sql.ErrNoRows {
		return Lease{}, fmt.Errorf("load run %q lease: %w", runID, err)
	}
	exists := err == nil
	expires := time.UnixMilli(expiresAt).UTC()
	if exists && existingOwner != owner && expires.After(now) {
		return Lease{}, fmt.Errorf("run %q is already leased", runID)
	}
	token := currentToken
	if !exists || !expires.After(now) {
		token++
		if token == 0 {
			token = 1
		}
	}
	nextExpires := now.Add(ttl)
	if exists {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE investigation_leases SET owner = ?, expires_at = ?, fencing_token = ? WHERE run_id = ?`,
			owner, nextExpires.UnixMilli(), token, runID,
		); err != nil {
			return Lease{}, fmt.Errorf("renew run %q lease: %w", runID, err)
		}
	} else {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO investigation_leases (run_id, owner, expires_at, fencing_token) VALUES (?, ?, ?, ?)`,
			runID, owner, nextExpires.UnixMilli(), token,
		); err != nil {
			return Lease{}, fmt.Errorf("insert run %q lease: %w", runID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, fmt.Errorf("commit run %q lease: %w", runID, err)
	}
	return Lease{RunID: runID, Owner: owner, Token: token, ExpiresAt: nextExpires}, nil
}

func (store *MySQLLeaseStore) RenewLease(
	ctx context.Context,
	runID string,
	owner string,
	ttl time.Duration,
) error {
	if err := validateLeaseInput(store, runID, owner, ttl); err != nil {
		return err
	}
	var token uint64
	if err := store.db.QueryRowContext(
		ctx,
		`SELECT fencing_token FROM investigation_leases WHERE run_id = ? AND owner = ?`,
		runID, owner,
	).Scan(&token); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("run %q lease is not owned by %q", runID, owner)
		}
		return fmt.Errorf("load run %q lease token: %w", runID, err)
	}
	return store.RenewLeaseWithToken(ctx, runID, owner, token, ttl)
}

func (store *MySQLLeaseStore) RenewLeaseWithToken(
	ctx context.Context,
	runID string,
	owner string,
	token uint64,
	ttl time.Duration,
) error {
	if err := validateLeaseInput(store, runID, owner, ttl); err != nil {
		return err
	}
	if token == 0 {
		return fmt.Errorf("lease fencing token must be positive")
	}
	result, err := store.db.ExecContext(
		ctx,
		`UPDATE investigation_leases SET expires_at = ? WHERE run_id = ? AND owner = ? AND fencing_token = ? AND expires_at > ?`,
		time.Now().UTC().Add(ttl).UnixMilli(), runID, owner, token, time.Now().UTC().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("renew run %q lease: %w", runID, err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("check renewed run %q lease: %w", runID, err)
	} else if affected != 1 {
		return fmt.Errorf("run %q lease is not owned by %q", runID, owner)
	}
	return nil
}

func (store *MySQLLeaseStore) ReleaseLease(
	ctx context.Context,
	runID string,
	owner string,
) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("mysql lease store: database is required")
	}
	if runID == "" || owner == "" {
		return fmt.Errorf("mysql lease store: run id and owner are required")
	}
	_, err := store.db.ExecContext(
		ctx,
		`UPDATE investigation_leases SET owner = '', expires_at = 0 WHERE run_id = ? AND owner = ?`,
		runID, owner,
	)
	if err != nil {
		return fmt.Errorf("release run %q lease: %w", runID, err)
	}
	return nil
}

func (store *MySQLLeaseStore) ValidateLease(
	ctx context.Context,
	runID string,
	owner string,
) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("mysql lease store: database is required")
	}
	var expiresAt int64
	if err := store.db.QueryRowContext(
		ctx,
		`SELECT expires_at FROM investigation_leases WHERE run_id = ? AND owner = ?`,
		runID, owner,
	).Scan(&expiresAt); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("run %q lease is not owned by %q", runID, owner)
		}
		return fmt.Errorf("validate run %q lease: %w", runID, err)
	}
	if !time.UnixMilli(expiresAt).After(time.Now().UTC()) {
		return fmt.Errorf("run %q lease owned by %q has expired", runID, owner)
	}
	return nil
}

func (store *MySQLLeaseStore) ValidateLeaseWithToken(
	ctx context.Context,
	runID string,
	owner string,
	token uint64,
) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("mysql lease store: database is required")
	}
	if token == 0 {
		return fmt.Errorf("lease fencing token must be positive")
	}
	var expiresAt int64
	if err := store.db.QueryRowContext(
		ctx,
		`SELECT expires_at FROM investigation_leases WHERE run_id = ? AND owner = ? AND fencing_token = ?`,
		runID, owner, token,
	).Scan(&expiresAt); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("run %q lease is not owned by %q", runID, owner)
		}
		return fmt.Errorf("validate run %q lease: %w", runID, err)
	}
	if !time.UnixMilli(expiresAt).After(time.Now().UTC()) {
		return fmt.Errorf("run %q lease owned by %q has expired", runID, owner)
	}
	return nil
}

func validateLeaseInput(store *MySQLLeaseStore, runID, owner string, ttl time.Duration) error {
	if store == nil || store.db == nil {
		return fmt.Errorf("mysql lease store: database is required")
	}
	if runID == "" || owner == "" || ttl <= 0 {
		return fmt.Errorf("mysql lease store: run id, owner, and positive ttl are required")
	}
	return nil
}
