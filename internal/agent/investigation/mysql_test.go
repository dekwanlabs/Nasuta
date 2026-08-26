package investigation

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMySQLRunStoreCreatesAndLoadsRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := NewMySQLRunStore(db)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO investigation_runs (id, payload, updated_at) VALUES (?, ?, ?)")).
		WithArgs("run-1", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO investigation_events (run_id, type, status, message, created_at) VALUES (?, ?, ?, ?, ?)")).
		WithArgs("run-1", "run_created", "created", "run created", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	task := testExecutableTask("task-a", nil)
	run := InvestigationRun{
		ID:        "run-1",
		Status:    RunCreated,
		CreatedAt: time.Now().UTC(),
		Tasks:     map[string]ExecutableTask{task.ID: task},
		Budget: BudgetSnapshot{Stages: map[BudgetStage]StageBudget{
			StageExecution: {Limit: BudgetVector{ToolCalls: 2}},
		}},
	}
	if err := store.Create(run); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRunStoreTransitionsAndRejectsTerminalRewrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := NewMySQLRunStore(db)
	if err != nil {
		t.Fatal(err)
	}

	// Get after a terminal failure must report the persisted snapshot, not allow
	// another transition.
	payload := `{"id":"run-1","status":"failed","tasks":{},"results":{},"budget":{"stages":{}}}`
	mock.ExpectQuery(regexp.QuoteMeta("SELECT payload FROM investigation_runs WHERE id = ?")).
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow(payload))

	if err := store.Transition("run-1", RunDelivered); !errors.Is(err, ErrTerminalRun) {
		t.Fatalf("terminal transition error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLeaseStoreAcquireTakeoverIncrementsFencingToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := NewMySQLLeaseStore(db)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner, expires_at, fencing_token FROM investigation_leases WHERE run_id = ? FOR UPDATE")).
		WithArgs("run-lease").
		WillReturnRows(sqlmock.NewRows([]string{"owner", "expires_at", "fencing_token"}).AddRow("owner-old", time.Now().Add(-time.Minute).UnixMilli(), uint64(41)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE investigation_leases SET owner = ?, expires_at = ?, fencing_token = ? WHERE run_id = ?")).
		WithArgs("owner-new", sqlmock.AnyArg(), uint64(42), "run-lease").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	lease, err := store.AcquireLeaseWithToken(t.Context(), "run-lease", "owner-new", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Token != 42 {
		t.Fatalf("takeover token = %d, want 42", lease.Token)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRunStoreFencedUpdateRejectsStaleRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := NewMySQLRunStore(db)
	if err != nil {
		t.Fatal(err)
	}

	payload := `{"id":"run-fenced","status":"created","tasks":{},"results":{},"budget":{"stages":{}}}`
	mock.ExpectQuery(regexp.QuoteMeta("SELECT payload FROM investigation_runs WHERE id = ? AND fencing_token = ?")).
		WithArgs("run-fenced", uint64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow(payload))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fencing_token, expires_at FROM investigation_leases WHERE run_id = ? AND owner = ? FOR UPDATE")).
		WithArgs("run-fenced", "owner-a").
		WillReturnRows(sqlmock.NewRows([]string{"fencing_token", "expires_at"}).AddRow(uint64(7), time.Now().Add(time.Minute).UnixMilli()))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE investigation_runs SET payload = ?, updated_at = ? WHERE id = ? AND fencing_token = ? AND EXISTS (SELECT 1 FROM investigation_leases WHERE run_id = ? AND owner = ? AND fencing_token = ? AND expires_at > ?)")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "run-fenced", uint64(7), "run-fenced", "owner-a", uint64(7), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	err = store.applyFenced("run-fenced", "owner-a", 7, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateTransition(run, RunAnalyzing)
	})
	if !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("fenced update error = %v, want ErrLeaseFenced", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRunStoreFencedUpdateUsesCurrentOwnerTokenAndExpiry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store, err := NewMySQLRunStore(db)
	if err != nil {
		t.Fatal(err)
	}

	payload := `{"id":"run-fenced","status":"created","tasks":{},"results":{},"budget":{"stages":{}}}`
	mock.ExpectQuery(regexp.QuoteMeta("SELECT payload FROM investigation_runs WHERE id = ? AND fencing_token = ?")).
		WithArgs("run-fenced", uint64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow(payload))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fencing_token, expires_at FROM investigation_leases WHERE run_id = ? AND owner = ? FOR UPDATE")).
		WithArgs("run-fenced", "owner-current").
		WillReturnRows(sqlmock.NewRows([]string{"fencing_token", "expires_at"}).AddRow(uint64(9), time.Now().Add(time.Minute).UnixMilli()))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE investigation_runs SET payload = ?, updated_at = ? WHERE id = ? AND fencing_token = ? AND EXISTS (SELECT 1 FROM investigation_leases WHERE run_id = ? AND owner = ? AND fencing_token = ? AND expires_at > ?)")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "run-fenced", uint64(9), "run-fenced", "owner-current", uint64(9), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO investigation_events (run_id, type, status, message, created_at) VALUES (?, ?, ?, ?, ?)")).
		WithArgs("run-fenced", "run_status_changed", "analyzing", "analyzing", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.applyFenced("run-fenced", "owner-current", 9, func(run InvestigationRun) (InvestigationRun, RunEvent, error) {
		return mutateTransition(run, RunAnalyzing)
	}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRunStoreFencedAppendEventDoesNotRewriteSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runStore, err := NewMySQLRunStore(db)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := NewMySQLLeaseStore(db)
	if err != nil {
		t.Fatal(err)
	}
	store := bindLeaseRunStore(runStore, leaseStore, "run-event", "owner-current", 9)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fencing_token, expires_at FROM investigation_leases WHERE run_id = ? AND owner = ? FOR UPDATE")).
		WithArgs("run-event", "owner-current").
		WillReturnRows(sqlmock.NewRows([]string{"fencing_token", "expires_at"}).AddRow(uint64(9), time.Now().Add(time.Minute).UnixMilli()))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fencing_token FROM investigation_runs WHERE id = ? FOR UPDATE")).
		WithArgs("run-event").
		WillReturnRows(sqlmock.NewRows([]string{"fencing_token"}).AddRow(uint64(9)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO investigation_events (run_id, type, status, message, created_at) VALUES (?, ?, ?, ?, ?)")).
		WithArgs("run-event", "plan_compiled", "", `{"revision":1}`, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.AppendEvent("run-event", "plan_compiled", `{"revision":1}`); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRunStoreFencedAppendEventRejectsStaleSnapshotToken(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runStore, err := NewMySQLRunStore(db)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := NewMySQLLeaseStore(db)
	if err != nil {
		t.Fatal(err)
	}
	store := bindLeaseRunStore(runStore, leaseStore, "run-event", "owner-current", 9)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fencing_token, expires_at FROM investigation_leases WHERE run_id = ? AND owner = ? FOR UPDATE")).
		WithArgs("run-event", "owner-current").
		WillReturnRows(sqlmock.NewRows([]string{"fencing_token", "expires_at"}).AddRow(uint64(9), time.Now().Add(time.Minute).UnixMilli()))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fencing_token FROM investigation_runs WHERE id = ? FOR UPDATE")).
		WithArgs("run-event").
		WillReturnRows(sqlmock.NewRows([]string{"fencing_token"}).AddRow(uint64(10)))
	mock.ExpectRollback()

	err = store.AppendEvent("run-event", "plan_compiled", `{"revision":1}`)
	if !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("append event error = %v, want ErrLeaseFenced", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRunStoreFencedAppendEventRejectsExpiredLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runStore, err := NewMySQLRunStore(db)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := NewMySQLLeaseStore(db)
	if err != nil {
		t.Fatal(err)
	}
	store := bindLeaseRunStore(runStore, leaseStore, "run-event", "owner-current", 9)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fencing_token, expires_at FROM investigation_leases WHERE run_id = ? AND owner = ? FOR UPDATE")).
		WithArgs("run-event", "owner-current").
		WillReturnRows(sqlmock.NewRows([]string{"fencing_token", "expires_at"}).AddRow(uint64(9), time.Now().Add(-time.Minute).UnixMilli()))
	mock.ExpectRollback()

	err = store.AppendEvent("run-event", "plan_compiled", `{"revision":1}`)
	if !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("append event error = %v, want ErrLeaseFenced", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRunStoreListActiveRunsUsesBoundedKeysetQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewMySQLRunStore(db)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	cursor := ActiveRunCursor{UpdatedAt: base, ID: "run-a"}
	query := regexp.QuoteMeta("SELECT id, payload, updated_at FROM investigation_runs WHERE updated_at < ? AND (updated_at > ? OR (updated_at = ? AND id > ?)) ORDER BY updated_at ASC, id ASC LIMIT ?")
	mock.ExpectQuery(query).
		WithArgs(base.Add(time.Hour).UnixMilli(), base.UnixMilli(), base.UnixMilli(), cursor.ID, 3).
		WillReturnRows(sqlmock.NewRows([]string{"id", "payload", "updated_at"}).
			AddRow("run-b", `{"id":"run-b","status":"delivered"}`, base.Add(time.Millisecond).UnixMilli()).
			AddRow("run-c", `{"id":"run-c","status":"executing"}`, base.Add(2*time.Millisecond).UnixMilli()).
			AddRow("run-d", `{"id":"run-d","status":"planned"}`, base.Add(3*time.Millisecond).UnixMilli()))

	page, err := store.ListActiveRuns(base.Add(time.Hour), cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Runs) != 1 || page.Runs[0].ID != "run-c" || !page.HasMore || page.Next.ID != "run-c" {
		t.Fatalf("page = %#v", page)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRunStoreAdoptsCurrentLeaseFencingTokenForRecovery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewMySQLRunStore(db)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fencing_token, expires_at FROM investigation_leases WHERE run_id = ? AND owner = ? FOR UPDATE")).
		WithArgs("run-recover", "owner-new").
		WillReturnRows(sqlmock.NewRows([]string{"fencing_token", "expires_at"}).AddRow(uint64(12), time.Now().Add(time.Minute).UnixMilli()))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM investigation_runs WHERE id = ? FOR UPDATE")).
		WithArgs("run-recover").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run-recover"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE investigation_runs SET fencing_token = ? WHERE id = ?")).
		WithArgs(uint64(12), "run-recover").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.adoptFencingToken("run-recover", "owner-new", 12); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLeaseStoreRevocationAdvancesTokenRegardlessOfOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewMySQLLeaseStore(db)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT fencing_token FROM investigation_leases WHERE run_id = ? FOR UPDATE")).
		WithArgs("run-cancel").
		WillReturnRows(sqlmock.NewRows([]string{"fencing_token"}).AddRow(uint64(8)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE investigation_leases SET owner = ?, expires_at = ?, fencing_token = ? WHERE run_id = ?")).
		WithArgs("cancel-owner", sqlmock.AnyArg(), uint64(9), "run-cancel").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	grant, err := store.RevokeLeaseWithToken(t.Context(), "run-cancel", "cancel-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Token != 9 || grant.Owner != "cancel-owner" {
		t.Fatalf("grant = %#v", grant)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
