package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestSchedulerHonorsDependencyOrder(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	order := make([]string, 0, 2)
	executor := TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		mu.Lock()
		order = append(order, task.ID)
		mu.Unlock()
		return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1}}, nil
	})
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger, MaxParallelism: 2}).Execute(context.Background(), []ExecutableTask{
		testExecutableTask("b", []string{"a"}),
		testExecutableTask("a", nil),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != TaskSucceeded || results[1].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("execution order = %#v", order)
	}
}

func TestSchedulerRunsIndependentTasksInParallel(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active, maxActive := 0, 0
	barrier := make(chan struct{})
	closed := false
	executor := TaskExecutorFunc(func(ctx context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		if active == 2 && !closed {
			close(barrier)
			closed = true
		}
		mu.Unlock()
		select {
		case <-barrier:
		case <-ctx.Done():
			return TaskExecutionResult{}, ctx.Err()
		}
		mu.Lock()
		active--
		mu.Unlock()
		return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1}}, nil
	})
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger, MaxParallelism: 2}).Execute(context.Background(), []ExecutableTask{
		testExecutableTask("a", nil),
		testExecutableTask("b", nil),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || maxActive != 2 {
		t.Fatalf("results = %#v, max active = %d", results, maxActive)
	}
}

func TestSchedulerPassesExplicitAgentNarrowingToExecutorTask(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100, OutputTokens: 50, ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	var got ExecutableTask
	var gotInput TaskExecutionInput
	executor := TaskExecutorFunc(func(_ context.Context, task ExecutableTask, input TaskExecutionInput) (TaskExecutionResult, error) {
		got = task
		gotInput = input
		return TaskExecutionResult{}, nil
	})
	task := testExecutableTask("agent-a", nil)
	task.Executor = ExecutorInvestigator
	task.Budget.Limit = BudgetVector{InputTokens: 70, OutputTokens: 30, ToolCalls: 2}
	scheduler := Scheduler{
		Executors: testExecutors(executor),
		Schemas:   testSchemas(),
		Ledger:    ledger,
	}
	results, err := scheduler.Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if got.Budget.Limit.InputTokens != 70 || got.Budget.Limit.OutputTokens != 30 {
		t.Fatalf("executor task budget = %+v, want explicit task narrowing", got.Budget.Limit)
	}
	if gotInput.RuntimeBudget.InputTokens != 70 || gotInput.RuntimeBudget.OutputTokens != 30 {
		t.Fatalf("runtime budget = %+v, want worker grant", gotInput.RuntimeBudget)
	}
}

func TestSchedulerPassesProtectedVerifierRoleGrantToRuntime(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{
		InputTokens: 300_000, OutputTokens: 128_000, TotalTokens: 512_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	grant := BudgetVector{InputTokens: 30_000, OutputTokens: 12_800, TotalTokens: 51_200}
	reservations, err := ledger.ReserveAdmissionGroup(StageVerification, []BudgetAdmission{{
		TaskID: "verifier", Grant: grant,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var gotInput TaskExecutionInput
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, input TaskExecutionInput) (TaskExecutionResult, error) {
		gotInput = input
		return TaskExecutionResult{Output: []byte(`{}`)}, nil
	})
	task := testExecutableTask("verifier", nil)
	task.Executor = ExecutorVerifier
	scheduler := Scheduler{
		Executors:           NewExecutorRegistry(map[ExecutorType]TaskExecutor{ExecutorVerifier: executor}),
		Schemas:             testSchemas(),
		Ledger:              ledger,
		ProtectedAdmissions: reservations,
	}
	results, err := scheduler.Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if gotInput.RuntimeBudget != grant {
		t.Fatalf("verifier runtime budget = %+v, want %+v", gotInput.RuntimeBudget, grant)
	}
}

func TestSchedulerBlocksRequiredDependentsAfterFailure(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	executor := TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		calls = append(calls, task.ID)
		return TaskExecutionResult{}, errors.New("upstream failed")
	})
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger}).Execute(context.Background(), []ExecutableTask{
		testExecutableTask("a", nil),
		testExecutableTask("b", []string{"a"}),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != "a" {
		t.Fatalf("executor calls = %#v", calls)
	}
	if results[1].Status != TaskBlocked || results[1].Failure == nil {
		t.Fatalf("dependent result = %#v", results[1])
	}
}

func TestSchedulerDoesNotStartOptionalDependentBeforeDependencyResolves(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	order := make([]string, 0, 2)
	executor := TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		mu.Lock()
		order = append(order, task.ID)
		mu.Unlock()
		if task.ID == "a" {
			time.Sleep(10 * time.Millisecond)
		}
		return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	})
	b := testExecutableTask("b", []string{"a"})
	b.Optional = true
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger}).Execute(context.Background(), []ExecutableTask{
		b,
		testExecutableTask("a", nil),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("execution order = %#v", order)
	}
	for _, result := range results {
		if result.Status != TaskSucceeded {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestSchedulerMapsCancellationAndTimeout(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := (Scheduler{Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{}, nil
	})), Schemas: testSchemas(), Ledger: ledger}).Execute(ctx, []ExecutableTask{testExecutableTask("cancelled", nil)}, nil)
	if err != nil || len(results) != 1 || results[0].Status != TaskCancelled || results[0].Failure.Code != FailureCancelled {
		t.Fatalf("cancelled results = %#v, error = %v", results, err)
	}

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer timeoutCancel()
	results, err = (Scheduler{Executors: testExecutors(TaskExecutorFunc(func(ctx context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		<-ctx.Done()
		return TaskExecutionResult{}, ctx.Err()
	})), Schemas: testSchemas(), Ledger: ledger}).Execute(timeoutCtx, []ExecutableTask{testExecutableTask("timed", nil)}, nil)
	if err != nil || len(results) != 1 || results[0].Status != TaskCancelled || results[0].Failure.Code != FailureTimeout {
		t.Fatalf("timed results = %#v, error = %v", results, err)
	}
}

