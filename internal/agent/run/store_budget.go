package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/budget"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

// NewDurableBudget returns a Root gate backed by the MySQL budget ledger. The
// caller must use it only for a logical root Run; delegated children inherit a
// task reservation from their parent and must not create another root.
const (
	minimumBudgetLeaseTTL = time.Minute
	budgetLeaseGrace      = 30 * time.Second
)

func (rs *Store) NewDurableBudget(rootRunID string, limits agentapi.RunLimits) (agentapi.RunBudgetGate, error) {
	return rs.NewDurableBudgetContext(context.Background(), rootRunID, limits)
}

// NewDurableBudgetContext creates a durable root and binds its heartbeat to ctx.
func (rs *Store) NewDurableBudgetContext(ctx context.Context, rootRunID string, limits agentapi.RunLimits) (agentapi.RunBudgetGate, error) {
	if rs == nil || rs.db == nil {
		return nil, fmt.Errorf("agent/runstore: database is required for durable budget")
	}
	if rs.budgetLeaseOwner == "" {
		return nil, fmt.Errorf("agent/runstore: durable budget lease owner is required")
	}
	root, err := budget.NewDurableRoot(&sqlBudgetBackend{db: rs.db, fencingEnabled: rs.fencingEnabled}, rootRunID, limits)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := root.AcquireLeaseContext(ctx, rs.budgetLeaseOwner, now, durableBudgetLeaseTTL(limits.Deadline, now)); err != nil {
		return nil, fmt.Errorf("acquire durable budget lease: %w", err)
	}
	return root, nil
}

func durableBudgetLeaseTTL(deadline, now time.Time) time.Duration {
	ttl := minimumBudgetLeaseTTL
	if !deadline.IsZero() {
		remaining := deadline.Sub(now) + budgetLeaseGrace
		if remaining > ttl {
			ttl = remaining
		}
	}
	return ttl
}

func (rs *Store) durableBudgetEnabled() bool {
	return rs != nil && rs.durableBudget
}

type sqlBudgetBackend struct {
	db             *sql.DB
	fencingEnabled bool
}

func (backend *sqlBudgetBackend) FencingEnabled() bool {
	return backend != nil && backend.fencingEnabled
}

