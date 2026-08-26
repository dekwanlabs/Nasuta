package investigation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryRunStoreRejectsIllegalTransitionsAndDuplicateTerminalWrites(t *testing.T) {
	store := NewMemoryRunStore()
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
	if err := store.SaveDelivery(run.ID, DeliveryResult{Status: DeliveryEvidenceInsufficient, Text: "not enough evidence"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("delivery in executing state error = %v", err)
	}
	if err := store.Fail(run.ID, RunFailure{Code: FailureExecution, Message: "failed"}, RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(run.ID, RunDelivered); !errors.Is(err, ErrTerminalRun) {
		t.Fatalf("terminal transition error = %v", err)
	}
}

func TestMemoryRunStorePersistsDeliveryOnlyFromComposingAndSequencesEvents(t *testing.T) {
	store := NewMemoryRunStore()
	if err := store.Create(InvestigationRun{ID: "run-2", Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	for _, next := range []RunStatus{RunAnalyzing, RunPlanned, RunExecuting, RunVerifying, RunComposing} {
		if err := store.Transition("run-2", next); err != nil {
			t.Fatal(err)
		}
	}
	delivery := DeliveryResult{Status: DeliveryPartial, Text: "partial result"}
	if err := store.SaveDelivery("run-2", delivery); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDelivery("run-2", delivery); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("duplicate delivery error = %v", err)
	}
	events, err := store.Events("run-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("events = %#v", events)
	}
	for index, event := range events {
		want := int64(index + 1)
		if event.Sequence != want || event.RunID != "run-2" {
			t.Fatalf("event[%d] = %#v", index, event)
		}
	}
	run, err := store.Get("run-2")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunDelivered || run.Delivery == nil || run.Delivery.Text != delivery.Text {
		t.Fatalf("persisted run = %#v", run)
	}
}

func TestMemoryRunStoreReturnsDetachedSnapshots(t *testing.T) {
	task := testExecutableTask("task-a", nil)
	run := InvestigationRun{
		ID:       "run-3",
		Status:   RunCreated,
		Contract: testContract(EvidenceGoal{ID: "g1", Kind: "flow", Facets: []string{"entry"}}),
		Tasks:    map[string]ExecutableTask{task.ID: task},
		Budget:   BudgetSnapshot{Stages: map[BudgetStage]StageBudget{StageExecution: {Limit: BudgetVector{ToolCalls: 2}}}},
	}
	store := NewMemoryRunStore()
	if err := store.Create(run); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	first.Contract.EvidenceGoals[0].Facets[0] = "changed"
	first.Tasks[task.ID] = testExecutableTask("different", nil)
	first.Budget.Stages[StageExecution] = StageBudget{Limit: BudgetVector{ToolCalls: 99}}
	second, err := store.Get(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Contract.EvidenceGoals[0].Facets[0] != "entry" || second.Tasks[task.ID].ID != task.ID || second.Budget.Stages[StageExecution].Limit.ToolCalls != 2 {
		t.Fatalf("snapshot was not detached: %#v", second)
	}
}

func TestFencedRunStoreRejectsStaleOwner(t *testing.T) {
	lease := NewMemoryLeaseStore()
	base := NewMemoryRunStore()
	run := InvestigationRun{
		ID: "fenced-run", Status: RunCreated,
		Contract: InvestigationContract{
			ID: "fenced-run", Version: InvestigationContractVersion,
		},
	}
	if err := base.Create(run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := lease.AcquireLease(context.Background(), "fenced-run", "owner-a", time.Minute); err != nil {
		t.Fatalf("acquire owner-a: %v", err)
	}
	bound := bindLeaseRunStore(base, lease, "fenced-run", "owner-a")
	if err := lease.ReleaseLease(context.Background(), "fenced-run", "owner-a"); err != nil {
		t.Fatalf("release owner-a: %v", err)
	}
	if err := lease.AcquireLease(context.Background(), "fenced-run", "owner-b", time.Minute); err != nil {
		t.Fatalf("acquire owner-b: %v", err)
	}
	if err := bound.SaveBudget("fenced-run", BudgetSnapshot{}); !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("stale write error = %v, want ErrLeaseFenced", err)
	}
}

func TestMemoryRunStoreListActiveRunsUsesStableStorageCursor(t *testing.T) {
	store := NewMemoryRunStore()
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	statuses := map[string]RunStatus{
		"run-a": RunExecuting,
		"run-b": RunDelivered,
		"run-c": RunPlanned,
		"run-d": RunVerifying,
		"run-e": RunExecuting,
	}
	for id := range statuses {
		if err := store.Create(InvestigationRun{ID: id, Status: RunCreated}); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	for id, status := range statuses {
		run := store.runs[id]
		run.Status = status
		run.UpdatedAt = base
		if id == "run-d" {
			run.UpdatedAt = base.Add(time.Millisecond)
		}
		if id == "run-e" {
			run.UpdatedAt = base.Add(2 * time.Hour)
		}
		store.runs[id] = run
	}
	store.mu.Unlock()

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

func TestMemoryRunStoreListActiveRunsRejectsIncompleteCursor(t *testing.T) {
	store := NewMemoryRunStore()
	_, err := store.ListActiveRuns(time.Now().UTC(), ActiveRunCursor{ID: "run-a"}, 1)
	if err == nil {
		t.Fatal("incomplete cursor was accepted")
	}
}

func TestFencedRunStoreRejectsStaleDelivery(t *testing.T) {
	lease := NewMemoryLeaseStore()
	base := NewMemoryRunStore()
	runID := "fenced-delivery"
	if err := base.Create(InvestigationRun{ID: runID, Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{RunAnalyzing, RunPlanned, RunExecuting, RunVerifying, RunComposing} {
		if err := base.Transition(runID, status); err != nil {
			t.Fatal(err)
		}
	}
	first, err := lease.AcquireLeaseWithToken(t.Context(), runID, "owner-a", 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	stale := bindLeaseRunStore(base, lease, runID, first.Owner, first.Token)
	time.Sleep(10 * time.Millisecond)
	if _, err := lease.AcquireLeaseWithToken(t.Context(), runID, "owner-b", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := stale.SaveDelivery(runID, DeliveryResult{Status: DeliveryPartial, Text: "stale"}); !errors.Is(err, ErrLeaseFenced) {
		t.Fatalf("stale delivery error = %v, want ErrLeaseFenced", err)
	}
	run, err := base.Get(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunComposing || run.Delivery != nil {
		t.Fatalf("stale delivery mutated run: %#v", run)
	}
}