func TestSchedulerPreservesExecutorError(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := "create definition run: duplicate key"
	results, err := (Scheduler{
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{}, errors.New(want)
		})),
		Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{testExecutableTask("failed", nil)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Failure == nil {
		t.Fatalf("results = %#v", results)
	}
	if results[0].Failure.Message != want {
		t.Fatalf("failure message = %q, want %q", results[0].Failure.Message, want)
	}
}

func TestSchedulerPassesAttemptToRetries(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	var attempts []int
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, input TaskExecutionInput) (TaskExecutionResult, error) {
		attempts = append(attempts, input.Attempt)
		if input.Attempt == 1 {
			return TaskExecutionResult{Failure: &RunFailure{
				Code: FailureReasoning, Message: "retry reasoning", Retryable: true,
			}}, nil
		}
		return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
	})
	task := testExecutableTask("retried", nil)
	task.Budget.MaxAttempts = 2
	results, err := (Scheduler{
		Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("attempts = %#v, want [1 2]", attempts)
	}
}

func TestSchedulerDoesNotTreatCleanupCancelAsTaskFailure(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{Duration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	task := testExecutableTask("timed-success", nil)
	task.Budget.Limit = BudgetVector{Duration: time.Second}
	results, err := (Scheduler{
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})),
		Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskSucceeded || results[0].Failure != nil {
		t.Fatalf("results = %#v", results)
	}
}

func testExecutableTask(id string, dependencies []string) ExecutableTask {
	return ExecutableTask{
		ID:           id,
		Objective:    id,
		Dependencies: dependencies,
		Budget:       TaskBudget{Limit: BudgetVector{ToolCalls: 1}},
		InputSchema:  testTemplate("task", 1, nil, ExecutorDirectTool, nil, BudgetVector{}).InputSchema,
		OutputSchema: testTemplate("task", 1, nil, ExecutorDirectTool, nil, BudgetVector{}).OutputSchema,
		Executor:     ExecutorDirectTool,
		Status:       TaskPending,
	}
}

