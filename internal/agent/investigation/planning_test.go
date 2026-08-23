package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestCatalogGeneratesCandidatesForRequiredGoals(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, nil, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	contract := testContract(
		EvidenceGoal{ID: "g1", Kind: "domain", Description: "find domain", Required: true},
		EvidenceGoal{ID: "g2", Kind: "flow", Description: "find flow", Required: true},
	)
	candidates, err := catalog.GenerateCandidates(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidate count = %d, want 1", len(candidates))
	}
	if got, want := len(candidates[0].GoalIDs), 2; got != want {
		t.Fatalf("candidate goal ids = %d, want %d", got, want)
	}
}

func TestCatalogVersionIsPartOfGeneratedCandidateIdentity(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	for version := int64(1); version <= 2; version++ {
		if err := catalog.Register(testTemplate("inspect", version, []string{"domain"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := catalog.GenerateCandidates(testContract(EvidenceGoal{ID: "g1", Kind: "domain", Required: true}))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].ID == candidates[1].ID {
		t.Fatalf("versioned candidates = %#v", candidates)
	}
}

func TestPlanRejectsRequiredGoalWithoutCandidate(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("flow", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	_, err := (PlanCompiler{Catalog: catalog, Schemas: testSchemas()}).Compile(testContract(EvidenceGoal{ID: "g1", Kind: "domain", Required: true}), []TaskCandidate{
		{ID: "task-flow", Template: TaskTemplateRef{ID: "flow", Version: 1}, Objective: "flow", GoalIDs: []string{"g1"}},
	})
	if err == nil {
		t.Fatal("plan accepted a candidate whose template does not match its goal")
	}
}

func TestPlanRejectsUnknownDependencyAndCycle(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, nil, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	_, err := (PlanCompiler{Catalog: catalog, Schemas: testSchemas()}).Compile(contract, []TaskCandidate{
		{ID: "a", Template: TaskTemplateRef{ID: "inspect", Version: 1}, Objective: "a", GoalIDs: []string{"g1"}, Dependencies: []string{"missing"}},
	})
	if err == nil || !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("unknown dependency error = %v", err)
	}
	_, err = (PlanCompiler{Catalog: catalog, Schemas: testSchemas()}).Compile(contract, []TaskCandidate{
		{ID: "a", Template: TaskTemplateRef{ID: "inspect", Version: 1}, Objective: "a", GoalIDs: []string{"g1"}, Dependencies: []string{"b"}},
		{ID: "b", Template: TaskTemplateRef{ID: "inspect", Version: 1}, Objective: "b", GoalIDs: []string{"g1"}, Dependencies: []string{"a"}},
	})
	if err == nil || !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("cycle error = %v", err)
	}
}

func TestPlanToolGrantUsesEffectivePermissionIntersection(t *testing.T) {
	registry := tool.NewRegistry()
	if err := registry.Register(testPlanTool("search_code")); err != nil {
		t.Fatal(err)
	}
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, nil, ExecutorDirectTool, []tool.ToolID{"search_code"}, BudgetVector{ToolCalls: 1})); err != nil {
		t.Fatal(err)
	}
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.AllowedToolIDs = []tool.ToolID{"trace_calls"}
	_, err := (PlanCompiler{
		Catalog: catalog,
		Schemas: testSchemas(),
		Tools:   registry.Snapshot(tool.ReadPolicy()),
	}).CompileGenerated(contract)
	if err == nil || !errors.Is(err, ErrPlanInvalid) {
		t.Fatalf("permission error = %v", err)
	}
}

func TestPlanBudgetCheckIncludesAllTaskBudgets(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect-a", 1, nil, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1})); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(testTemplate("inspect-b", 1, nil, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1})); err != nil {
		t.Fatal(err)
	}
	contract := testContract(
		EvidenceGoal{ID: "g1", Kind: "flow", Required: true},
		EvidenceGoal{ID: "g2", Kind: "flow", Required: true},
	)
	_, err = (PlanCompiler{Catalog: catalog, Schemas: testSchemas(), Ledger: ledger}).CompileGenerated(contract)
	if err == nil || !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("plan budget error = %v", err)
	}
}

func testTemplate(id string, version int64, goalKinds []string, executor ExecutorType, grants []tool.ToolID, cost BudgetVector) TaskTemplate {
	return TaskTemplate{
		ID:           id,
		Version:      version,
		GoalKinds:    append([]string(nil), goalKinds...),
		ToolGrant:    append([]tool.ToolID(nil), grants...),
		InputSchema:  agentapi.SchemaRef{ID: "investigation.input", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "investigation.output", Version: 1},
		Executor:     executor,
		CostProfile:  cost,
		Enabled:      true,
	}
}

func testSchemas() *agentapi.SchemaRegistry {
	registry := agentapi.NewSchemaRegistry()
	definition := func(id string) agentapi.SchemaDefinition {
		return agentapi.SchemaDefinition{
			ID: id, Version: 1,
			Document: json.RawMessage(`{"type":"object"}`),
		}
	}
	if err := registry.Publish([]agentapi.SchemaDefinition{definition("investigation.input"), definition("investigation.output")}); err != nil {
		panic(err)
	}
	return registry
}

func testExecutors(executor TaskExecutor) ExecutorRegistry {
	return NewExecutorRegistry(map[ExecutorType]TaskExecutor{
		ExecutorDirectTool: executor, ExecutorToolPipeline: executor,
		ExecutorInvestigator: executor, ExecutorVerifier: executor,
		ExecutorComposer: executor,
	})
}

func testContract(goals ...EvidenceGoal) InvestigationContract {
	return InvestigationContract{ID: "contract-test", Question: "test question", Goals: goals}
}

func testPlanTool(id tool.ToolID) tool.Tool {
	return tool.Tool{
		ID:          id,
		Description: "test tool",
		Kind:        tool.KindRead,
		InputSchema: tool.JSONSchema{"type": "object", "properties": map[string]any{}},
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{Content: "ok"}, nil
		}),
	}
}
