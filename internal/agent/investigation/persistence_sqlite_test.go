package investigation

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openSQLiteRunStore(t *testing.T) *SQLiteRunStore {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewSQLiteRunStore(db)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSQLiteRunStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewSQLiteRunStore(db)
	if err != nil {
		t.Fatal(err)
	}
	task := testExecutableTask("task-a", nil)
	run := InvestigationRun{
		ID:     "run-1",
		Status: RunCreated,
		Tasks:  map[string]ExecutableTask{task.ID: task},
		Budget: BudgetSnapshot{Stages: map[BudgetStage]StageBudget{StageExecution: {Limit: BudgetVector{ToolCalls: 2}}}},
	}
	if err := store.Create(run); err != nil {
		t.Fatal(err)
	}
	for _, next := range []RunStatus{RunAnalyzing, RunPlanned} {
		if err := store.Transition(run.ID, next); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SavePlan(run.ID, PlanRevision{Revision: 1, ContractID: "contract", Tasks: []ExecutableTask{task}}); err != nil {
		t.Fatal(err)
	}
	for _, next := range []RunStatus{RunExecuting, RunVerifying, RunComposing} {
		if err := store.Transition(run.ID, next); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveDelivery(run.ID, DeliveryResult{Status: DeliveryPartial, Text: "partial"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	store, err = NewSQLiteRunStore(reopened)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != RunDelivered || loaded.Delivery == nil || loaded.Delivery.Text != "partial" {
		t.Fatalf("reopened run = %#v", loaded)
	}
	if loaded.Tasks[task.ID].ID != task.ID || loaded.Budget.Stages[StageExecution].Limit.ToolCalls != 2 {
		t.Fatalf("reopened run lost task or budget: %#v", loaded)
	}
	events, err := store.Events(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[0].Type != "run_created" {
		t.Fatalf("reopened events = %#v", events)
	}
}

func TestSQLiteRunStoreRejectsIllegalTransitionsAndDuplicateTerminalWrites(t *testing.T) {
	store := openSQLiteRunStore(t)
	task := testExecutableTask("task-a", nil)
	run := InvestigationRun{
		ID:     "run-1",
		Status: RunCreated,
		Tasks:  map[string]ExecutableTask{task.ID: task},
		Budget: BudgetSnapshot{Stages: map[BudgetStage]StageBudget{StageExecution: {Limit: BudgetVector{ToolCalls: 1}}}},
	}
	if err := store.Create(run); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(run.ID, RunBudgetExhausted); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("illegal transition error = %v", err)
	}
	for _, next := range []RunStatus{RunAnalyzing, RunPlanned, RunExecuting} {
		if err := store.Transition(run.ID, next); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SavePlan(run.ID, PlanRevision{Revision: 1, ContractID: "contract", Tasks: []ExecutableTask{task}}); err != nil {
		t.Fatal(err)
	}
	record := TaskExecutionRecord{TaskID: task.ID, Status: TaskSucceeded, Output: []byte(`{"ok":true}`)}
	if err := store.SaveResult(run.ID, record); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveResult(run.ID, record); err == nil {
		t.Fatal("duplicate terminal task result was accepted")
	}
	if err := store.SaveDelivery(run.ID, DeliveryResult{Status: DeliveryEvidenceInsufficient, Text: "not enough"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("delivery in executing state error = %v", err)
	}
	if err := store.Fail(run.ID, RunFailure{Code: FailureExecution, Message: "failed"}, RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(run.ID, RunDelivered); !errors.Is(err, ErrTerminalRun) {
		t.Fatalf("terminal transition error = %v", err)
	}
}

func TestSQLiteRunStoreListActiveRunsPaginatesAcrossTerminalRows(t *testing.T) {
	store := openSQLiteRunStore(t)
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	insert := func(id string, status RunStatus, updatedAt time.Time) {
		t.Helper()
		payload, err := json.Marshal(InvestigationRun{ID: id, Status: status})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(
			`INSERT INTO investigation_runs (id, payload, updated_at) VALUES (?, ?, ?)`,
			id, string(payload), updatedAt.UnixMilli(),
		); err != nil {
			t.Fatal(err)
		}
	}
	insert("run-a", RunExecuting, base)
	insert("run-b", RunDelivered, base)
	insert("run-c", RunPlanned, base)
	insert("run-d", RunVerifying, base.Add(time.Millisecond))
	insert("run-e", RunExecuting, base.Add(2*time.Hour))

	first, err := store.ListActiveRuns(base.Add(time.Hour), ActiveRunCursor{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Runs) != 1 || first.Runs[0].ID != "run-a" || !first.HasMore || first.Next.ID != "run-b" {
		t.Fatalf("first page = %#v", first)
	}
	second, err := store.ListActiveRuns(base.Add(time.Hour), first.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Runs) != 2 || second.Runs[0].ID != "run-c" || second.Runs[1].ID != "run-d" || second.HasMore {
		t.Fatalf("second page = %#v", second)
	}
}

func TestSQLiteRunStoreListActiveRunsWrapsDecodeFailure(t *testing.T) {
	store := openSQLiteRunStore(t)
	updatedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	if _, err := store.db.Exec(
		`INSERT INTO investigation_runs (id, payload, updated_at) VALUES (?, ?, ?)`,
		"run-corrupt", "{", updatedAt.UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}
	_, err := store.ListActiveRuns(time.Now().UTC(), ActiveRunCursor{}, 1)
	if err == nil || !strings.Contains(err.Error(), `decode run "run-corrupt"`) {
		t.Fatalf("decode error = %v", err)
	}
}