func TestSchedulerSerializesNonParallelAgentNodes(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active, maxActive := 0, 0
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return TaskExecutionResult{Usage: BudgetVector{ToolCalls: 1}}, nil
	})
	first := testExecutableTask("agent-a", nil)
	first.Executor = ExecutorInvestigator
	first.AllowParallel = false
	second := testExecutableTask("agent-b", nil)
	second.Executor = ExecutorInvestigator
	second.AllowParallel = false
	results, err := (Scheduler{
		Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger, MaxParallelism: 2,
	}).Execute(context.Background(), []ExecutableTask{first, second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if maxActive != 1 {
		t.Fatalf("maximum concurrent serialized agents = %d, want 1", maxActive)
	}
	for _, result := range results {
		if result.Status != TaskSucceeded {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestSchedulerRejectsDependencyCycle(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	a := testExecutableTask("a", []string{"b"})
	b := testExecutableTask("b", []string{"a"})
	_, err = (Scheduler{Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{}, nil
	})), Schemas: testSchemas(), Ledger: ledger}).Execute(context.Background(), []ExecutableTask{a, b}, nil)
	if err == nil || !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("cycle error = %v, want ErrPlanInvalid", err)
	}
}

func TestSchedulerChargesRetryAttemptsToSharedRunBudget(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		attempts++
		if attempts == 1 {
			return TaskExecutionResult{Usage: BudgetVector{ToolCalls: 1}, Failure: &RunFailure{
				Code: FailureReasoning, Message: "retry", Retryable: true,
			}}, nil
		}
		return TaskExecutionResult{Usage: BudgetVector{ToolCalls: 1}}, nil
	})
	task := testExecutableTask("retry-budget", nil)
	task.Budget.Limit = BudgetVector{}
	task.Budget.MaxAttempts = 2
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if results[0].Result.Usage.ToolCalls != 2 {
		t.Fatalf("task usage = %+v, want both attempts charged", results[0].Result.Usage)
	}
	if got := ledger.Snapshot().Run.Used.ToolCalls; got != 2 {
		t.Fatalf("run used tool calls = %d, want 2", got)
	}
}

func TestSchedulerReservesSingleAgentBudgetForParallelAgents(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{Usage: BudgetVector{OutputTokens: 10}}, nil
	})
	first := testExecutableTask("agent-output-a", nil)
	first.Executor = ExecutorInvestigator
	first.AllowParallel = true
	first.Budget.Limit = BudgetVector{}
	second := testExecutableTask("agent-output-b", nil)
	second.Executor = ExecutorInvestigator
	second.AllowParallel = true
	second.Budget.Limit = BudgetVector{}
	results, err := (Scheduler{
		Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger,
		MaxParallelism: 2, MaxAgentParallelism: 2,
	}).Execute(context.Background(), []ExecutableTask{first, second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.Status != TaskSucceeded {
			t.Fatalf("result = %#v", result)
		}
	}
	if got := ledger.Snapshot().Run.Used.OutputTokens; got != 20 {
		t.Fatalf("used output tokens = %d, want 20", got)
	}
	if got := ledger.Snapshot().Run.Reserved.OutputTokens; got != 0 {
		t.Fatalf("reserved output tokens = %d, want 0", got)
	}
}

