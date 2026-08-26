package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorResumeRejectsUnsupportedContractVersion(t *testing.T) {
	store := NewMemoryRunStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.Version = 2
	if err := store.Create(InvestigationRun{
		ID: "run-old-contract", Contract: contract, Status: RunCreated,
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{Store: store})

	_, err := coordinator.Resume(t.Context(), "run-old-contract")
	if !errors.Is(err, ErrPlanInvalid) || !strings.Contains(err.Error(), "current version is 1") {
		t.Fatalf("Resume error = %v", err)
	}
}

func TestCoordinatorResumeSkipsCompletedTasks(t *testing.T) {
	store := NewMemoryRunStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.ParentRunID = "run-parent"
	contract.Actor.UserID = 42
	contract.Actor.TenantID = "tenant-a"
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

	var executionInput TaskExecutionInput
	coordinator := NewCoordinator(CoordinatorOptions{
		Schemas: testSchemas(),
		Store:   store,
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, input TaskExecutionInput) (TaskExecutionResult, error) {
			executionInput = input
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
	if executionInput.WorkflowRunID != run.ID || executionInput.ParentRunID != contract.ParentRunID ||
		executionInput.Actor != contract.Actor || executionInput.Attempt != 1 {
		t.Fatalf("resume execution input identity = %#v", executionInput)
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

func TestCoordinatorConcurrentResumeUsesSingleLeaseOwner(t *testing.T) {
	store := NewMemoryRunStore()
	lease := NewMemoryLeaseStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	if err := store.Create(InvestigationRun{ID: "run-concurrent-resume", Contract: contract, Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{RunAnalyzing, RunPlanned, RunExecuting} {
		if err := store.Transition("run-concurrent-resume", status); err != nil {
			t.Fatal(err)
		}
	}
	task := testExecutableTask("task-blocked", nil)
	if err := store.SavePlan("run-concurrent-resume", PlanRevision{Revision: 1, ContractID: contract.ID, Tasks: []ExecutableTask{task}}); err != nil {
		t.Fatal(err)
	}
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{ToolCalls: 1, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBudget("run-concurrent-resume", ledger.Snapshot()); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	executor := TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1}}, nil
	})
	options := CoordinatorOptions{
		Schemas: testSchemas(), Store: store, Lease: lease,
		Executors: testExecutors(executor), BudgetLimit: BudgetVector{ToolCalls: 1, Duration: time.Minute}, MaxRounds: 1,
	}
	first := NewCoordinator(options)
	second := NewCoordinator(options)
	firstDone := make(chan error, 1)
	go func() {
		_, err := first.Resume(t.Context(), "run-concurrent-resume")
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first resume did not start")
	}
	if _, err := second.Resume(t.Context(), "run-concurrent-resume"); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second resume error = %v, want ErrLeaseHeld", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first resume: %v", err)
	}
}

func TestCoordinatorResumeConvergesVerificationStages(t *testing.T) {
	for _, status := range []RunStatus{RunVerifying, RunReplanning, RunComposing} {
		t.Run(string(status), func(t *testing.T) {
			store := NewMemoryRunStore()
			runID := "run-resume-" + string(status)
			contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
			contract.ID = runID
			contract.CreatedAt = time.Now().UTC()
			if err := store.Create(InvestigationRun{ID: runID, Contract: contract, Status: RunCreated}); err != nil {
				t.Fatal(err)
			}
			for _, next := range []RunStatus{RunAnalyzing, RunPlanned} {
				if err := store.Transition(runID, next); err != nil {
					t.Fatal(err)
				}
			}
			task := testExecutableTask("task-finished", nil)
			if err := store.SavePlan(runID, PlanRevision{Revision: 1, ContractID: contract.ID, Tasks: []ExecutableTask{task}}); err != nil {
				t.Fatal(err)
			}
			if err := store.Transition(runID, RunExecuting); err != nil {
				t.Fatal(err)
			}
			if err := store.SaveResult(runID, TaskExecutionRecord{TaskID: task.ID, Status: TaskSucceeded, Output: json.RawMessage(`{"ok":true}`)}); err != nil {
				t.Fatal(err)
			}
			if err := store.Transition(runID, RunVerifying); err != nil {
				t.Fatal(err)
			}
			if status == RunReplanning || status == RunComposing {
				if err := store.SaveReport(runID, InvestigationReport{}); err != nil {
					t.Fatal(err)
				}
				if status == RunReplanning {
					if err := store.Transition(runID, RunReplanning); err != nil {
						t.Fatal(err)
					}
				} else if err := store.Transition(runID, RunComposing); err != nil {
					t.Fatal(err)
				}
			}
			coordinator := NewCoordinator(CoordinatorOptions{Store: store, Schemas: testSchemas()})
			run, err := coordinator.Resume(t.Context(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != RunDelivered || run.Delivery == nil {
				t.Fatalf("run = %#v", run)
			}
		})
	}
}

func TestCoordinatorResumeRejectsMissingVerifierExecution(t *testing.T) {
	store := NewMemoryRunStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.ID = "contract-resume-missing-verifier"
	runID := "run-resume-missing-verifier"
	if err := store.Create(InvestigationRun{ID: runID, Contract: contract, Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{RunAnalyzing, RunPlanned} {
		if err := store.Transition(runID, status); err != nil {
			t.Fatal(err)
		}
	}
	investigator := ExecutableTask{ID: "investigate", Executor: ExecutorInvestigator, Status: TaskSucceeded}
	verifier := ExecutableTask{
		ID: "evidence.verify", Executor: ExecutorVerifier, Status: TaskPending, Dependencies: []string{investigator.ID},
	}
	if err := store.SavePlan(runID, PlanRevision{
		Revision: 1, ContractID: contract.ID, Tasks: []ExecutableTask{investigator, verifier},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(runID, RunExecuting); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveResult(runID, TaskExecutionRecord{
		TaskID: investigator.ID, Status: TaskSucceeded, Output: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(runID, RunVerifying); err != nil {
		t.Fatal(err)
	}
	ledger, err := NewBudgetLedger(BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBudget(runID, ledger.Snapshot()); err != nil {
		t.Fatal(err)
	}
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("evidence.verify", 1, []string{"flow"}, ExecutorVerifier, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{Catalog: catalog, Store: store, Schemas: testSchemas()})

	run, err := coordinator.Resume(t.Context(), runID)
	if err == nil || run.Status != RunFailed || run.Failure == nil || run.Failure.Code != FailureVerifier {
		t.Fatalf("run = %#v, err = %v", run, err)
	}
	if !strings.Contains(run.Failure.Message, "did not execute") {
		t.Fatalf("failure = %#v", run.Failure)
	}
}
