package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestCodeInvestigationClosedLoop(t *testing.T) {
	registry := tool.NewRegistry()
	for _, id := range []tool.ToolID{"get_service", "search_code", "get_symbol", "trace_calls"} {
		candidate := tool.Tool{
			ID:          id,
			Description: string(id),
			Kind:        tool.KindRead,
			InputSchema: tool.JSONSchema{"type": "object", "properties": map[string]any{}},
			Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
				return tool.Result{Content: "evidence from " + string(id)}, nil
			}),
		}
		if err := registry.Register(candidate); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := registry.Snapshot(tool.ReadPolicy())

	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish([]agentapi.SchemaDefinition{
		{ID: DefaultTaskInputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
		{ID: DefaultTaskOutputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatal(err)
	}

	catalog := NewTaskTemplateCatalog()
	if err := RegisterCodeInvestigationTemplates(catalog); err != nil {
		t.Fatal(err)
	}

	toolExecutor := tool.NewExecutor(0)
	executors := NewExecutorRegistry(map[ExecutorType]TaskExecutor{
		ExecutorDirectTool:   DirectToolExecutor{Executor: toolExecutor, Snapshot: snapshot},
		ExecutorToolPipeline: ToolPipelineExecutor{Executor: toolExecutor, Snapshot: snapshot},
		ExecutorVerifier: TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, input TaskExecutionInput) (TaskExecutionResult, error) {
			if len(input.Evidence) == 0 {
				return TaskExecutionResult{}, errors.New("verifier received no upstream evidence")
			}
			unit := input.Evidence[0]
			claim := ClaimCandidate{
				GoalID: "g1",
				Text:   "the service exposes an AI call path",
				Status: ClaimSupported,
				EvidenceRefs: []EvidenceRef{{
					EvidenceID: unit.ID, SourceKind: unit.SourceKind, Target: unit.Target, ContentHash: unit.ContentHash,
				}},
			}
			return TaskExecutionResult{Output: []byte(`{"verified":true}`), Claims: []ClaimCandidate{claim}}, nil
		}),
	})

	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog:     catalog,
		Schemas:     schemas,
		Tools:       snapshot,
		Store:       NewMemoryRunStore(),
		Executors:   executors,
		BudgetLimit: BudgetVector{ToolCalls: 10},
		Composer: ComposerFunc(func(_ context.Context, _ InvestigationContract, report InvestigationReport) (AnswerDraft, error) {
			if len(report.Claims) == 0 {
				return AnswerDraft{}, errors.New("composer received no claims")
			}
			return AnswerDraft{Text: "the AI entry point is traced", Status: DeliverySucceeded, ClaimIDs: []string{report.Claims[0].ID}}, nil
		}),
	})

	contract := InvestigationContract{
		ID: "code-entrypoint", Question: "where does hsas-aiot-service call the AI model?",
		Goals:          []EvidenceGoal{{ID: "g1", Kind: GoalKindEntrypoint, Required: true}},
		AllowedToolIDs: []tool.ToolID{"get_service", "search_code", "get_symbol", "trace_calls"},
		CreatedAt:      time.Now().UTC(),
	}
	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunDelivered || run.Delivery == nil || run.Delivery.Status != DeliverySucceeded {
		t.Fatalf("run = %#v", run)
	}
	if len(run.Plan.Tasks) != 5 {
		t.Fatalf("plan task count = %d, want 5", len(run.Plan.Tasks))
	}
	if len(run.Report.Evidence) == 0 || len(run.Report.Claims) != 1 {
		t.Fatalf("report = %#v", run.Report)
	}
	if run.Report.Claims[0].Status != ClaimSupported {
		t.Fatalf("claim = %#v", run.Report.Claims[0])
	}
}

func TestCodeInvestigationCandidatesFormAcyclicDependencyChain(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterCodeInvestigationTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	candidates, err := catalog.GenerateCandidates(InvestigationContract{
		ID:       "contract",
		Question: "where is the AI entry point?",
		Goals:    []EvidenceGoal{{ID: "g1", Kind: GoalKindEntrypoint, Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 5 {
		t.Fatalf("candidate count = %d, want 5", len(candidates))
	}
	byTemplate := make(map[string]string, len(candidates))
	for _, candidate := range candidates {
		byTemplate[candidate.Template.ID] = candidate.ID
	}
	for _, want := range []struct {
		task string
		dep  string
	}{
		{"code.search", "workspace.resolve_entity"},
		{"code.inspect_symbol", "code.search"},
		{"code.trace_calls", "code.inspect_symbol"},
	} {
		taskID := byTemplate[want.task]
		depID := byTemplate[want.dep]
		actual := candidateDependencies(candidates, taskID)
		if len(actual) != 1 || actual[0] != depID {
			t.Fatalf("task %s dependencies = %#v, want %s", want.task, actual, want.dep)
		}
	}
}

func candidateDependencies(candidates []TaskCandidate, id string) []string {
	for _, candidate := range candidates {
		if candidate.ID == id {
			return candidate.Dependencies
		}
	}
	return nil
}