func TestSchedulerReleasesReservationWhenSettleRejectsUsage(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{Usage: BudgetVector{OutputTokens: 11}}, nil
	})
	task := testExecutableTask("agent-over-budget", nil)
	task.Executor = ExecutorInvestigator
	task.Budget.Limit = BudgetVector{}
	results, err := (Scheduler{
		Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskFailed {
		t.Fatalf("results = %#v, want one failed task", results)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Reserved.OutputTokens != 0 || snapshot.Run.Used.OutputTokens != 11 {
		t.Fatalf("budget after unavoidable overrun = %+v, want used=11 reserved=0", snapshot.Run)
	}
}

func TestSchedulerChargesAgentUsageAboveAdmissionGrant(t *testing.T) {
	// A single Agent attempt accumulates usage across its Steps. The admission
	// grant is only a concurrency floor, so an Agent may settle above it while
	// staying inside the shared Run limit.
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 30})
	if err != nil {
		t.Fatal(err)
	}
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{Usage: BudgetVector{OutputTokens: 20}}, nil
	})
	task := testExecutableTask("agent-multi-step", nil)
	task.Executor = ExecutorInvestigator
	task.Budget.Limit = BudgetVector{}
	results, err := (Scheduler{
		Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskSucceeded {
		t.Fatalf("results = %#v, want one succeeded task", results)
	}
	if got := ledger.Snapshot().Run.Used.OutputTokens; got != 20 {
		t.Fatalf("used output tokens = %d, want 20", got)
	}
	if got := ledger.Snapshot().Run.Reserved.OutputTokens; got != 0 {
		t.Fatalf("reserved output tokens = %d, want 0", got)
	}
}

func TestSchedulerStillRejectsAgentUsageAboveRunLimit(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{Usage: BudgetVector{OutputTokens: 11}}, nil
	})
	task := testExecutableTask("agent-over-run", nil)
	task.Executor = ExecutorInvestigator
	task.Budget.Limit = BudgetVector{}
	results, err := (Scheduler{
		Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskFailed {
		t.Fatalf("results = %#v, want one failed task", results)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Reserved.OutputTokens != 0 || snapshot.Run.Used.OutputTokens != 11 {
		t.Fatalf("budget after run-limit overrun = %+v, want used=11 reserved=0", snapshot.Run)
	}
}

func TestSchedulerRetriesStrictDocsReportFormatOnce(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		attempts++
		return TaskExecutionResult{Failure: &RunFailure{
			Code: FailureSchema, Message: "malformed investigation.report", Retryable: true,
		}}, nil
	})
	task := testExecutableTask("docs-report", nil)
	task.Executor = ExecutorInvestigator
	task.Capability = "knowledge.docs.verify"
	task.Budget.MaxAttempts = 1
	task.Budget.Limit = BudgetVector{OutputTokens: 5}
	results, err := (Scheduler{
		Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want exactly one retry", attempts)
	}
	if len(results) != 1 || results[0].Status != TaskFailed || results[0].Failure == nil || results[0].Failure.Code != FailureSchema {
		t.Fatalf("results = %#v", results)
	}
}

func TestSchedulerAgentAdmissionIgnoresExecutionStageAllocation(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 30, ToolCalls: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{OutputTokens: 10, ToolCalls: 3}); err != nil {
		t.Fatal(err)
	}
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{OutputTokens: 20, ToolCalls: 4}}, nil
	})
	task := testExecutableTask("agent-stage-overrun", nil)
	task.Executor = ExecutorInvestigator
	task.Budget.Limit = BudgetVector{}
	results, err := (Scheduler{
		Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if got := ledger.Snapshot().Run.Used; got.OutputTokens != 20 || got.ToolCalls != 4 {
		t.Fatalf("run usage = %+v", got)
	}
}

func TestSchedulerIgnoresStaleAndNonTerminalInitialResults(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	executor := TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		calls = append(calls, task.ID)
		return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1}}, nil
	})
	tasks := []ExecutableTask{testExecutableTask("a", nil), testExecutableTask("b", []string{"a"})}
	initial := map[string]ScheduledTaskResult{
		"stale": {Status: TaskSucceeded, Result: TaskExecutionResult{Output: json.RawMessage(`{"stale":true}`)}},
		"a":     {Status: TaskRunning},
		"b":     {Status: TaskPending},
	}
	results, err := (Scheduler{
		Executors:      testExecutors(executor),
		Schemas:        testSchemas(),
		Ledger:         ledger,
		InitialResults: initial,
	}).Execute(context.Background(), tasks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != TaskSucceeded || results[1].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if len(calls) != 2 || calls[0] != "a" || calls[1] != "b" {
		t.Fatalf("calls = %#v, want [a b]", calls)
	}
}

func TestSchedulerPreservesDependencyFailureCause(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	upstream := testExecutableTask("upstream", nil)
	downstream := testExecutableTask("downstream", []string{"upstream"})
	want := &RunFailure{Code: FailureReasoning, Message: "model response truncated", Retryable: true}
	results, err := (Scheduler{
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			if task.ID == upstream.ID {
				return TaskExecutionResult{Failure: want}, nil
			}
			return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})),
		Schemas: testSchemas(),
		Ledger:  ledger,
	}).Execute(context.Background(), []ExecutableTask{downstream, upstream}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != TaskBlocked || results[0].Failure == nil {
		t.Fatalf("results = %#v", results)
	}
	failure := results[0].Failure
	if failure.Code != want.Code || !failure.Retryable ||
		!strings.Contains(failure.Message, `required dependency "upstream" failed`) ||
		!strings.Contains(failure.Message, want.Message) {
		t.Fatalf("dependency failure = %#v", failure)
	}
}

