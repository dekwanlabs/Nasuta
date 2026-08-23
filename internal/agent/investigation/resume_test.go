package investigation

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestCoordinatorResumeSkipsCompletedTasks(t *testing.T) {
	store := NewMemoryRunStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	if err := store.Create(InvestigationRun{ID: "run-resume", Contract: contract, Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{RunAnalyzing, RunPlanned, RunExecuting} {
		if err := store.Transition("run-resume", status); err != nil {
			t.Fatal(err)
		}
	}
	first := testExecutableTask("task-first", nil)
	second := testExecutableTask("task-second", nil)
	if err := store.SavePlan("run-resume", PlanRevision{
		Revision: 1, ContractID: contract.ID,
		Tasks: []ExecutableTask{first, second},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveResult("run-resume", TaskExecutionRecord{
		TaskID: first.ID, Status: TaskSucceeded,
		Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1},
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{ToolCalls: 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBudget("run-resume", ledger.Snapshot()); err != nil {
		t.Fatal(err)
	}

	coordinator := NewCoordinator(CoordinatorOptions{
		Schemas: testSchemas(),
		Store:   store,
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1}}, nil
		})),
		BudgetLimit: BudgetVector{ToolCalls: 2},
		MaxRounds:   1,
	})
	run, err := coordinator.Resume(context.Background(), "run-resume")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunDelivered || len(run.Results) != 2 {
		t.Fatalf("resumed run = %#v", run)
	}
	if run.Metrics.Rounds != 1 || run.Metrics.Tasks != 2 {
		t.Fatalf("resumed metrics = %#v", run.Metrics)
	}
	if run.Results["task-first"].Status != TaskSucceeded ||
		run.Results["task-second"].Status != TaskSucceeded {
		t.Fatalf("resumed results = %#v", run.Results)
	}
	events, err := store.Events("run-resume")
	if err != nil {
		t.Fatal(err)
	}
	var taskCompleted bool
	var deliveryCompleted bool
	for _, event := range events {
		if event.Type == "delivery_completed" {
			deliveryCompleted = true
		}
		if event.Type != "task_completed" {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(event.Message), &fields); err != nil {
			t.Fatalf("decode task event: %v", err)
		}
		if fields["task_id"] == second.ID && fields["executor"] == string(second.Executor) {
			taskCompleted = true
		}
	}
	if !taskCompleted {
		t.Fatalf("resume task completion event missing: %#v", events)
	}
	if !deliveryCompleted {
		t.Fatalf("resume delivery completion event missing: %#v", events)
	}
}
