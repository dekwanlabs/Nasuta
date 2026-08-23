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