func TestSchedulerAllowsVerifierAfterInvestigatorReturnsEvidenceWithFailure(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	investigator := testExecutableTask("investigator", nil)
	investigator.Executor = ExecutorInvestigator
	verifier := testExecutableTask("evidence.verify", []string{investigator.ID})
	verifier.Executor = ExecutorVerifier
	verifier.AllowParallel = false
	var calls []string
	executor := TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		calls = append(calls, task.ID)
		if task.ID == investigator.ID {
			return TaskExecutionResult{
				EvidenceCandidates: []EvidenceCandidate{{SourceKind: "code", Target: "service-a", Content: "retrieved evidence"}},
				Failure:            &RunFailure{Code: FailureReasoning, Message: "worker stopped after tool evidence"},
			}, nil
		}
		return TaskExecutionResult{Output: json.RawMessage(`{"verified":true}`)}, nil
	})
	results, err := (Scheduler{
		Executors: testExecutors(executor),
		Schemas:   testSchemas(),
		Ledger:    ledger,
	}).Execute(context.Background(), []ExecutableTask{verifier, investigator}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
	byID := make(map[string]ScheduledTaskResult, len(results))
	for _, result := range results {
		byID[result.Task.ID] = result
	}
	if byID[investigator.ID].Status != TaskPartial || byID[verifier.ID].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if len(byID[investigator.ID].Result.EvidenceCandidates) != 1 || len(calls) != 2 || calls[0] != investigator.ID || calls[1] != verifier.ID {
		t.Fatalf("results = %#v, calls = %#v", results, calls)
	}
}

func TestSchedulerPreservesBudgetFailureWhenInvestigatorIsRejectedBeforeExecution(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	spent, err := ledger.Reserve(StageExecution, "spent", BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := spent.Settle(BudgetVector{ToolCalls: 1}); err != nil {
		t.Fatal(err)
	}

	called := false
	task := testExecutableTask("investigator", nil)
	task.Executor = ExecutorInvestigator
	results, err := (Scheduler{
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			called = true
			return TaskExecutionResult{}, nil
		})),
		Schemas: testSchemas(),
		Ledger:  ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("investigator executed after shared budget was exhausted")
	}
	if len(results) != 1 {
		t.Fatalf("results = %#v", results)
	}
	result := results[0]
	if result.Status != TaskFailed || result.Failure == nil || result.Failure.Code != FailureBudget {
		t.Fatalf("result = %#v, want terminal budget failure", result)
	}
	if len(result.Result.EvidenceCandidates) != 0 || len(result.Result.Output) != 0 {
		t.Fatalf("result contains fabricated partial artifact: %#v", result.Result)
	}
}

type minimumBudgetTaskExecutor struct {
	minOutput int64
	fn        func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error)
}

func (executor minimumBudgetTaskExecutor) MinimumBudget(ExecutableTask) (BudgetVector, error) {
	return BudgetVector{OutputTokens: executor.minOutput}, nil
}

func (executor minimumBudgetTaskExecutor) Execute(ctx context.Context, task ExecutableTask, input TaskExecutionInput) (TaskExecutionResult, error) {
	return executor.fn(ctx, task, input)
}

func TestSchedulerProtectsMinimumBudgetForParallelAgents(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 2000})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active, maxActive := 0, 0
	barrier := make(chan struct{})
	closed := false
	executor := minimumBudgetTaskExecutor{minOutput: 1000, fn: func(ctx context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		if active == 2 && !closed {
			close(barrier)
			closed = true
		}
		mu.Unlock()
		select {
		case <-barrier:
		case <-ctx.Done():
			return TaskExecutionResult{}, ctx.Err()
		}
		mu.Lock()
		active--
		mu.Unlock()
		return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{OutputTokens: 1}}, nil
	}}
	tasks := []ExecutableTask{testExecutableTask("agent-a", nil), testExecutableTask("agent-b", nil)}
	for index := range tasks {
		tasks[index].Executor = ExecutorInvestigator
		tasks[index].AllowParallel = true
	}
	results, err := (Scheduler{
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{ExecutorInvestigator: executor}),
		Schemas:   testSchemas(), Ledger: ledger, MaxParallelism: 2,
	}).Execute(context.Background(), tasks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != TaskSucceeded || results[1].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if maxActive != 2 {
		t.Fatalf("max active = %d, want both protected agents admitted", maxActive)
	}
}

