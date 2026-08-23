package investigation

import (
	"context"
	"testing"
	"time"
)

func TestCoordinatorPersistsRunMetrics(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1})); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1}}, nil
		})),
		BudgetLimit: BudgetVector{ToolCalls: 4},
		MaxRounds:   2,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if run.Metrics.Rounds != 1 || run.Metrics.Tasks != 1 || run.Metrics.ToolCalls != 1 {
		t.Fatalf("metrics = %#v", run.Metrics)
	}
	if run.Metrics.Duration <= 0 {
		t.Fatalf("metrics duration = %s", run.Metrics.Duration)
	}
	if run.Metrics.ExecutorCounts[ExecutorDirectTool] != 1 {
		t.Fatalf("executor counts = %#v", run.Metrics.ExecutorCounts)
	}
	if run.Metrics.StageUsage[StageExecution].ToolCalls != 1 {
		t.Fatalf("stage usage = %#v", run.Metrics.StageUsage)
	}
}
