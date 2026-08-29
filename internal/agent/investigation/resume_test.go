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
			for _, key := range []string{
				"run_id", "budget_boundary", "budget_dimension", "run_limit",
				"run_reserved", "run_used", "run_remaining", "task_reserved",
				"task_used", "requested_output", "effective_output", "input_tokens",
				"failure_code", "completion_status", "evidence_candidate_count",
			} {
				if _, ok := fields[key]; !ok {
					t.Fatalf("task completion event missing budget field %q: %#v", key, fields)
				}
			}
			if fields["run_id"] != "run-resume" || fields["budget_boundary"] != "run" ||
				fields["completion_status"] != string(TaskSucceeded) {
				t.Fatalf("task completion event identity = %#v", fields)
			}
		}
	}
	if !taskCompleted {
		t.Fatalf("resume task completion event missing: %#v", events)
	}
	if !deliveryCompleted {
		t.Fatalf("resume delivery completion event missing: %#v", events)
	}
}

func TestCoordinatorResumeDropsStaleBudgetReservations(t *testing.T) {
	store := NewMemoryRunStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.ID = "contract-resume-stale-budget"
	runID := "run-resume-stale-budget"
	if err := store.Create(InvestigationRun{ID: runID, Contract: contract, Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{RunAnalyzing, RunPlanned, RunExecuting} {
		if err := store.Transition(runID, status); err != nil {
			t.Fatal(err)
		}
	}
	task := testExecutableTask("pending-task", nil)
	if err := store.SavePlan(runID, PlanRevision{
		Revision: 1, ContractID: contract.ID, Tasks: []ExecutableTask{task},
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
	if _, err := ledger.Reserve(StageExecution, "dead-process-task", BudgetVector{ToolCalls: 2}); err != nil {
		t.Fatal(err)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Reserved.ToolCalls != 2 || snapshot.Stages[StageExecution].Reserved.ToolCalls != 2 {
		t.Fatalf("test snapshot has no stale reservation: %#v", snapshot)
	}
	if err := store.SaveBudget(runID, snapshot); err != nil {
		t.Fatal(err)
	}

	executed := false
	coordinator := NewCoordinator(CoordinatorOptions{
		Store:   store,
		Schemas: testSchemas(),
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			executed = true
			return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1}}, nil
		})),
	})
	run, err := coordinator.Resume(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !executed || run.Status != RunDelivered {
		t.Fatalf("resumed run = %#v, executed=%v", run, executed)
	}
	if run.Budget.Run.Used.ToolCalls != 1 || run.Budget.Run.Reserved.ToolCalls != 0 {
		t.Fatalf("run budget = %#v, want used=1 reserved=0", run.Budget.Run)
	}
	if stage := run.Budget.Stages[StageExecution]; stage.Used.ToolCalls != 1 || stage.Reserved.ToolCalls != 0 {
		t.Fatalf("execution budget = %#v, want used=1 reserved=0", stage)
	}
}

func TestCoordinatorResumePassesPartialInvestigatorOutputToVerifier(t *testing.T) {
	store := NewMemoryRunStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.ID = "contract-resume-partial-output"
	runID := "run-resume-partial-output"
	contract.CreatedAt = time.Now().UTC()
	if err := store.Create(InvestigationRun{ID: runID, Contract: contract, Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{RunAnalyzing, RunPlanned, RunExecuting} {
		if err := store.Transition(runID, status); err != nil {
			t.Fatal(err)
		}
	}

	investigator := testExecutableTask("investigator", nil)
	investigator.Executor = ExecutorInvestigator
	verifier := testExecutableTask("evidence.verify", []string{investigator.ID})
	verifier.Executor = ExecutorVerifier
	if err := store.SavePlan(runID, PlanRevision{
		Revision: 1, ContractID: contract.ID, Tasks: []ExecutableTask{investigator, verifier},
	}); err != nil {
		t.Fatal(err)
	}

	evidenceCandidate := EvidenceCandidate{
		SourceKind: "code", Target: "service-a", Content: "the flow enters service-a",
	}
	unit, err := normalizeEvidence(investigator.ID, evidenceCandidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveEvidence(runID, []EvidenceUnit{unit}); err != nil {
		t.Fatal(err)
	}
	partialOutput := json.RawMessage(`{"summary":"partial investigator report"}`)
	if err := store.SaveResult(runID, TaskExecutionRecord{
		TaskID: investigator.ID, Status: TaskPartial, Output: partialOutput,
		Failure: &RunFailure{Code: FailureTimeout, Message: "investigator stopped after collecting evidence", TaskID: investigator.ID},
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1, Duration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{ToolCalls: 1, Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBudget(runID, ledger.Snapshot()); err != nil {
		t.Fatal(err)
	}

	claim := ClaimCandidate{
		GoalID: "g1", Text: "the flow enters service-a", Status: ClaimSupported,
		EvidenceRefs: []EvidenceRef{{
			EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target,
			ContentHash: unit.ContentHash,
		}},
	}
	var verifierInput TaskExecutionInput
	coordinator := NewCoordinator(CoordinatorOptions{
		Store:   store,
		Schemas: testSchemas(),
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, task ExecutableTask, input TaskExecutionInput) (TaskExecutionResult, error) {
			if task.ID != verifier.ID {
				t.Fatalf("unexpected resumed task: %s", task.ID)
			}
			verifierInput = input
			return TaskExecutionResult{Output: json.RawMessage(`{"verified":true}`), Claims: []ClaimCandidate{claim}}, nil
		})),
	})

	run, err := coordinator.Resume(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunDelivered || run.Delivery == nil || run.Delivery.Status != DeliverySucceeded {
		t.Fatalf("resumed run = %#v", run)
	}
	if got := string(verifierInput.Upstream[investigator.ID]); got != string(partialOutput) {
		t.Fatalf("verifier upstream investigator output = %q, want %q", got, partialOutput)
	}
	if len(verifierInput.Evidence) != 1 || verifierInput.Evidence[0].ID != unit.ID {
		t.Fatalf("verifier evidence = %#v", verifierInput.Evidence)
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

func TestCoordinatorResumeStopsBeforeSchedulingSeededBudgetFailure(t *testing.T) {
	store := NewMemoryRunStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.ID = "resume-seeded-budget"
	if err := store.Create(InvestigationRun{
		ID: "run-resume-seeded-budget", Contract: contract, Status: RunCreated,
	}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{RunAnalyzing, RunPlanned, RunExecuting} {
		if err := store.Transition("run-resume-seeded-budget", status); err != nil {
			t.Fatal(err)
		}
	}
	task := ExecutableTask{
		ID: "optional-investigator", Executor: ExecutorInvestigator, Optional: true, Status: TaskPartial,
	}
	if err := store.SavePlan("run-resume-seeded-budget", PlanRevision{
		Revision: 1, ContractID: contract.ID, Tasks: []ExecutableTask{task},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveResult("run-resume-seeded-budget", TaskExecutionRecord{
		TaskID: task.ID, Status: TaskPartial,
		Output:  json.RawMessage(`{"partial":true}`),
		Failure: &RunFailure{Code: FailureBudget, Message: "shared budget exhausted", TaskID: task.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBudget("run-resume-seeded-budget", BudgetSnapshot{
		Run: RunBudget{
			Limit: BudgetVector{OutputTokens: 10},
			Used:  BudgetVector{OutputTokens: 10},
		},
	}); err != nil {
		t.Fatal(err)
	}

	executed := false
	coordinator := NewCoordinator(CoordinatorOptions{
		Store:   store,
		Schemas: testSchemas(),
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			executed = true
			return TaskExecutionResult{Output: json.RawMessage(`{"unexpected":true}`)}, nil
		})),
	})

	run, err := coordinator.Resume(t.Context(), "run-resume-seeded-budget")
	if err == nil || run.Status != RunBudgetExhausted || run.Failure == nil || run.Failure.Code != FailureBudget {
		t.Fatalf("resume = %#v, err=%v; expected budget exhaustion", run, err)
	}
	if executed {
		t.Fatal("resume scheduled tasks after a persisted budget failure")
	}
}

func TestCoordinatorResumeDoesNotLetCompositionProtectionStarvePendingExecution(t *testing.T) {
	store := NewMemoryRunStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.ID = "contract-resume-composition-protection"
	runID := "run-resume-composition-protection"
	if err := store.Create(InvestigationRun{ID: runID, Contract: contract, Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{RunAnalyzing, RunPlanned} {
		if err := store.Transition(runID, status); err != nil {
			t.Fatal(err)
		}
	}
	task := testExecutableTask("pending-investigator", nil)
	task.Executor = ExecutorInvestigator
	task.Budget.Limit = BudgetVector{}
	if err := store.SavePlan(runID, PlanRevision{
		Revision: 1, ContractID: contract.ID, Tasks: []ExecutableTask{task},
	}); err != nil {
		t.Fatal(err)
	}
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBudget(runID, ledger.Snapshot()); err != nil {
		t.Fatal(err)
	}

	composerCalled := false
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: NewTaskTemplateCatalog(), Schemas: testSchemas(), Store: store,
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: json.RawMessage(`{"summary":"collected"}`), Usage: BudgetVector{OutputTokens: 95}}, nil
		})),
		BudgetLimit:       BudgetVector{OutputTokens: 100},
		CompositionBudget: BudgetVector{OutputTokens: 100},
		Composer: ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
			composerCalled = true
			return AnswerDraft{Text: "unexpected"}, nil
		}),
	})

	run, err := coordinator.Resume(t.Context(), runID)
	if err != nil || run.Status != RunDelivered {
		t.Fatalf("run = %#v, err=%v; want delivered without a budget starvation failure", run, err)
	}
	if composerCalled {
		t.Fatal("composer ran without verified claims")
	}
	if run.Budget.Run.Used.OutputTokens != 95 || run.Budget.Run.Reserved.OutputTokens != 0 {
		t.Fatalf("terminal budget = %+v, want used=95 reserved=0", run.Budget.Run)
	}
}

func TestCoordinatorResumeDeliversRequiredInvestigatorBudgetFailureWithEvidence(t *testing.T) {
	store := NewMemoryRunStore()
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.ID = "contract-resume-required-budget-evidence"
	runID := "run-resume-required-budget-evidence"
	contract.CreatedAt = time.Now().UTC()
	if err := store.Create(InvestigationRun{ID: runID, Contract: contract, Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []RunStatus{RunAnalyzing, RunPlanned, RunExecuting} {
		if err := store.Transition(runID, status); err != nil {
			t.Fatal(err)
		}
	}

	investigator := testExecutableTask("investigator", nil)
	investigator.Executor = ExecutorInvestigator
	verifier := testExecutableTask("evidence.verify", []string{investigator.ID})
	verifier.Executor = ExecutorVerifier
	if err := store.SavePlan(runID, PlanRevision{
		Revision: 1, ContractID: contract.ID, Tasks: []ExecutableTask{investigator, verifier},
	}); err != nil {
		t.Fatal(err)
	}

	unit, err := normalizeEvidence(investigator.ID, EvidenceCandidate{
		SourceKind: "code", Target: "service-a", Content: "the flow enters service-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveEvidence(runID, []EvidenceUnit{unit}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveResult(runID, TaskExecutionRecord{
		TaskID: investigator.ID, Status: TaskPartial, Output: json.RawMessage(`{"summary":"partial"}`),
		Failure: &RunFailure{Code: FailureBudget, Message: "shared investigation run budget is exhausted", TaskID: investigator.ID},
	}); err != nil {
		t.Fatal(err)
	}

	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{OutputTokens: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveBudget(runID, ledger.Snapshot()); err != nil {
		t.Fatal(err)
	}

	claim := ClaimCandidate{
		GoalID: "g1", Text: "the flow enters service-a", Status: ClaimSupported,
		EvidenceRefs: []EvidenceRef{{
			EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target,
			ContentHash: unit.ContentHash,
		}},
	}
	verifierCalled := false
	coordinator := NewCoordinator(CoordinatorOptions{
		Store: store, Schemas: testSchemas(), MaxRounds: 1,
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			if task.ID == verifier.ID {
				verifierCalled = true
				return TaskExecutionResult{
					Output: json.RawMessage(`{"verified":true}`),
					Claims: []ClaimCandidate{claim},
				}, nil
			}
			t.Fatalf("unexpected resumed task: %s", task.ID)
			return TaskExecutionResult{}, nil
		})),
	})

	run, err := coordinator.Resume(t.Context(), runID)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !verifierCalled || run.Status != RunDelivered || run.Delivery == nil || run.Delivery.Status != DeliverySucceeded {
		t.Fatalf("resumed run = %#v, verifier_called=%v", run, verifierCalled)
	}
}