func TestSchedulerQueuesAgentWhenParallelMinimumsDoNotFit(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 1500})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	active, maxActive := 0, 0
	executor := minimumBudgetTaskExecutor{minOutput: 1000, fn: func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		active--
		mu.Unlock()
		return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{OutputTokens: 1}}, nil
	}}
	tasks := []ExecutableTask{testExecutableTask("agent-a", nil), testExecutableTask("agent-b", nil)}
	for index := range tasks {
		tasks[index].Executor = ExecutorInvestigator
		tasks[index].AllowParallel = true
	}
	results, err := (Scheduler{
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{ExecutorInvestigator: executor}),
		Schemas:   testSchemas(), Ledger: ledger, MaxParallelism: 2,
	}).Execute(context.Background(), tasks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != TaskSucceeded || results[1].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	if maxActive != 1 {
		t.Fatalf("max active = %d, want queued admission", maxActive)
	}
}

type resolvingTaskExecutorRegistry struct {
	err      error
	executor TaskExecutor
}

func (registry resolvingTaskExecutorRegistry) Resolve(ExecutorType) (TaskExecutor, error) {
	if registry.err != nil {
		return nil, registry.err
	}
	return registry.executor, nil
}

func TestSchedulerPropagatesNonCapabilityAdmissionError(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("definition provider unavailable")
	task := testExecutableTask("investigator", nil)
	task.Executor = ExecutorInvestigator
	_, err = (Scheduler{
		Executors: resolvingTaskExecutorRegistry{err: wantErr},
		Schemas:   testSchemas(),
		Ledger:    ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapped admission error", err)
	}
}

func TestSchedulerKeepsCapabilityGapAsTaskFailure(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	task := testExecutableTask("missing-investigator", nil)
	task.Executor = ExecutorInvestigator
	results, err := (Scheduler{
		Executors: NewExecutorRegistry(nil),
		Schemas:   testSchemas(),
		Ledger:    ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskFailed || results[0].Failure == nil {
		t.Fatalf("results = %#v, want one task failure", results)
	}
	// The scheduler preserves the executor's safe task-failure behavior. The
	// capability sentinel is wrapped at the registry boundary and intentionally
	// represented by the stable task failure code in the persisted result.
	if results[0].Failure.Code != FailureExecution {
		t.Fatalf("failure = %#v, want execution failure", results[0].Failure)
	}
}

func TestSchedulerSalvagesEvidenceOnBudgetFailure(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	task := testExecutableTask("budget-with-evidence", nil)
	task.Executor = ExecutorInvestigator
	executor := TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{
			EvidenceCandidates: []EvidenceCandidate{{SourceKind: "runbook", Target: "budget.md", Content: "usable evidence"}},
			Failure:            &RunFailure{Code: FailureBudget, Message: "next turn exceeded budget"},
		}, nil
	})
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskPartial {
		t.Fatalf("results = %#v, want partial task", results)
	}
	if results[0].Failure == nil || results[0].Failure.Code != FailureBudget {
		t.Fatalf("failure = %#v, want budget failure", results[0].Failure)
	}
	if len(results[0].Result.EvidenceCandidates) != 1 {
		t.Fatalf("evidence candidates = %#v, want salvaged evidence", results[0].Result.EvidenceCandidates)
	}
}

func TestSchedulerSalvagesEvidenceOnRuntimeFailure(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	task := testExecutableTask("runtime-with-evidence", nil)
	task.Executor = ExecutorInvestigator
	executor := TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{
			EvidenceCandidates: []EvidenceCandidate{{SourceKind: "code", Target: "handler.go", Content: "usable evidence"}},
			Usage:              BudgetVector{ToolCalls: 1},
		}, errors.New("provider unavailable")
	})
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskPartial {
		t.Fatalf("results = %#v, want partial task", results)
	}
	if results[0].Failure == nil || results[0].Failure.Code != FailureExecution {
		t.Fatalf("failure = %#v, want runtime execution failure", results[0].Failure)
	}
	if len(results[0].Result.EvidenceCandidates) != 1 {
		t.Fatalf("evidence candidates = %#v, want salvaged evidence", results[0].Result.EvidenceCandidates)
	}
}

