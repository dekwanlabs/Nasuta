package investigation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorExecuteIsIdempotentForTerminalRun(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		calls.Add(1)
		return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
	})
	store := NewMemoryRunStore()
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog:        catalog,
		Schemas:        testSchemas(),
		Store:          store,
		Executors:      testExecutors(executor),
		BudgetLimit:    BudgetVector{},
		MaxRounds:      1,
		MaxParallelism: 1,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()

	first, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	second, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Status != RunDelivered || second.Delivery == nil {
		t.Fatalf("second run = %#v", second)
	}
	if calls.Load() != 1 {
		t.Fatalf("executor calls = %d, want 1", calls.Load())
	}
}

func TestCoordinatorCancelPersistsUnfinishedRun(t *testing.T) {
	store := NewMemoryRunStore()
	if err := store.Create(InvestigationRun{ID: "run-cancel", Status: RunCreated}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition("run-cancel", RunAnalyzing); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition("run-cancel", RunPlanned); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{Store: store})
	if err := coordinator.Cancel(context.Background(), "run-cancel"); err != nil {
		t.Fatal(err)
	}
	run, err := store.Get("run-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunCancelled || run.Failure == nil || run.Failure.Code != FailureCancelled {
		t.Fatalf("cancelled run = %#v", run)
	}
	if err := coordinator.Cancel(context.Background(), "run-cancel"); err != nil {
		t.Fatalf("terminal cancel should be idempotent: %v", err)
	}
}

func TestCoordinatorCancelStopsActiveRun(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(ctx context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			close(started)
			<-ctx.Done()
			return TaskExecutionResult{}, ctx.Err()
		})),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	type execution struct {
		run InvestigationRun
		err error
	}
	done := make(chan execution, 1)
	go func() {
		run, err := coordinator.Execute(context.Background(), contract)
		done <- execution{run: run, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	if err := coordinator.Cancel(context.Background(), investigationRunID(contract)); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err == nil || result.run.Status != RunCancelled || result.run.Failure == nil || result.run.Failure.Code != FailureCancelled {
			t.Fatalf("active run result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not finish after cancellation")
	}
}

func TestCoordinatorTimeoutPersistsTimedOutRun(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(ctx context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			<-ctx.Done()
			return TaskExecutionResult{}, ctx.Err()
		})),
		BudgetLimit: BudgetVector{Duration: 20 * time.Millisecond},
		MaxRounds:   1,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(context.Background(), contract)
	if err == nil {
		t.Fatal("coordinator returned success on timeout")
	}
	if run.Status != RunTimedOut || run.Failure == nil || run.Failure.Code != FailureTimeout {
		t.Fatalf("timed out run = %#v", run)
	}
}
