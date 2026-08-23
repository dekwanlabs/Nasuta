package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestPlanFailsWithoutSchemaRegistry(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	_, err := (PlanCompiler{Catalog: catalog}).CompileGenerated(contract)
	if err == nil || !errors.Is(err, ErrCapabilityGap) {
		t.Fatalf("missing schema registry error = %v", err)
	}
}

func TestSchedulerFailsOnUnregisteredExecutor(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	executor := TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
	})
	registry := NewExecutorRegistry(map[ExecutorType]TaskExecutor{ExecutorDirectTool: executor})
	task := testExecutableTask("task-a", nil)
	task.Executor = ExecutorInvestigator
	results, err := (Scheduler{Executors: registry, Schemas: testSchemas(), Ledger: ledger}).Execute(
		context.Background(),
		[]ExecutableTask{task},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskFailed || results[0].Failure == nil {
		t.Fatalf("unregistered executor result = %#v", results)
	}
}

func TestPlanRequiresResolvableSchemas(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	missingOutput := agentapi.NewSchemaRegistry()
	if err := missingOutput.Publish([]agentapi.SchemaDefinition{{
		ID:       "investigation.input",
		Version:  1,
		Document: json.RawMessage(`{"type":"object"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	_, err := (PlanCompiler{Catalog: catalog, Schemas: missingOutput}).CompileGenerated(contract)
	if err == nil || !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("missing output schema error = %v", err)
	}
}