func TestSchedulerDoesNotFabricatePartialWithoutEvidence(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 1024})
	if err != nil {
		t.Fatal(err)
	}
	task := testExecutableTask("budget-without-evidence", nil)
	task.Executor = ExecutorInvestigator
	executor := TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{Failure: &RunFailure{Code: FailureBudget, Message: "budget exhausted"}}, nil
	})
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskFailed {
		t.Fatalf("results = %#v, want failed task", results)
	}
	if len(results[0].Result.EvidenceCandidates) != 0 {
		t.Fatalf("evidence candidates = %#v, want none", results[0].Result.EvidenceCandidates)
	}
}

func TestSchedulerReleasesAdmissionWhenInputValidationFails(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	task := testExecutableTask("invalid-input", nil)
	task.Executor = ExecutorInvestigator
	task.InputSchema = agentapi.SchemaRef{ID: "missing-input-schema", Version: 1}
	executor := minimumBudgetTaskExecutor{minOutput: 1000, fn: func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
		t.Fatal("executor ran despite invalid input schema")
		return TaskExecutionResult{}, nil
	}}
	results, err := (Scheduler{
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{ExecutorInvestigator: executor}),
		Schemas:   testSchemas(),
		Ledger:    ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskFailed || results[0].Failure == nil || results[0].Failure.Code != FailureSchema {
		t.Fatalf("results = %#v, want schema failure", results)
	}
	if got := ledger.Snapshot().Run.Reserved.OutputTokens; got != 0 {
		t.Fatalf("reserved output after validation failure = %d, want 0", got)
	}
}

func TestSchedulerRequiresLedgerBeforeStartingTasks(t *testing.T) {
	started := false
	scheduler := Scheduler{
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			started = true
			return TaskExecutionResult{}, nil
		})),
		Schemas: testSchemas(),
		OnComplete: func(ScheduledTaskResult) {
			t.Fatal("completion callback invoked before scheduler initialization")
		},
	}
	_, err := scheduler.Execute(context.Background(), []ExecutableTask{testExecutableTask("missing-ledger", nil)}, nil)
	if err == nil || err.Error() != "budget ledger is required" {
		t.Fatalf("error = %v, want missing ledger error", err)
	}
	if started {
		t.Fatal("task started without a budget ledger")
	}
}

func TestSchedulerReleasesZeroGrantAdmissionWhenSharedBudgetIsExhausted(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	spent, err := ledger.Reserve(StageExecution, "spent", BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := spent.Settle(BudgetVector{ToolCalls: 1}); err != nil {
		t.Fatal(err)
	}

	called := false
	task := testExecutableTask("zero-grant", nil)
	task.Executor = ExecutorInvestigator
	results, err := (Scheduler{
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			called = true
			return TaskExecutionResult{}, nil
		})),
		Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if called || len(results) != 1 || results[0].Failure == nil || results[0].Failure.Code != FailureBudget {
		t.Fatalf("results = %#v, called = %v", results, called)
	}
	if snapshot := ledger.Snapshot(); snapshot.Run.Reserved != (BudgetVector{}) {
		t.Fatalf("budget reservation leaked after zero-grant rejection: %+v", snapshot.Run.Reserved)
	}
}

