package investigation

import (
	"context"
	"testing"
	"time"
)

func TestCoordinatorRecordsSchemaInvalidTaskFailure(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte("not-json")}, nil
		})),
		BudgetLimit: BudgetVector{},
		MaxRounds:   1,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFailureCode(run.Report.Failures, FailureSchema) {
		t.Fatalf("report failures = %#v", run.Report.Failures)
	}
}

func TestCoordinatorBudgetExhaustedPersistsTerminalFailure(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1})); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
		})),
		BudgetLimit: BudgetVector{},
		MaxRounds:   1,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	_, err := coordinator.Execute(context.Background(), contract)
	if err == nil {
		t.Fatal("budget exhaustion returned success")
	}
	run, getErr := coordinator.Store.Get(investigationRunID(contract))
	if getErr != nil {
		t.Fatal(getErr)
	}
	if run.Status != RunBudgetExhausted || run.Failure == nil || run.Failure.Code != FailureBudget {
		t.Fatalf("budget run = %#v", run)
	}
}

func hasFailureCode(failures []RunFailure, code FailureCode) bool {
	for _, failure := range failures {
		if failure.Code == code {
			return true
		}
	}
	return false
}
