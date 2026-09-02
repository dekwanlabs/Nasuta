package run

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/budget"
)

func TestSQLBudgetEnsureRootIsIdempotentAndChecksStoredLimits(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &sqlBudgetBackend{db: db}
	limits := agentapi.RunLimits{MaxTotalTokens: 100, ParentAnswerReserve: 20, Deadline: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	limitsRaw, _ := json.Marshal(limits)
	zeroRaw, _ := json.Marshal(agentapi.Usage{})

	expectEnsureRoot(mock, "root-1", limitsRaw, zeroRaw)
	mock.ExpectCommit()
	if err := backend.EnsureRoot("root-1", limits); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// A second process may retry initialization, but it must not silently
	// replace the immutable limits pinned to the root ledger.
	db2, mock2, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	backend2 := &sqlBudgetBackend{db: db2}
	expectEnsureRoot(mock2, "root-1", limitsRaw, zeroRaw)
	mock2.ExpectRollback()
	if err := backend2.EnsureRoot("root-1", agentapi.RunLimits{MaxTotalTokens: 101, ParentAnswerReserve: 20}); err == nil {
		t.Fatal("existing root accepted conflicting limits")
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectEnsureRoot(mock sqlmock.Sqlmock, rootID string, storedLimits, zeroRaw []byte) {
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO agent_run_budget_ledger`).
		WithArgs(rootID, sqlmock.AnyArg(), zeroRaw, zeroRaw, "", "", nil, int64(0), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT limits_json FROM agent_run_budget_ledger WHERE root_run_id=\? FOR UPDATE`).
		WithArgs(rootID).
		WillReturnRows(sqlmock.NewRows([]string{"limits_json"}).AddRow(storedLimits))
	if len(storedLimits) > 0 {
		// The caller decides whether the stored JSON matches. A matching call
		// commits; a conflict rolls back. sqlmock allows either only when the
		// expectation is registered by the concrete test, so this helper is used
		// below with an explicit transaction expectation instead.
	}
}

func TestSQLBudgetReserveTaskAndDirectCallSettlement(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &sqlBudgetBackend{db: db}
	limitsRaw, _ := json.Marshal(agentapi.RunLimits{MaxTotalTokens: 100})
	zeroRaw, _ := json.Marshal(agentapi.Usage{})
	grantRaw, _ := json.Marshal(agentapi.Usage{TotalTokens: 60})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT limits_json,used_usage_json,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"limits_json", "used_usage_json", "reserved_usage_json"}).AddRow(limitsRaw, zeroRaw, zeroRaw))
	mock.ExpectQuery(`SELECT root_run_id,parent_reservation_id,kind,phase,.*FROM agent_run_budget_reservations WHERE reservation_id=\? FOR UPDATE`).
		WithArgs("task-1").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`INSERT INTO agent_run_budget_reservations`).
		WithArgs("task-1", "root-1", "", "task", "default", sqlmock.AnyArg(), grantRaw, zeroRaw, "active", sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE agent_run_budget_ledger`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "root-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := backend.Reserve("root-1", durableReservation("task-1", "task", agentapi.Usage{TotalTokens: 60})); err != nil {
		t.Fatal(err)
	}

	// A direct call consumes root-reserved estimate on settlement and moves
	// actual provider usage into used_usage_json.
	estimateRaw, _ := json.Marshal(agentapi.Usage{TotalTokens: 30})
	actualRaw, _ := json.Marshal(agentapi.Usage{TotalTokens: 20})
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT limits_json,used_usage_json,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"limits_json", "used_usage_json", "reserved_usage_json"}).AddRow(limitsRaw, zeroRaw, estimateRaw))
	mock.ExpectQuery(`SELECT root_run_id,parent_reservation_id,kind,phase,.*FROM agent_run_budget_reservations WHERE reservation_id=\? FOR UPDATE`).
		WithArgs("call-1").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`INSERT INTO agent_run_budget_reservations`).
		WithArgs("call-1", "root-1", "", "call", "default", estimateRaw, zeroRaw, zeroRaw, "open", sqlmock.AnyArg(), nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE agent_run_budget_ledger`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "root-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := backend.Reserve("root-1", durableReservation("call-1", "call", agentapi.Usage{TotalTokens: 30})); err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT parent_reservation_id,kind,state,estimate_usage_json,used_usage_json.*FOR UPDATE`).
		WithArgs("root-1", "call-1").
		WillReturnRows(sqlmock.NewRows([]string{"parent_reservation_id", "kind", "state", "estimate_usage_json", "used_usage_json"}).AddRow("", "call", "open", estimateRaw, zeroRaw))
	mock.ExpectQuery(`SELECT used_usage_json,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"used_usage_json", "reserved_usage_json"}).AddRow(zeroRaw, estimateRaw))
	mock.ExpectExec(`UPDATE agent_run_budget_reservations`).
		WithArgs("settled", actualRaw, sqlmock.AnyArg(), "root-1", "call-1", "open").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE agent_run_budget_ledger`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "root-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT limits_json FROM agent_run_budget_ledger WHERE root_run_id=\?`).
		WithArgs("root-1").WillReturnRows(sqlmock.NewRows([]string{"limits_json"}).AddRow(limitsRaw))
	mock.ExpectCommit()
	if err := backend.SettleCall("root-1", "call-1", agentapi.Usage{TotalTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func durableReservation(id, kind string, usage agentapi.Usage) budget.DurableReservation {
	if kind == "task" {
		return budget.DurableReservation{ID: id, Kind: kind, Grant: usage}
	}
	return budget.DurableReservation{ID: id, Kind: kind, Estimate: usage}
}

func TestSQLBudgetDuplicateReservationMustMatchIdentity(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &sqlBudgetBackend{db: db}
	limitsRaw, _ := json.Marshal(agentapi.RunLimits{MaxTotalTokens: 100})
	zeroRaw, _ := json.Marshal(agentapi.Usage{})
	grantRaw, _ := json.Marshal(agentapi.Usage{TotalTokens: 60})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT limits_json,used_usage_json,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"limits_json", "used_usage_json", "reserved_usage_json"}).AddRow(limitsRaw, zeroRaw, grantRaw))
	mock.ExpectQuery(`SELECT root_run_id,parent_reservation_id,kind,phase,.*FROM agent_run_budget_reservations WHERE reservation_id=\? FOR UPDATE`).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{"root_run_id", "parent_reservation_id", "kind", "phase", "estimate_usage_json", "grant_usage_json", "state"}).AddRow("root-1", "", "task", "default", zeroRaw, grantRaw, "active"))
	mock.ExpectCommit()
	if err := backend.Reserve("root-1", durableReservation("task-1", "task", agentapi.Usage{TotalTokens: 60})); err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT limits_json,used_usage_json,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"limits_json", "used_usage_json", "reserved_usage_json"}).AddRow(limitsRaw, zeroRaw, grantRaw))
	mock.ExpectQuery(`SELECT root_run_id,parent_reservation_id,kind,phase,.*FROM agent_run_budget_reservations WHERE reservation_id=\? FOR UPDATE`).
		WithArgs("task-1").
		WillReturnRows(sqlmock.NewRows([]string{"root_run_id", "parent_reservation_id", "kind", "phase", "estimate_usage_json", "grant_usage_json", "state"}).AddRow("root-1", "", "task", "default", zeroRaw, grantRaw, "active"))
	mock.ExpectRollback()
	if err := backend.Reserve("root-1", durableReservation("task-1", "task", agentapi.Usage{TotalTokens: 61})); err == nil {
		t.Fatal("conflicting duplicate reservation was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLBudgetChildSettleAfterTaskReleaseIsRejected(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &sqlBudgetBackend{db: db}
	zeroRaw, _ := json.Marshal(agentapi.Usage{})
	estimateRaw, _ := json.Marshal(agentapi.Usage{TotalTokens: 10})
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT parent_reservation_id,kind,state,estimate_usage_json,used_usage_json.*FOR UPDATE`).
		WithArgs("root-1", "call-1").
		WillReturnRows(sqlmock.NewRows([]string{"parent_reservation_id", "kind", "state", "estimate_usage_json", "used_usage_json"}).AddRow("task-1", "call", "open", estimateRaw, zeroRaw))
	mock.ExpectQuery(`SELECT used_usage_json,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"used_usage_json", "reserved_usage_json"}).AddRow(zeroRaw, estimateRaw))
	mock.ExpectQuery(`SELECT state,used_usage_json.*FOR UPDATE`).
		WithArgs("root-1", "task-1", "task").
		WillReturnRows(sqlmock.NewRows([]string{"state", "used_usage_json"}).AddRow("released", zeroRaw))
	mock.ExpectRollback()
	if err := backend.SettleCall("root-1", "call-1", agentapi.Usage{TotalTokens: 5}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("late child settle = %v, want budget exceeded", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLBudgetAcquireLeaseRejectsLiveForeignOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &sqlBudgetBackend{db: db}
	now := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	zeroRaw, _ := json.Marshal(agentapi.Usage{})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT lease_owner,lease_expires_at,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"lease_owner", "lease_expires_at", "reserved_usage_json"}).
			AddRow("worker-a", now.Add(time.Minute), zeroRaw))
	mock.ExpectRollback()

	err = backend.AcquireLease("root-1", "worker-b", now, time.Minute)
	if !errors.Is(err, budget.ErrLeaseHeld) {
		t.Fatalf("AcquireLease = %v, want lease held", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLBudgetAcquireLeaseReclaimsExpiredReservations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &sqlBudgetBackend{db: db}
	now := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	reservedRaw, _ := json.Marshal(agentapi.Usage{TotalTokens: 40})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT lease_owner,lease_expires_at,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"lease_owner", "lease_expires_at", "reserved_usage_json"}).
			AddRow("worker-a", now.Add(-time.Second), reservedRaw))
	mock.ExpectExec(`UPDATE agent_run_budget_reservations.*SET state=\?,settled_at=\?.*state IN \(\?,\?\)`).
		WithArgs("released", sqlmock.AnyArg(), "root-1", "open", "active").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE agent_run_budget_ledger.*SET reserved_usage_json=\?,version=version\+1,updated_at=\?.*WHERE root_run_id=\?`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "root-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE agent_run_budget_ledger.*SET lease_owner=\?,lease_expires_at=\?,version=version\+1,updated_at=\?.*WHERE root_run_id=\?`).
		WithArgs("worker-b", sqlmock.AnyArg(), sqlmock.AnyArg(), "root-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := backend.AcquireLease("root-1", "worker-b", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLBudgetRenewLeaseRejectsOwnerMismatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &sqlBudgetBackend{db: db}
	now := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	zeroRaw, _ := json.Marshal(agentapi.Usage{})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT lease_owner,lease_expires_at,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"lease_owner", "lease_expires_at", "reserved_usage_json"}).
			AddRow("worker-a", now.Add(time.Minute), zeroRaw))
	mock.ExpectRollback()

	err = backend.RenewLease("root-1", "worker-b", now, time.Minute)
	if !errors.Is(err, budget.ErrLeaseOwnerMismatch) {
		t.Fatalf("RenewLease = %v, want owner mismatch", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLBudgetReleaseLeaseRejectsActiveReservations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &sqlBudgetBackend{db: db}
	now := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	zeroRaw, _ := json.Marshal(agentapi.Usage{})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT lease_owner,lease_expires_at,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"lease_owner", "lease_expires_at", "reserved_usage_json"}).
			AddRow("worker-a", now.Add(time.Minute), zeroRaw))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM agent_run_budget_reservations.*state IN \(\?,\?\)`).
		WithArgs("root-1", "open", "active").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectRollback()

	err = backend.ReleaseLease("root-1", "worker-a", now)
	if !errors.Is(err, budget.ErrLeaseHasReservations) {
		t.Fatalf("ReleaseLease = %v, want active reservations", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLBudgetReclaimExpiredIsIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend := &sqlBudgetBackend{db: db}
	now := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	reservedRaw, _ := json.Marshal(agentapi.Usage{TotalTokens: 40})
	zeroRaw, _ := json.Marshal(agentapi.Usage{})

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT lease_owner,lease_expires_at,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"lease_owner", "lease_expires_at", "reserved_usage_json"}).
			AddRow("worker-a", now.Add(-time.Second), reservedRaw))
	mock.ExpectExec(`UPDATE agent_run_budget_reservations.*SET state=\?,settled_at=\?.*state IN \(\?,\?\)`).
		WithArgs("released", sqlmock.AnyArg(), "root-1", "open", "active").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE agent_run_budget_ledger.*SET reserved_usage_json=\?,version=version\+1,updated_at=\?.*WHERE root_run_id=\?`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "root-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE agent_run_budget_ledger.*SET lease_owner=\?,lease_expires_at=\?,version=version\+1,updated_at=\?.*WHERE root_run_id=\?`).
		WithArgs("", nil, sqlmock.AnyArg(), "root-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := backend.ReclaimExpired("root-1", now); err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT lease_owner,lease_expires_at,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-1").
		WillReturnRows(sqlmock.NewRows([]string{"lease_owner", "lease_expires_at", "reserved_usage_json"}).
			AddRow("", nil, zeroRaw))
	mock.ExpectCommit()
	if err := backend.ReclaimExpired("root-1", now.Add(time.Minute)); err != nil {
		t.Fatalf("idempotent reclaim = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreNewDurableBudgetAcquiresLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db, durableBudget: true, budgetLeaseOwner: "worker-a"}
	limits := agentapi.RunLimits{MaxTotalTokens: 100, Deadline: time.Now().UTC().Add(2 * time.Minute)}
	limitsRaw, _ := json.Marshal(limits)
	zeroRaw, _ := json.Marshal(agentapi.Usage{})

	expectEnsureRoot(mock, "root-lease", limitsRaw, zeroRaw)
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT lease_owner,lease_expires_at,reserved_usage_json.*FOR UPDATE`).
		WithArgs("root-lease").
		WillReturnRows(sqlmock.NewRows([]string{"lease_owner", "lease_expires_at", "reserved_usage_json"}).
			AddRow("", nil, zeroRaw))
	mock.ExpectExec(`UPDATE agent_run_budget_ledger.*SET lease_owner=\?,lease_expires_at=\?,version=version\+1,updated_at=\?.*WHERE root_run_id=\?`).
		WithArgs("worker-a", sqlmock.AnyArg(), sqlmock.AnyArg(), "root-lease").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	gate, err := store.NewDurableBudget("root-lease", limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gate.(*budget.DurableRoot); !ok {
		t.Fatalf("gate type = %T, want *budget.DurableRoot", gate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDurableBudgetLeaseTTLIncludesDeadlineAndGrace(t *testing.T) {
	now := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	if got := durableBudgetLeaseTTL(time.Time{}, now); got != minimumBudgetLeaseTTL {
		t.Fatalf("zero-deadline ttl = %s, want %s", got, minimumBudgetLeaseTTL)
	}
	if got := durableBudgetLeaseTTL(now.Add(10*time.Second), now); got != minimumBudgetLeaseTTL {
		t.Fatalf("short-deadline ttl = %s, want %s", got, minimumBudgetLeaseTTL)
	}
	want := 5*time.Minute + budgetLeaseGrace
	if got := durableBudgetLeaseTTL(now.Add(5*time.Minute), now); got != want {
		t.Fatalf("deadline ttl = %s, want %s", got, want)
	}
}

func TestCreateWithDurableBudgetIsAtomicWhenLedgerInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs := &Store{db: db, durableBudget: true, fencingEnabled: true, budgetLeaseOwner: "owner-a"}
	record := Record{ID: "run-atomic", AgentID: "qa.answerer", Question: "q", StartedAt: "2026-09-02T10:00:00Z"}
	limits := agentapi.RunLimits{MaxTotalTokens: 100, Deadline: time.Now().UTC().Add(time.Minute)}

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO agent_runs\(`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO agent_run_budget_ledger\(`).WillReturnError(errors.New("duplicate ledger"))
	mock.ExpectRollback()
	if _, err := rs.CreateWithDurableBudgetContext(context.Background(), record, limits); err == nil {
		t.Fatal("ledger failure unexpectedly committed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWithDurableBudgetRollsBackWhenRunInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs := &Store{db: db, durableBudget: true, fencingEnabled: true, budgetLeaseOwner: "owner-a"}
	record := Record{ID: "run-atomic", AgentID: "qa.answerer", Question: "q"}
	limits := agentapi.RunLimits{MaxTotalTokens: 100}

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO agent_runs\(`).WillReturnError(errors.New("duplicate run"))
	mock.ExpectRollback()
	if _, err := rs.CreateWithDurableBudgetContext(context.Background(), record, limits); err == nil {
		t.Fatal("run failure unexpectedly committed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateWithDurableBudgetRollsBackOnCommitFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs := &Store{db: db, durableBudget: true, fencingEnabled: true, budgetLeaseOwner: "owner-a"}
	record := Record{ID: "run-atomic", AgentID: "qa.answerer", Question: "q"}
	limits := agentapi.RunLimits{MaxTotalTokens: 100}

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO agent_runs\(`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO agent_run_budget_ledger\(`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(errors.New("commit unavailable"))
	if _, err := rs.CreateWithDurableBudgetContext(context.Background(), record, limits); err == nil {
		t.Fatal("commit failure unexpectedly succeeded")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