func TestSchedulerReportsRejectedAgentInsteadOfReadyDirectTool(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 1000, ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	spent, err := ledger.Reserve(StageExecution, "spent", BudgetVector{OutputTokens: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if err := spent.Settle(BudgetVector{OutputTokens: 1000}); err != nil {
		t.Fatal(err)
	}

	var calls []string
	executor := minimumBudgetTaskExecutor{
		minOutput: 1024,
		fn: func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			calls = append(calls, task.ID)
			return TaskExecutionResult{}, nil
		},
	}
	direct := testExecutableTask("direct", nil)
	agent := testExecutableTask("agent", nil)
	agent.Executor = ExecutorInvestigator
	agent.AllowParallel = true
	results, err := (Scheduler{
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{
			ExecutorDirectTool:   executor,
			ExecutorInvestigator: executor,
		}),
		Schemas:        testSchemas(),
		Ledger:         ledger,
		MaxParallelism: 2,
	}).Execute(context.Background(), []ExecutableTask{agent, direct}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != direct.ID {
		t.Fatalf("executor calls = %#v, results=%#v, want only direct task", calls, results)
	}
	if len(results) != 2 {
		t.Fatalf("results = %#v", results)
	}
	byID := make(map[string]ScheduledTaskResult, len(results))
	for _, result := range results {
		byID[result.Task.ID] = result
	}
	if byID[direct.ID].Status != TaskSucceeded {
		t.Fatalf("direct result = %#v, want success", byID[direct.ID])
	}
	if byID[agent.ID].Status != TaskFailed || byID[agent.ID].Failure == nil || byID[agent.ID].Failure.Code != FailureBudget {
		t.Fatalf("agent result = %#v, want budget failure", byID[agent.ID])
	}
}

type protectedAdmissionExecutor struct {
	TaskExecutor
	minimum BudgetVector
}

func (executor protectedAdmissionExecutor) MinimumBudget(ExecutableTask) (BudgetVector, error) {
	return executor.minimum, nil
}

func TestSchedulerConsumesProtectedAdmissionWithoutDoubleReservation(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	protected, err := ledger.ReserveAdmission(StageVerification, "verifier", BudgetVector{OutputTokens: 80})
	if err != nil {
		t.Fatal(err)
	}
	verifier := protectedAdmissionExecutor{
		TaskExecutor: TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`), Usage: BudgetVector{OutputTokens: 80}}, nil
		}),
		minimum: BudgetVector{OutputTokens: 80},
	}
	task := testExecutableTask("verifier", nil)
	task.Executor = ExecutorVerifier
	results, err := (Scheduler{
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{
			ExecutorVerifier: verifier,
		}),
		Schemas:             testSchemas(),
		Ledger:              ledger,
		ProtectedAdmissions: map[string]BudgetReservation{task.ID: protected},
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Used.OutputTokens != 80 || snapshot.Run.Reserved.OutputTokens != 0 {
		t.Fatalf("budget snapshot = %#v, want 80 used and no reservation", snapshot.Run)
	}
}

func TestSchedulerAllowsInvestigatorAndVerifierWithCompositionProtection(t *testing.T) {
	// Reproduce the important ordering from the production workflow: composition
	// is protected first, then verification is protected, and the investigator
	// must still be admitted and consume its larger role floor.
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 24_576})
	if err != nil {
		t.Fatal(err)
	}
	composition, err := ledger.Reserve(StageComposition, "composition", BudgetVector{OutputTokens: composerMinimumOutputTokens})
	if err != nil {
		t.Fatal(err)
	}
	defer composition.Release()
	verifierAdmission, err := ledger.ReserveAdmission(StageVerification, "verifier", BudgetVector{OutputTokens: verifierMinimumOutputTokens})
	if err != nil {
		t.Fatal(err)
	}

	investigator := minimumBudgetTaskExecutor{
		minOutput: investigatorMinimumOutputTokens,
		fn: func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			if task.Executor == ExecutorInvestigator {
				return TaskExecutionResult{Output: json.RawMessage(`{"report":"complete"}`), Usage: BudgetVector{OutputTokens: investigatorMinimumOutputTokens}}, nil
			}
			return TaskExecutionResult{Output: json.RawMessage(`{"verified":true}`), Usage: BudgetVector{OutputTokens: verifierMinimumOutputTokens}}, nil
		},
	}
	investigatorTask := testExecutableTask("investigator", nil)
	investigatorTask.Executor = ExecutorInvestigator
	investigatorTask.AllowParallel = false
	verifierTask := testExecutableTask("verifier", []string{investigatorTask.ID})
	verifierTask.Executor = ExecutorVerifier

	results, err := (Scheduler{
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{
			ExecutorInvestigator: investigator,
			ExecutorVerifier:     investigator,
		}),
		Schemas: testSchemas(), Ledger: ledger,
		ProtectedAdmissions: map[string]BudgetReservation{
			verifierTask.ID: verifierAdmission,
		},
	}).Execute(context.Background(), []ExecutableTask{investigatorTask, verifierTask}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Status != TaskSucceeded || results[1].Status != TaskSucceeded {
		t.Fatalf("results = %#v", results)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Used.OutputTokens != investigatorMinimumOutputTokens+verifierMinimumOutputTokens {
		t.Fatalf("run usage = %d, want %d", snapshot.Run.Used.OutputTokens, investigatorMinimumOutputTokens+verifierMinimumOutputTokens)
	}
	if snapshot.Run.Reserved.OutputTokens != composerMinimumOutputTokens {
		t.Fatalf("composition protection = %d, want %d", snapshot.Run.Reserved.OutputTokens, composerMinimumOutputTokens)
	}
}