func (backend *sqlBudgetBackend) EnsureRoot(rootRunID string, limits agentapi.RunLimits) error {
	if backend == nil || backend.db == nil {
		return fmt.Errorf("budget database is required")
	}
	if rootRunID == "" {
		return fmt.Errorf("budget root run id is required")
	}
	limitsRaw, err := json.Marshal(limits)
	if err != nil {
		return fmt.Errorf("marshal budget limits: %w", err)
	}
	zero, err := json.Marshal(agentapi.Usage{})
	if err != nil {
		return fmt.Errorf("marshal zero budget usage: %w", err)
	}
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(
		`INSERT INTO agent_run_budget_ledger(
			root_run_id,limits_json,used_usage_json,reserved_usage_json,price_version,
			lease_owner,lease_expires_at,version,created_at,updated_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE root_run_id=root_run_id`,
		rootRunID, limitsRaw, zero, zero, "", "", nil, int64(0),
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
	)
	if err != nil {
		return fmt.Errorf("insert budget root: %w", err)
	}
	var storedRaw []byte
	if err := tx.QueryRow(
		`SELECT limits_json FROM agent_run_budget_ledger WHERE root_run_id=? FOR UPDATE`,
		rootRunID,
	).Scan(&storedRaw); err != nil {
		return fmt.Errorf("read budget root limits: %w", err)
	}
	var stored agentapi.RunLimits
	if err := json.Unmarshal(storedRaw, &stored); err != nil {
		return fmt.Errorf("decode stored budget limits: %w", err)
	}
	if !sameBudgetLimits(stored, limits) {
		return fmt.Errorf("budget root %q limits conflict with existing ledger", rootRunID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (backend *sqlBudgetBackend) RootSnapshot(rootRunID string) (budget.DurableRootSnapshot, error) {
	row := backend.db.QueryRow(
		`SELECT limits_json,used_usage_json,reserved_usage_json
		 FROM agent_run_budget_ledger WHERE root_run_id=?`, rootRunID,
	)
	var limitsRaw, usedRaw, reservedRaw []byte
	if err := row.Scan(&limitsRaw, &usedRaw, &reservedRaw); err != nil {
		return budget.DurableRootSnapshot{}, err
	}
	var snapshot budget.DurableRootSnapshot
	if err := json.Unmarshal(limitsRaw, &snapshot.Limits); err != nil {
		return budget.DurableRootSnapshot{}, fmt.Errorf("decode budget limits: %w", err)
	}
	if err := json.Unmarshal(usedRaw, &snapshot.Used); err != nil {
		return budget.DurableRootSnapshot{}, fmt.Errorf("decode budget usage: %w", err)
	}
	if err := json.Unmarshal(reservedRaw, &snapshot.Reserved); err != nil {
		return budget.DurableRootSnapshot{}, fmt.Errorf("decode budget reservation: %w", err)
	}
	return snapshot, nil
}

func (backend *sqlBudgetBackend) AcquireLease(rootRunID, owner string, now time.Time, ttl time.Duration) error {
	if err := validateBudgetLeaseRequest(backend, rootRunID, owner, ttl); err != nil {
		return err
	}
	now = now.UTC()
	expiresAt := now.Add(ttl)
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	currentOwner, currentExpiry, reserved, err := lockBudgetLease(tx, rootRunID)
	if err != nil {
		return err
	}
	leaseExpired := currentOwner != "" && currentExpiry.Valid && !currentExpiry.Time.After(now)
	switch {
	case currentOwner == owner && !leaseExpired:
		// Idempotent reacquisition by the live owner is a renewal.
	case currentOwner == "":
		if !isZeroBudgetUsage(reserved) {
			return fmt.Errorf("%w: unowned root %q has reserved usage", budget.ErrLeaseHasReservations, rootRunID)
		}
	case leaseExpired:
		if err := reclaimBudgetReservations(tx, rootRunID, now); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: root %q is owned by %q", budget.ErrLeaseHeld, rootRunID, currentOwner)
	}
	if err := setBudgetLease(tx, rootRunID, owner, &expiresAt, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (backend *sqlBudgetBackend) RenewLease(rootRunID, owner string, now time.Time, ttl time.Duration) error {
	if err := validateBudgetLeaseRequest(backend, rootRunID, owner, ttl); err != nil {
		return err
	}
	now = now.UTC()
	expiresAt := now.Add(ttl)
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	currentOwner, currentExpiry, _, err := lockBudgetLease(tx, rootRunID)
	if err != nil {
		return err
	}
	if currentOwner != owner {
		return fmt.Errorf("%w: root %q is owned by %q", budget.ErrLeaseOwnerMismatch, rootRunID, currentOwner)
	}
	if !currentExpiry.Valid || !currentExpiry.Time.After(now) {
		return fmt.Errorf("%w: root %q", budget.ErrLeaseNotActive, rootRunID)
	}
	if err := setBudgetLease(tx, rootRunID, owner, &expiresAt, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (backend *sqlBudgetBackend) ReleaseLease(rootRunID, owner string, now time.Time) error {
	if backend == nil || backend.db == nil {
		return fmt.Errorf("budget database is required")
	}
	if rootRunID == "" || owner == "" {
		return fmt.Errorf("budget root and lease owner are required")
	}
	now = now.UTC()
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	currentOwner, _, reserved, err := lockBudgetLease(tx, rootRunID)
	if err != nil {
		return err
	}
	if currentOwner == "" {
		return tx.Commit()
	}
	if currentOwner != owner {
		return fmt.Errorf("%w: root %q is owned by %q", budget.ErrLeaseOwnerMismatch, rootRunID, currentOwner)
	}
	var active int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND state IN (?,?)`,
		rootRunID, "open", "active",
	).Scan(&active); err != nil {
		return err
	}
	if active > 0 || !isZeroBudgetUsage(reserved) {
		return fmt.Errorf("%w: root %q has %d active reservation(s)", budget.ErrLeaseHasReservations, rootRunID, active)
	}
	if err := setBudgetLease(tx, rootRunID, "", nil, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (backend *sqlBudgetBackend) ReclaimExpired(rootRunID string, now time.Time) error {
	if backend == nil || backend.db == nil {
		return fmt.Errorf("budget database is required")
	}
	if rootRunID == "" {
		return fmt.Errorf("budget root run id is required")
	}
	now = now.UTC()
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	owner, expiresAt, _, err := lockBudgetLease(tx, rootRunID)
	if err != nil {
		return err
	}
	if owner == "" || !expiresAt.Valid || expiresAt.Time.After(now) {
		return tx.Commit()
	}
	if err := reclaimBudgetReservations(tx, rootRunID, now); err != nil {
		return err
	}
	if err := setBudgetLease(tx, rootRunID, "", nil, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (backend *sqlBudgetBackend) AcquireLeaseWithFence(rootRunID, owner string, now time.Time, ttl time.Duration) (int64, error) {
	if err := validateBudgetLeaseRequest(backend, rootRunID, owner, ttl); err != nil {
		return 0, err
	}
	now = now.UTC()
	expires := now.Add(ttl)
	tx, err := backend.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var currentOwner string
	var currentExpiry sql.NullTime
	var reservedRaw []byte
	var fence int64
	if err := tx.QueryRow(`SELECT lease_owner,lease_expires_at,reserved_usage_json,lease_fence FROM agent_run_budget_ledger WHERE root_run_id=? FOR UPDATE`, rootRunID).Scan(&currentOwner, &currentExpiry, &reservedRaw, &fence); err != nil {
		return 0, err
	}
	var reserved agentapi.Usage
	if err := json.Unmarshal(reservedRaw, &reserved); err != nil {
		return 0, err
	}
	expired := currentOwner != "" && currentExpiry.Valid && !currentExpiry.Time.After(now)
	switch {
	case currentOwner == owner && !expired:
	case currentOwner == "":
		if !isZeroBudgetUsage(reserved) {
			return 0, fmt.Errorf("%w: unowned root %q has reserved usage", budget.ErrLeaseHasReservations, rootRunID)
		}
		fence++
	case expired:
		if err := reclaimBudgetReservations(tx, rootRunID, now); err != nil {
			return 0, err
		}
		fence++
	default:
		return 0, fmt.Errorf("%w: root %q is owned by %q", budget.ErrLeaseHeld, rootRunID, currentOwner)
	}
	if _, err := tx.Exec(`UPDATE agent_run_budget_ledger SET lease_owner=?,lease_expires_at=?,lease_fence=?,version=version+1,updated_at=? WHERE root_run_id=?`, owner, store.DatabaseTime(expires.Format(time.RFC3339Nano)), fence, store.DatabaseTime(now.Format(time.RFC3339Nano)), rootRunID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return fence, nil
}

func (backend *sqlBudgetBackend) RenewLeaseWithFence(rootRunID, owner string, fence int64, now time.Time, ttl time.Duration) error {
	if err := validateBudgetLeaseRequest(backend, rootRunID, owner, ttl); err != nil {
		return err
	}
	if fence <= 0 {
		return budget.ErrLeaseNotActive
	}
	now = now.UTC()
	expires := now.Add(ttl)
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentOwner string
	var currentExpiry sql.NullTime
	var currentFence int64
	if err := tx.QueryRow(`SELECT lease_owner,lease_expires_at,lease_fence FROM agent_run_budget_ledger WHERE root_run_id=? FOR UPDATE`, rootRunID).Scan(&currentOwner, &currentExpiry, &currentFence); err != nil {
		return err
	}
	if currentOwner != owner || currentFence != fence {
		return fmt.Errorf("%w: root %q", budget.ErrLeaseOwnerMismatch, rootRunID)
	}
	if !currentExpiry.Valid || !currentExpiry.Time.After(now) {
		return budget.ErrLeaseNotActive
	}
	_, err = tx.Exec(`UPDATE agent_run_budget_ledger SET lease_expires_at=?,version=version+1,updated_at=? WHERE root_run_id=? AND lease_owner=? AND lease_fence=?`, store.DatabaseTime(expires.Format(time.RFC3339Nano)), store.DatabaseTime(now.Format(time.RFC3339Nano)), rootRunID, owner, fence)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func applyLeaseReleaseTx(tx *sql.Tx, rootRunID, owner string, fence int64, now time.Time) error {
	var currentOwner string
	var currentExpiry sql.NullTime
	var currentFence int64
	var reservedRaw []byte
	if err := tx.QueryRow(`SELECT lease_owner,lease_expires_at,lease_fence,reserved_usage_json FROM agent_run_budget_ledger WHERE root_run_id=? FOR UPDATE`, rootRunID).Scan(&currentOwner, &currentExpiry, &currentFence, &reservedRaw); err != nil {
		return err
	}
	if currentOwner == "" {
		return nil
	}
	if currentOwner != owner || currentFence != fence {
		return budget.ErrLeaseOwnerMismatch
	}
	var reserved agentapi.Usage
	if err := json.Unmarshal(reservedRaw, &reserved); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_run_budget_reservations WHERE root_run_id=? AND state IN (?,?)`, rootRunID, "open", "active").Scan(&active); err != nil {
		return err
	}
	if active > 0 || !isZeroBudgetUsage(reserved) {
		return budget.ErrLeaseHasReservations
	}
	_, err := tx.Exec(`UPDATE agent_run_budget_ledger SET lease_owner='',lease_expires_at=NULL,version=version+1,updated_at=? WHERE root_run_id=? AND lease_owner=? AND lease_fence=?`, store.DatabaseTime(now.UTC().Format(time.RFC3339Nano)), rootRunID, owner, fence)
	if err != nil {
		return err
	}
	return nil
}

func (backend *sqlBudgetBackend) ReleaseLeaseWithFence(rootRunID, owner string, fence int64, now time.Time) error {
	if backend == nil || backend.db == nil {
		return fmt.Errorf("budget database is required")
	}
	if rootRunID == "" || owner == "" || fence <= 0 {
		return fmt.Errorf("budget root, owner and fence are required")
	}
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyLeaseReleaseTx(tx, rootRunID, owner, fence, now); err != nil {
		return err
	}
	return tx.Commit()
}

func validateBudgetLeaseRequest(backend *sqlBudgetBackend, rootRunID, owner string, ttl time.Duration) error {
	if backend == nil || backend.db == nil {
		return fmt.Errorf("budget database is required")
	}
	if rootRunID == "" || owner == "" {
		return fmt.Errorf("budget root and lease owner are required")
	}
	if ttl <= 0 {
		return fmt.Errorf("budget lease ttl must be positive")
	}
	return nil
}

func lockBudgetLease(tx *sql.Tx, rootRunID string) (string, sql.NullTime, agentapi.Usage, error) {
	var owner string
	var expiresAt sql.NullTime
	var reservedRaw []byte
	if err := tx.QueryRow(
		`SELECT lease_owner,lease_expires_at,reserved_usage_json
		 FROM agent_run_budget_ledger WHERE root_run_id=? FOR UPDATE`,
		rootRunID,
	).Scan(&owner, &expiresAt, &reservedRaw); err != nil {
		return "", sql.NullTime{}, agentapi.Usage{}, err
	}
	var reserved agentapi.Usage
	if err := json.Unmarshal(reservedRaw, &reserved); err != nil {
		return "", sql.NullTime{}, agentapi.Usage{}, fmt.Errorf("decode budget reserved usage: %w", err)
	}
	return owner, expiresAt, reserved, nil
}

func reclaimBudgetReservations(tx *sql.Tx, rootRunID string, now time.Time) error {
	if _, err := tx.Exec(
		`UPDATE agent_run_budget_reservations
		 SET state=?,settled_at=?
		 WHERE root_run_id=? AND state IN (?,?)`,
		"released", store.DatabaseTime(now.UTC().Format(time.RFC3339Nano)),
		rootRunID, "open", "active",
	); err != nil {
		return err
	}
	zeroRaw, err := json.Marshal(agentapi.Usage{})
	if err != nil {
		return err
	}
	result, err := tx.Exec(
		`UPDATE agent_run_budget_ledger
		 SET reserved_usage_json=?,version=version+1,updated_at=?
		 WHERE root_run_id=?`,
		zeroRaw, store.DatabaseTime(now.UTC().Format(time.RFC3339Nano)), rootRunID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func setBudgetLease(tx *sql.Tx, rootRunID, owner string, expiresAt *time.Time, now time.Time) error {
	var expiry any
	if expiresAt != nil {
		expiry = store.DatabaseTime(expiresAt.UTC().Format(time.RFC3339Nano))
	}
	result, err := tx.Exec(
		`UPDATE agent_run_budget_ledger
		 SET lease_owner=?,lease_expires_at=?,version=version+1,updated_at=?
		 WHERE root_run_id=?`,
		owner, expiry, store.DatabaseTime(now.UTC().Format(time.RFC3339Nano)), rootRunID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (backend *sqlBudgetBackend) TaskSnapshot(rootRunID, reservationID string) (budget.DurableTaskSnapshot, error) {
	if backend == nil || backend.db == nil {
		return budget.DurableTaskSnapshot{}, fmt.Errorf("budget database is required")
	}
	tx, err := backend.db.Begin()
	if err != nil {
		return budget.DurableTaskSnapshot{}, err
	}
	defer tx.Rollback()
	row := tx.QueryRow(
		`SELECT grant_usage_json,used_usage_json,state
		 FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND reservation_id=? AND kind=?`,
		rootRunID, reservationID, "task",
	)
	var grantRaw, usedRaw []byte
	var state string
	if err := row.Scan(&grantRaw, &usedRaw, &state); err != nil {
		return budget.DurableTaskSnapshot{}, err
	}
	var snapshot budget.DurableTaskSnapshot
	if err := json.Unmarshal(grantRaw, &snapshot.Grant); err != nil {
		return budget.DurableTaskSnapshot{}, fmt.Errorf("decode task grant: %w", err)
	}
	if err := json.Unmarshal(usedRaw, &snapshot.Used); err != nil {
		return budget.DurableTaskSnapshot{}, fmt.Errorf("decode task usage: %w", err)
	}
	snapshot.Released = state != "active"
	rows, err := tx.Query(
		`SELECT estimate_usage_json FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND parent_reservation_id=? AND state=?`,
		rootRunID, reservationID, "open",
	)
	if err != nil {
		return budget.DurableTaskSnapshot{}, err
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return budget.DurableTaskSnapshot{}, err
		}
		var estimate agentapi.Usage
		if err := json.Unmarshal(raw, &estimate); err != nil {
			rows.Close()
			return budget.DurableTaskSnapshot{}, fmt.Errorf("decode task call estimate: %w", err)
		}
		snapshot.InFlight = addUsage(snapshot.InFlight, estimate)
	}
	if err := rows.Close(); err != nil {
		return budget.DurableTaskSnapshot{}, err
	}
	if err := rows.Err(); err != nil {
		return budget.DurableTaskSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return budget.DurableTaskSnapshot{}, err
	}
	return snapshot, nil
}

func (backend *sqlBudgetBackend) reserve(rootRunID string, reservation budget.DurableReservation, owner string, fence int64) error {
	reservation, estimate, grant, err := validateAndNormalizeReservation(backend, rootRunID, reservation)
	if err != nil {
		return err
	}
	estimateRaw, grantRaw, zeroRaw, err := marshalBudgetReservation(estimate, grant)
	if err != nil {
		return err
	}
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	limitsRaw, usedRaw, reservedRaw, err := loadReservationLedger(tx, rootRunID, owner, fence)
	if err != nil {
		return err
	}
	if existing, found, err := findBudgetReservation(tx, reservation.ID); err != nil {
		return err
	} else if found {
		if err := validateDuplicateReservation(existing, rootRunID, reservation, estimate, grant); err != nil {
			return err
		}
		return tx.Commit()
	}

	limits, used, reserved, err := decodeBudgetLedger(limitsRaw, usedRaw, reservedRaw)
	if err != nil {
		return err
	}
	reserved, err = applyReservationChange(tx, rootRunID, reservation, estimate, grant, limits, used, reserved)
	if err != nil {
		return err
	}

	committed, err := insertBudgetReservation(tx, rootRunID, reservation, estimateRaw, grantRaw, zeroRaw, estimate, grant)
	if err != nil {
		return err
	}
	if committed {
		return nil
	}
	if err := updateBudgetRoot(tx, rootRunID, used, reserved); err != nil {
		return err
	}
	return tx.Commit()
}

func validateReservationShape(reservation *budget.DurableReservation, estimate, grant agentapi.Usage) error {
	if reservation.Kind != "task" && reservation.Kind != "call" {
		return fmt.Errorf("unsupported budget reservation kind %q", reservation.Kind)
	}
	if reservation.Phase == "" {
		reservation.Phase = agentapi.RunBudgetPhaseDefault
	}
	if reservation.Kind == "task" {
		if reservation.ParentID != "" {
			return fmt.Errorf("task reservation cannot have a parent")
		}
		if !isZeroBudgetUsage(estimate) {
			return fmt.Errorf("task reservation cannot have a physical-call estimate")
		}
		return nil
	}
	if reservation.ParentID != "" && reservation.ParentID == reservation.ID {
		return fmt.Errorf("call reservation cannot parent itself")
	}
	if !isZeroBudgetUsage(grant) {
		return fmt.Errorf("call reservation cannot have a task grant")
	}
	return nil
}

func validateAndNormalizeReservation(
	backend *sqlBudgetBackend,
	rootRunID string,
	reservation budget.DurableReservation,
) (budget.DurableReservation, agentapi.Usage, agentapi.Usage, error) {
	if backend == nil || backend.db == nil {
		return reservation, agentapi.Usage{}, agentapi.Usage{}, fmt.Errorf("budget database is required")
	}
	if rootRunID == "" || reservation.ID == "" {
		return reservation, agentapi.Usage{}, agentapi.Usage{}, fmt.Errorf("budget root and reservation ids are required")
	}
	estimate, err := normalizeBudgetUsage(reservation.Estimate)
	if err != nil {
		return reservation, agentapi.Usage{}, agentapi.Usage{}, err
	}
	grant, err := normalizeBudgetUsage(reservation.Grant)
	if err != nil {
		return reservation, agentapi.Usage{}, agentapi.Usage{}, err
	}
	if err := validateReservationShape(&reservation, estimate, grant); err != nil {
		return reservation, agentapi.Usage{}, agentapi.Usage{}, err
	}
	return reservation, estimate, grant, nil
}

func marshalBudgetReservation(
	estimate, grant agentapi.Usage,
) (estimateRaw, grantRaw, zeroRaw []byte, err error) {
	estimateRaw, err = json.Marshal(estimate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal budget estimate: %w", err)
	}
	grantRaw, err = json.Marshal(grant)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal budget grant: %w", err)
	}
	zeroRaw, err = json.Marshal(agentapi.Usage{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal zero budget usage: %w", err)
	}
	return estimateRaw, grantRaw, zeroRaw, nil
}

func loadReservationLedger(
	tx *sql.Tx,
	rootRunID, owner string,
	fence int64,
) (limitsRaw, usedRaw, reservedRaw []byte, err error) {
	if err := tx.QueryRow(
		`SELECT limits_json,used_usage_json,reserved_usage_json
		 FROM agent_run_budget_ledger WHERE root_run_id=? FOR UPDATE`, rootRunID,
	).Scan(&limitsRaw, &usedRaw, &reservedRaw); err != nil {
		return nil, nil, nil, err
	}
	if fence > 0 {
		if err := assertBudgetFence(tx, rootRunID, owner, fence); err != nil {
			return nil, nil, nil, err
		}
	}
	return limitsRaw, usedRaw, reservedRaw, nil
}

func decodeBudgetLedger(
	limitsRaw, usedRaw, reservedRaw []byte,
) (agentapi.RunLimits, agentapi.Usage, agentapi.Usage, error) {
	var limits agentapi.RunLimits
	var used, reserved agentapi.Usage
	if err := json.Unmarshal(limitsRaw, &limits); err != nil {
		return limits, used, reserved, fmt.Errorf("decode budget limits: %w", err)
	}
	if err := json.Unmarshal(usedRaw, &used); err != nil {
		return limits, used, reserved, fmt.Errorf("decode budget used usage: %w", err)
	}
	if err := json.Unmarshal(reservedRaw, &reserved); err != nil {
		return limits, used, reserved, fmt.Errorf("decode budget reserved usage: %w", err)
	}
	return limits, used, reserved, nil
}

func applyReservationChange(
	tx *sql.Tx,
	rootRunID string,
	reservation budget.DurableReservation,
	estimate, grant agentapi.Usage,
	limits agentapi.RunLimits,
	used, reserved agentapi.Usage,
) (agentapi.Usage, error) {
	if reservation.Kind == "call" && reservation.ParentID != "" {
		if err := reserveChildCall(tx, rootRunID, reservation.ParentID, estimate); err != nil {
			return reserved, err
		}
		return reserved, nil
	}
	phase := reservation.Phase
	available := budgetAvailable(limits, used, reserved, phase)
	request := estimate
	if reservation.Kind == "task" {
		request = grant
	}
	if err := requireBudgetWithin(request, available, reservation.Kind); err != nil {
		return reserved, err
	}
	return addUsage(reserved, request), nil
}

func insertBudgetReservation(
	tx *sql.Tx,
	rootRunID string,
	reservation budget.DurableReservation,
	estimateRaw, grantRaw, zeroRaw []byte,
	estimate, grant agentapi.Usage,
) (bool, error) {
	_, execErr := tx.Exec(
		`INSERT INTO agent_run_budget_reservations(
			reservation_id,root_run_id,parent_reservation_id,kind,phase,
			estimate_usage_json,grant_usage_json,used_usage_json,state,
			created_at,settled_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		reservation.ID, rootRunID, reservation.ParentID, reservation.Kind,
		string(reservation.Phase), estimateRaw, grantRaw, zeroRaw, reservationStateFor(reservation),
		store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)), nil,
	)
	if execErr == nil {
		return false, nil
	}
	if !isDuplicate(execErr) {
		return false, execErr
	}
	existing, found, lookupErr := findBudgetReservation(tx, reservation.ID)
	if lookupErr != nil {
		return false, lookupErr
	}
	if !found {
		return false, execErr
	}
	if validateErr := validateDuplicateReservation(existing, rootRunID, reservation, estimate, grant); validateErr != nil {
		return false, validateErr
	}
	return true, tx.Commit()
}

func (backend *sqlBudgetBackend) Reserve(rootRunID string, reservation budget.DurableReservation) error {
	return backend.reserve(rootRunID, reservation, "", 0)
}
func (backend *sqlBudgetBackend) ReserveFenced(rootRunID, owner string, fence int64, reservation budget.DurableReservation) error {
	return backend.reserve(rootRunID, reservation, owner, fence)
}

type budgetReservationRow struct {
	RootID   string
	ParentID string
	Kind     string
	Phase    string
	Estimate agentapi.Usage
	Grant    agentapi.Usage
	State    string
}

func findBudgetReservation(tx *sql.Tx, reservationID string) (budgetReservationRow, bool, error) {
	row := tx.QueryRow(
		`SELECT root_run_id,parent_reservation_id,kind,phase,
			estimate_usage_json,grant_usage_json,state
		 FROM agent_run_budget_reservations WHERE reservation_id=? FOR UPDATE`,
		reservationID,
	)
	var result budgetReservationRow
	var estimateRaw, grantRaw []byte
	if err := row.Scan(
		&result.RootID, &result.ParentID, &result.Kind, &result.Phase,
		&estimateRaw, &grantRaw, &result.State,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return budgetReservationRow{}, false, nil
		}
		return budgetReservationRow{}, false, err
	}
	if err := json.Unmarshal(estimateRaw, &result.Estimate); err != nil {
		return budgetReservationRow{}, false, fmt.Errorf("decode existing budget estimate: %w", err)
	}
	if err := json.Unmarshal(grantRaw, &result.Grant); err != nil {
		return budgetReservationRow{}, false, fmt.Errorf("decode existing budget grant: %w", err)
	}
	return result, true, nil
}

func validateDuplicateReservation(
	existing budgetReservationRow,
	rootRunID string,
	requested budget.DurableReservation,
	estimate, grant agentapi.Usage,
) error {
	if existing.RootID != rootRunID || existing.ParentID != requested.ParentID ||
		existing.Kind != requested.Kind || existing.Phase != string(requested.Phase) ||
		existing.Estimate != estimate || existing.Grant != grant {
		return fmt.Errorf("budget reservation %q conflicts with an existing reservation", requested.ID)
	}
	switch requested.Kind {
	case "task":
		if existing.State != "active" {
			return fmt.Errorf("budget task reservation %q is no longer active", requested.ID)
		}
	case "call":
		if existing.State != "open" && existing.State != "settled" {
			return fmt.Errorf("budget call reservation %q is no longer open", requested.ID)
		}
	default:
		return fmt.Errorf("unsupported budget reservation kind %q", requested.Kind)
	}
	return nil
}

func reserveChildCall(tx *sql.Tx, rootRunID, parentID string, estimate agentapi.Usage) error {
	var grantRaw, usedRaw []byte
	var state string
	if err := tx.QueryRow(
		`SELECT grant_usage_json,used_usage_json,state
		 FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND reservation_id=? AND kind=? FOR UPDATE`,
		rootRunID, parentID, "task",
	).Scan(&grantRaw, &usedRaw, &state); err != nil {
		return err
	}
	if state != "active" {
		return fmt.Errorf("%w: child task budget released", agentapi.ErrBudgetExceeded)
	}
	var grant, used agentapi.Usage
	if err := json.Unmarshal(grantRaw, &grant); err != nil {
		return err
	}
	if err := json.Unmarshal(usedRaw, &used); err != nil {
		return err
	}
	rows, err := tx.Query(
		`SELECT estimate_usage_json FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND parent_reservation_id=? AND state=? FOR UPDATE`,
		rootRunID, parentID, "open",
	)
	if err != nil {
		return err
	}
	var inFlight agentapi.Usage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		var usage agentapi.Usage
		if err := json.Unmarshal(raw, &usage); err != nil {
			rows.Close()
			return err
		}
		inFlight = addUsage(inFlight, usage)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return requireBudgetWithin(estimate, subtractUsage(grant, addUsage(used, inFlight)), "child call")
}

func (backend *sqlBudgetBackend) settleCall(rootRunID, reservationID string, actual agentapi.Usage, owner string, fence int64) error {
	actual, err := normalizeBudgetUsage(actual)
	if err != nil {
		return err
	}
	actualRaw, err := json.Marshal(actual)
	if err != nil {
		return fmt.Errorf("marshal actual budget usage: %w", err)
	}
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	parentID, estimate, alreadySettled, err := loadOpenCallReservation(tx, rootRunID, reservationID, actual)
	if err != nil {
		return err
	}
	if alreadySettled {
		return nil
	}
	accountingErr := requireBudgetWithin(actual, estimate, "reported model usage")

	used, reserved, err := applySettledCallToLedger(tx, rootRunID, parentID, actual, estimate, owner, fence)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`UPDATE agent_run_budget_reservations
			 SET state=?,used_usage_json=?,settled_at=?
			 WHERE root_run_id=? AND reservation_id=? AND state=?`,
		"settled", actualRaw, store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
		rootRunID, reservationID, "open",
	); err != nil {
		return err
	}
	if err := updateBudgetRoot(tx, rootRunID, used, reserved); err != nil {
		return err
	}
	limits, err := loadBudgetLimits(tx, rootRunID)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if accountingErr == nil {
		accountingErr = budgetWithinLimits(limits, addUsage(used, reserved), "durable root")
	}
	if accountingErr != nil {
		return &budget.SettlementError{Err: accountingErr, Committed: true}
	}
	return nil
}

// loadOpenCallReservation validates the call reservation row and returns its
// parent grant and estimate. A previously settled call with identical usage is
// idempotent; a differing second settlement or a non-open state is an error.
func loadOpenCallReservation(
	tx *sql.Tx,
	rootRunID, reservationID string,
	actual agentapi.Usage,
) (parentID string, estimate agentapi.Usage, alreadySettled bool, err error) {
	var kind, state string
	var estimateRaw, usedRaw []byte
	if err := tx.QueryRow(
		`SELECT parent_reservation_id,kind,state,estimate_usage_json,used_usage_json
		 FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND reservation_id=? FOR UPDATE`, rootRunID, reservationID,
	).Scan(&parentID, &kind, &state, &estimateRaw, &usedRaw); err != nil {
		return "", agentapi.Usage{}, false, err
	}
	if kind != "call" {
		return "", agentapi.Usage{}, false, fmt.Errorf("budget reservation %q is not a physical call", reservationID)
	}
	if state == "settled" {
		var previous agentapi.Usage
		if err := json.Unmarshal(usedRaw, &previous); err != nil {
			return "", agentapi.Usage{}, false, err
		}
		if previous != actual {
			return "", agentapi.Usage{}, false, fmt.Errorf("%w: model call settled twice with different usage", agentapi.ErrBudgetExceeded)
		}
		return "", agentapi.Usage{}, true, nil
	}
	if state != "open" {
		return "", agentapi.Usage{}, false, fmt.Errorf("%w: model call reservation is not open", agentapi.ErrBudgetExceeded)
	}
	if err := json.Unmarshal(estimateRaw, &estimate); err != nil {
		return "", agentapi.Usage{}, false, err
	}
	return parentID, estimate, false, nil
}

// applySettledCallToLedger folds the settled usage into the root ledger and, for
// task-backed calls, the parent task grant's used capacity.
func applySettledCallToLedger(
	tx *sql.Tx,
	rootRunID, parentID string,
	actual, estimate agentapi.Usage,
	owner string,
	fence int64,
) (used, reserved agentapi.Usage, err error) {
	var usedRawRoot, reservedRaw []byte
	if err := tx.QueryRow(
		`SELECT used_usage_json,reserved_usage_json
		 FROM agent_run_budget_ledger WHERE root_run_id=? FOR UPDATE`, rootRunID,
	).Scan(&usedRawRoot, &reservedRaw); err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	if fence > 0 {
		if err := assertBudgetFence(tx, rootRunID, owner, fence); err != nil {
			return agentapi.Usage{}, agentapi.Usage{}, err
		}
	}
	if err := json.Unmarshal(usedRawRoot, &used); err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	if err := json.Unmarshal(reservedRaw, &reserved); err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	used = addUsage(used, actual)
	if parentID == "" {
		reserved = subtractUsage(reserved, estimate)
		return used, reserved, nil
	}
	// A task grant is kept in reserved_usage_json as its remaining
	// unconsumed capacity. Once a child call settles, the consumed part
	// moves to used_usage_json and must leave the root reservation.
	reserved = subtractUsage(reserved, actual)

	var taskState string
	var taskUsedRaw []byte
	if err := tx.QueryRow(
		`SELECT state,used_usage_json FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND reservation_id=? AND kind=? FOR UPDATE`,
		rootRunID, parentID, "task",
	).Scan(&taskState, &taskUsedRaw); err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	if taskState != "active" {
		return agentapi.Usage{}, agentapi.Usage{}, fmt.Errorf("%w: child task budget released before call settlement", agentapi.ErrBudgetExceeded)
	}
	var taskUsed agentapi.Usage
	if err := json.Unmarshal(taskUsedRaw, &taskUsed); err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	taskUsed = addUsage(taskUsed, actual)
	taskUsedRaw, err = json.Marshal(taskUsed)
	if err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	if _, err := tx.Exec(
		`UPDATE agent_run_budget_reservations SET used_usage_json=? WHERE root_run_id=? AND reservation_id=?`,
		taskUsedRaw, rootRunID, parentID,
	); err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	return used, reserved, nil
}

func loadBudgetLimits(tx *sql.Tx, rootRunID string) (agentapi.RunLimits, error) {
	var limitsRaw []byte
	if err := tx.QueryRow(
		`SELECT limits_json FROM agent_run_budget_ledger WHERE root_run_id=?`, rootRunID,
	).Scan(&limitsRaw); err != nil {
		return agentapi.RunLimits{}, err
	}
	var limits agentapi.RunLimits
	if err := json.Unmarshal(limitsRaw, &limits); err != nil {
		return agentapi.RunLimits{}, err
	}
	return limits, nil
}

func (backend *sqlBudgetBackend) SettleCall(rootRunID, reservationID string, actual agentapi.Usage) error {
	return backend.settleCall(rootRunID, reservationID, actual, "", 0)
}
func (backend *sqlBudgetBackend) SettleCallFenced(rootRunID, owner string, fence int64, reservationID string, actual agentapi.Usage) error {
	return backend.settleCall(rootRunID, reservationID, actual, owner, fence)
}

func (backend *sqlBudgetBackend) ReleaseCall(rootRunID, reservationID string) error {
	return backend.release(rootRunID, reservationID, "call", "", 0)
}
func (backend *sqlBudgetBackend) ReleaseCallFenced(rootRunID, owner string, fence int64, reservationID string) error {
	return backend.release(rootRunID, reservationID, "call", owner, fence)
}

func applyReleaseTx(tx *sql.Tx, rootRunID, reservationID, kind, owner string, fence int64) error {
	reservation, err := loadReleaseReservation(tx, rootRunID, reservationID, kind)
	if err != nil {
		return err
	}
	if !reservation.releasable {
		return nil
	}
	used, reserved, err := loadReleaseLedger(tx, rootRunID)
	if err != nil {
		return err
	}
	if fence > 0 {
		if err := assertBudgetFence(tx, rootRunID, owner, fence); err != nil {
			return err
		}
	}
	if kind == "call" && reservation.parentID == "" {
		var estimate agentapi.Usage
		if err := json.Unmarshal(reservation.estimateRaw, &estimate); err != nil {
			return err
		}
		reserved = subtractUsage(reserved, estimate)
	}
	if kind == "task" {
		reserved, err = releaseTaskReservation(tx, rootRunID, reservationID, reserved)
		if err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`UPDATE agent_run_budget_reservations SET state=?,settled_at=?
		 WHERE root_run_id=? AND reservation_id=? AND state IN (?,?)`,
		"released", store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)),
		rootRunID, reservationID, "open", "active",
	); err != nil {
		return err
	}
	if err := updateBudgetRoot(tx, rootRunID, used, reserved); err != nil {
		return err
	}
	return nil
}

func (backend *sqlBudgetBackend) release(rootRunID, reservationID, kind, owner string, fence int64) error {
	if backend == nil || backend.db == nil {
		return fmt.Errorf("budget database is required")
	}
	tx, err := backend.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := applyReleaseTx(tx, rootRunID, reservationID, kind, owner, fence); err != nil {
		return err
	}
	return tx.Commit()

}

type releaseReservation struct {
	parentID    string
	estimateRaw []byte
	releasable  bool
}

func loadReleaseReservation(tx *sql.Tx, rootRunID, reservationID, kind string) (releaseReservation, error) {
	var parentID, state string
	var estimateRaw []byte
	if err := tx.QueryRow(
		`SELECT parent_reservation_id,state,estimate_usage_json
		 FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND reservation_id=? AND kind=? FOR UPDATE`,
		rootRunID, reservationID, kind,
	).Scan(&parentID, &state, &estimateRaw); err != nil {
		return releaseReservation{}, err
	}
	if state == "released" || state == "settled" {
		return releaseReservation{releasable: false}, nil
	}
	if state != "open" && !(kind == "task" && state == "active") {
		return releaseReservation{}, fmt.Errorf("budget reservation %q is not releasable", reservationID)
	}
	return releaseReservation{parentID: parentID, estimateRaw: estimateRaw, releasable: true}, nil
}

func loadReleaseLedger(tx *sql.Tx, rootRunID string) (agentapi.Usage, agentapi.Usage, error) {
	var usedRaw, reservedRaw []byte
	if err := tx.QueryRow(
		`SELECT used_usage_json,reserved_usage_json
		 FROM agent_run_budget_ledger WHERE root_run_id=? FOR UPDATE`, rootRunID,
	).Scan(&usedRaw, &reservedRaw); err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	var used, reserved agentapi.Usage
	if err := json.Unmarshal(usedRaw, &used); err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	if err := json.Unmarshal(reservedRaw, &reserved); err != nil {
		return agentapi.Usage{}, agentapi.Usage{}, err
	}
	return used, reserved, nil
}

func releaseTaskReservation(tx *sql.Tx, rootRunID, reservationID string, reserved agentapi.Usage) (agentapi.Usage, error) {
	var grant, taskUsed agentapi.Usage
	var grantRaw, taskUsedRaw []byte
	if err := tx.QueryRow(
		`SELECT grant_usage_json,used_usage_json FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND reservation_id=? AND kind=? FOR UPDATE`,
		rootRunID, reservationID, "task",
	).Scan(&grantRaw, &taskUsedRaw); err != nil {
		return reserved, err
	}
	if err := json.Unmarshal(grantRaw, &grant); err != nil {
		return reserved, err
	}
	if err := json.Unmarshal(taskUsedRaw, &taskUsed); err != nil {
		return reserved, err
	}
	// An open child call means task usage is not final and releasing it
	// would make the root ledger under-accounted.
	var openCalls int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM agent_run_budget_reservations
		 WHERE root_run_id=? AND parent_reservation_id=? AND state=?`,
		rootRunID, reservationID, "open",
	).Scan(&openCalls); err != nil {
		return reserved, err
	}
	if openCalls > 0 {
		return reserved, fmt.Errorf("%w: cannot release child task with in-flight model calls", agentapi.ErrBudgetExceeded)
	}
	return subtractUsage(reserved, subtractUsage(grant, taskUsed)), nil
}

func (backend *sqlBudgetBackend) ReleaseTask(rootRunID, reservationID string) error {
	return backend.release(rootRunID, reservationID, "task", "", 0)
}
func (backend *sqlBudgetBackend) ReleaseTaskFenced(rootRunID, owner string, fence int64, reservationID string) error {
	return backend.release(rootRunID, reservationID, "task", owner, fence)
}

func assertBudgetFence(tx *sql.Tx, rootRunID, owner string, fence int64) error {
	var storedOwner string
	var storedFence int64
	var expiry sql.NullTime
	if err := tx.QueryRow(`SELECT lease_owner,lease_fence,lease_expires_at FROM agent_run_budget_ledger WHERE root_run_id=? FOR UPDATE`, rootRunID).Scan(&storedOwner, &storedFence, &expiry); err != nil {
		return err
	}
	if storedOwner != owner || storedFence != fence || !expiry.Valid || !expiry.Time.After(time.Now().UTC()) {
		return fmt.Errorf("%w: root %q", budget.ErrLeaseOwnerMismatch, rootRunID)
	}
	return nil
}

func reservationStateFor(reservation budget.DurableReservation) string {
	if reservation.Kind == "task" {
		return "active"
	}
	return "open"
}

func updateBudgetRoot(tx *sql.Tx, rootRunID string, used, reserved agentapi.Usage) error {
	usedRaw, err := json.Marshal(used)
	if err != nil {
		return err
	}
	reservedRaw, err := json.Marshal(reserved)
	if err != nil {
		return err
	}
	result, err := tx.Exec(
		`UPDATE agent_run_budget_ledger
			 SET used_usage_json=?,reserved_usage_json=?,version=version+1,updated_at=?
			 WHERE root_run_id=?`,
		usedRaw, reservedRaw, store.DatabaseTime(time.Now().UTC().Format(time.RFC3339Nano)), rootRunID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func normalizeBudgetUsage(usage agentapi.Usage) (agentapi.Usage, error) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningTokens < 0 || usage.TotalTokens < 0 || usage.CostMicros < 0 {
		return agentapi.Usage{}, fmt.Errorf("%w: usage cannot be negative", agentapi.ErrBudgetExceeded)
	}
	if usage.TotalTokens == 0 {
		if usage.InputTokens > math.MaxInt64-usage.OutputTokens {
			return agentapi.Usage{}, fmt.Errorf("%w: usage total overflow", agentapi.ErrBudgetExceeded)
		}
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage, nil
}

func addUsage(left, right agentapi.Usage) agentapi.Usage {
	return agentapi.Usage{
		InputTokens:     saturatingAddBudget(left.InputTokens, right.InputTokens),
		OutputTokens:    saturatingAddBudget(left.OutputTokens, right.OutputTokens),
		ReasoningTokens: saturatingAddBudget(left.ReasoningTokens, right.ReasoningTokens),
		TotalTokens:     saturatingAddBudget(left.TotalTokens, right.TotalTokens),
		CostMicros:      saturatingAddBudget(left.CostMicros, right.CostMicros),
	}
}

func subtractUsage(left, right agentapi.Usage) agentapi.Usage {
	return agentapi.Usage{
		InputTokens:     maxInt64(0, left.InputTokens-right.InputTokens),
		OutputTokens:    maxInt64(0, left.OutputTokens-right.OutputTokens),
		ReasoningTokens: maxInt64(0, left.ReasoningTokens-right.ReasoningTokens),
		TotalTokens:     maxInt64(0, left.TotalTokens-right.TotalTokens),
		CostMicros:      maxInt64(0, left.CostMicros-right.CostMicros),
	}
}

func isZeroBudgetUsage(usage agentapi.Usage) bool {
	return usage == (agentapi.Usage{})
}

func sameBudgetLimits(left, right agentapi.RunLimits) bool {
	return left.Deadline.Equal(right.Deadline) &&
		left.MaxSteps == right.MaxSteps &&
		left.MaxToolCalls == right.MaxToolCalls &&
		left.MaxInputTokens == right.MaxInputTokens &&
		left.MaxContextTokens == right.MaxContextTokens &&
		left.MaxOutputTokens == right.MaxOutputTokens &&
		left.MaxTotalTokens == right.MaxTotalTokens &&
		left.MaxCostMicros == right.MaxCostMicros &&
		left.ParentAnswerReserve == right.ParentAnswerReserve
}

func budgetWithinLimits(limits agentapi.RunLimits, allocated agentapi.Usage, subject string) error {
	if limits.MaxInputTokens > 0 && allocated.InputTokens > limits.MaxInputTokens {
		return fmt.Errorf("%w: %s input tokens %d exceed %d", agentapi.ErrBudgetExceeded, subject, allocated.InputTokens, limits.MaxInputTokens)
	}
	if limits.MaxTotalTokens > 0 && allocated.TotalTokens > limits.MaxTotalTokens {
		return fmt.Errorf("%w: %s total tokens %d exceed %d", agentapi.ErrBudgetExceeded, subject, allocated.TotalTokens, limits.MaxTotalTokens)
	}
	if limits.MaxCostMicros > 0 && allocated.CostMicros > limits.MaxCostMicros {
		return fmt.Errorf("%w: %s cost %d exceed %d", agentapi.ErrBudgetExceeded, subject, allocated.CostMicros, limits.MaxCostMicros)
	}
	return nil
}

func saturatingAddBudget(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func budgetAvailable(limits agentapi.RunLimits, used, reserved agentapi.Usage, phase agentapi.RunBudgetPhase) agentapi.Usage {
	allocated := addUsage(used, reserved)
	available := agentapi.Usage{
		InputTokens:  remainingBudget(limits.MaxInputTokens, allocated.InputTokens),
		OutputTokens: remainingBudget(limits.MaxTotalTokens, allocated.TotalTokens),
		TotalTokens:  remainingBudget(limits.MaxTotalTokens, allocated.TotalTokens),
		CostMicros:   remainingBudget(limits.MaxCostMicros, allocated.CostMicros),
	}
	if phase != agentapi.RunBudgetPhaseAnswer && limits.ParentAnswerReserve > 0 {
		available.OutputTokens = maxInt64(0, available.OutputTokens-limits.ParentAnswerReserve)
		available.TotalTokens = maxInt64(0, available.TotalTokens-limits.ParentAnswerReserve)
	}
	return available
}

func requireBudgetWithin(request, available agentapi.Usage, subject string) error {
	if request.InputTokens > available.InputTokens || request.OutputTokens > available.OutputTokens || request.TotalTokens > available.TotalTokens || request.CostMicros > available.CostMicros {
		return fmt.Errorf("%w: %s input=%d output=%d total=%d cost=%d available_input=%d available_output=%d available_total=%d available_cost=%d", agentapi.ErrBudgetExceeded, subject, request.InputTokens, request.OutputTokens, request.TotalTokens, request.CostMicros, available.InputTokens, available.OutputTokens, available.TotalTokens, available.CostMicros)
	}
	return nil
}

func remainingBudget(limit, used int64) int64 {
	if limit <= 0 {
		return math.MaxInt64
	}
	if used >= limit {
		return 0
	}
	return limit - used
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func isDuplicate(err error) bool {
	// MySQL drivers expose duplicate-key errors with code 1062, but keep this
	// check deliberately conservative so a backend error is never hidden.
	var mysqlErr *mysqlDriver.MySQLError
	return err != nil && !errors.Is(err, sql.ErrNoRows) &&
		errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

// CreateWithDurableBudget atomically creates the logical Agent Run, its root
// ledger, and the initial fenced lease. No caller can observe one without the
// other, which prevents orphan runs/ledgers during startup failures.
func (rs *Store) CreateWithDurableBudget(record Record, limits agentapi.RunLimits) (agentapi.RunBudgetGate, error) {
	return rs.CreateWithDurableBudgetContext(context.Background(), record, limits)
}

// CreateWithDurableBudgetContext atomically creates the run, ledger and initial lease.
func (rs *Store) CreateWithDurableBudgetContext(ctx context.Context, record Record, limits agentapi.RunLimits) (agentapi.RunBudgetGate, error) {
	if rs == nil || rs.db == nil {
		return nil, fmt.Errorf("agent/runstore: database is required")
	}
	if rs.budgetLeaseOwner == "" {
		return nil, fmt.Errorf("agent/runstore: durable budget lease owner is required")
	}
	limitsRaw, err := json.Marshal(limits)
	if err != nil {
		return nil, err
	}
	zeroRaw, err := json.Marshal(agentapi.Usage{})
	if err != nil {
		return nil, err
	}
	selectionJSON, err := json.Marshal(record.Selection)
	if err != nil {
		return nil, err
	}
	runLimitsJSON, err := json.Marshal(record.RunLimits)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	ttl := durableBudgetLeaseTTL(limits.Deadline, now)
	expiry := now.Add(ttl)
	tx, err := rs.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if record.RunKind == "" {
		record.RunKind = KindAgent
	}
	if record.Status == "" {
		record.Status = StatusRunning
	}
	if record.StartedAt == "" {
		record.StartedAt = now.Format(time.RFC3339)
	}
	_, err = tx.Exec(`INSERT INTO agent_runs(
		id,run_kind,user_id,session_id,agent_id,definition_version,definition_hash,selection_json,tool_snapshot_id,
		input_schema_version,output_schema_version,parent_run_id,capability_id,capability_version,capability_content_hash,
		delegation_id,delegation_depth,run_limits_json,capability_registry_revision,workflow_run_id,workflow_node_id,
		question,status,error_code,mode,max_steps,step_count,token_used,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.ID, record.RunKind, record.UserID, record.SessionID, record.AgentID, record.DefinitionVersion, record.DefinitionHash, selectionJSON, record.ToolSnapshotID, record.InputSchemaVersion, record.OutputSchemaVersion, record.ParentRunID, record.CapabilityID, record.CapabilityVersion, record.CapabilityHash, record.DelegationID, record.DelegationDepth, runLimitsJSON, record.CapabilityRevision, record.WorkflowRunID, record.WorkflowNodeID, record.Question, record.Status, record.ErrorCode, record.Mode, record.MaxSteps, 0, 0, store.DatabaseTime(record.StartedAt))
	if err != nil {
		return nil, fmt.Errorf("create run and budget transaction: %w", err)
	}
	_, err = tx.Exec(`INSERT INTO agent_run_budget_ledger(root_run_id,limits_json,used_usage_json,reserved_usage_json,price_version,lease_owner,lease_expires_at,lease_fence,version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, record.ID, limitsRaw, zeroRaw, zeroRaw, "", rs.budgetLeaseOwner, store.DatabaseTime(expiry.Format(time.RFC3339Nano)), int64(1), int64(0), store.DatabaseTime(now.Format(time.RFC3339Nano)), store.DatabaseTime(now.Format(time.RFC3339Nano)))
	if err != nil {
		return nil, fmt.Errorf("create budget ledger: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return budget.NewDurableRootWithLeaseContext(ctx, &sqlBudgetBackend{db: rs.db, fencingEnabled: rs.fencingEnabled}, record.ID, limits, rs.budgetLeaseOwner, 1, ttl)
}

// AttachDurableBudgetContext attaches to an already-claimed root lease. It is
// used only by recovery workers; it never creates or mutates the logical run.
func (rs *Store) AttachDurableBudgetContext(ctx context.Context, rootRunID string, limits agentapi.RunLimits) (agentapi.RunBudgetGate, error) {
	if rs == nil || rs.db == nil || rs.budgetLeaseOwner == "" || rootRunID == "" {
		return nil, fmt.Errorf("agent/runstore: invalid durable recovery budget attachment")
	}
	var owner string
	var fence int64
	var expiry sql.NullTime
	if err := rs.db.QueryRowContext(ctx, `SELECT lease_owner,lease_fence,lease_expires_at FROM agent_run_budget_ledger WHERE root_run_id=?`, rootRunID).Scan(&owner, &fence, &expiry); err != nil {
		return nil, err
	}
	if owner != rs.budgetLeaseOwner || fence <= 0 || !expiry.Valid || !expiry.Time.After(time.Now().UTC()) {
		return nil, fmt.Errorf("agent/runstore: durable recovery lease is not owned by this worker")
	}
	ttl := time.Until(expiry.Time)
	if ttl <= 0 {
		return nil, fmt.Errorf("agent/runstore: durable recovery lease expired")
	}
	return budget.NewDurableRootWithLeaseContext(ctx, &sqlBudgetBackend{db: rs.db, fencingEnabled: rs.fencingEnabled}, rootRunID, limits, owner, fence, ttl)
}
