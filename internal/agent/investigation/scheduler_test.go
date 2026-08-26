package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
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
	if got := ledger.Snapshot().Run.Reserved.OutputTokens; got != 0 {
		t.Fatalf("reserved output tokens after rejected settle = %d, want 0", got)
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
	if got := ledger.Snapshot().Run.Reserved.OutputTokens; got != 0 {
		t.Fatalf("reserved output tokens after run-limit rejection = %d, want 0", got)
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
