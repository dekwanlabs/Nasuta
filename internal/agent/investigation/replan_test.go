package investigation

import (
	"context"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"testing"
	"time"
)

func TestCoordinatorReplansWhenRequiredGoalRemainsUnresolved(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("a_first", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(testTemplate("z_fallback", 1, []string{"flow"}, ExecutorVerifier, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}

	evidenceCandidate := EvidenceCandidate{SourceKind: "code", Target: "service-a", Content: "verified alternative path"}
	normalized, err := normalizeEvidence("", evidenceCandidate)
	if err != nil {
		t.Fatal(err)
	}
	claim := ClaimCandidate{
		GoalID:       "g1",
		Text:         "the alternative path is verified",
		Status:       ClaimSupported,
		EvidenceRefs: []EvidenceRef{{EvidenceID: normalized.ID, SourceKind: normalized.SourceKind, Target: normalized.Target, ContentHash: normalized.ContentHash}},
	}
	executor := TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		if task.Template.ID == "z_fallback" {
			return TaskExecutionResult{
				Output:             []byte(`{"verified":true}`),
				EvidenceCandidates: []EvidenceCandidate{evidenceCandidate},
				Claims:             []ClaimCandidate{claim},
			}, nil
		}
		return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
	})
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog:        catalog,
		Schemas:        testSchemas(),
		Store:          NewMemoryRunStore(),
		Executors:      testExecutors(executor),
		BudgetLimit:    BudgetVector{},
		MaxRounds:      2,
		MaxTasks:       1,
		MaxParallelism: 1,
		Composer: ComposerFunc(func(_ context.Context, _ InvestigationContract, report InvestigationReport) (AnswerDraft, error) {
			return AnswerDraft{Text: "alternative answer", Status: DeliverySucceeded, ClaimIDs: []string{report.Claims[0].ID}}, nil
		}),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()

	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunDelivered || run.Delivery == nil || run.Delivery.Status != DeliverySucceeded {
		t.Fatalf("run = %#v", run)
	}
	if run.Plan.Revision != 2 || len(run.Plan.Tasks) != 1 || run.Plan.Tasks[0].Template.ID != "z_fallback" {
		t.Fatalf("final plan = %#v", run.Plan)
	}
	if len(run.Report.Claims) != 1 || run.Report.Claims[0].Status != ClaimSupported {
		t.Fatalf("report = %#v", run.Report)
	}
}

func TestSelectReplanCandidatesPrefersCoverageAndKeepsBudgetBound(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	for _, template := range []TaskTemplate{
		testTemplate("cheap-one", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1}),
		testTemplate("cheap-two", 1, []string{"state"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1}),
		testTemplate("shared", 1, []string{"flow", "state"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 3}),
	} {
		if err := catalog.Register(template); err != nil {
			t.Fatal(err)
		}
	}
	contract := testContract(
		EvidenceGoal{ID: "g-flow", Kind: "flow", Required: true},
		EvidenceGoal{ID: "g-state", Kind: "state", Required: true},
	)
	candidates := []TaskCandidate{
		{ID: "task_cheap-one_v1", Template: TaskTemplateRef{ID: "cheap-one", Version: 1}, EvidenceGoalIDs: []string{"g-flow"}, Budget: BudgetVector{ToolCalls: 1}},
		{ID: "task_cheap-two_v1", Template: TaskTemplateRef{ID: "cheap-two", Version: 1}, EvidenceGoalIDs: []string{"g-state"}, Budget: BudgetVector{ToolCalls: 1}},
		{ID: "task_shared_v1", Template: TaskTemplateRef{ID: "shared", Version: 1}, EvidenceGoalIDs: []string{"g-flow", "g-state"}, Budget: BudgetVector{ToolCalls: 3}},
	}
	selected := selectReplanCandidates(
		catalog, contract, candidates,
		map[string]struct{}{"g-flow": {}, "g-state": {}}, nil, 1,
		BudgetVector{ToolCalls: 3}, nil,
	)
	if len(selected) != 1 || selected[0].ID != "task_shared_v1" {
		t.Fatalf("selected candidates = %#v", selected)
	}

	selected = selectReplanCandidates(
		catalog, contract, candidates,
		map[string]struct{}{"g-flow": {}, "g-state": {}}, nil, 2,
		BudgetVector{ToolCalls: 2}, nil,
	)
	if len(selected) != 2 || selected[0].ID != "task_cheap-one_v1" || selected[1].ID != "task_cheap-two_v1" {
		t.Fatalf("budget-bound candidates = %#v", selected)
	}
}

func TestSelectReplanCandidatesMatchesRequiredSource(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	internal := testTemplate("a-internal", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1})
	internal.SourceKinds = []string{"internal"}
	runtime := testTemplate("z-runtime", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1})
	runtime.SourceKinds = []string{"runtime"}
	if err := catalog.Register(internal); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(runtime); err != nil {
		t.Fatal(err)
	}
	contract := testContract(EvidenceGoal{
		ID: "g1", Kind: "flow", Required: true,
		Sources:         []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime},
		RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime},
	})
	selected := selectReplanCandidates(
		catalog, contract,
		[]TaskCandidate{
			{ID: "task_a-internal_v1", Template: TaskTemplateRef{ID: "a-internal", Version: 1}, EvidenceGoalIDs: []string{"g1"}, Budget: BudgetVector{ToolCalls: 1}},
			{ID: "task_z-runtime_v1", Template: TaskTemplateRef{ID: "z-runtime", Version: 1}, EvidenceGoalIDs: []string{"g1"}, Budget: BudgetVector{ToolCalls: 1}},
		},
		map[string]struct{}{"g1": {}}, nil, 1, BudgetVector{ToolCalls: 1}, nil,
	)
	if len(selected) != 1 || selected[0].ID != "task_z-runtime_v1" {
		t.Fatalf("source-ranked candidates = %#v", selected)
	}
}

func TestSelectReplanCandidatesTreatsExecutedDependenciesAsSatisfied(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	provider := testTemplate("provider", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1})
	provider.Provides = []string{"symbol"}
	consumer := testTemplate("consumer", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1})
	consumer.RequiredInputs = []string{"symbol"}
	if err := catalog.Register(provider); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(consumer); err != nil {
		t.Fatal(err)
	}
	selected := selectReplanCandidates(
		catalog, testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true}),
		[]TaskCandidate{{
			ID: "task_consumer_v1", Template: TaskTemplateRef{ID: "consumer", Version: 1},
			EvidenceGoalIDs: []string{"g1"}, Dependencies: []string{"task_provider_v1"}, Budget: BudgetVector{ToolCalls: 1},
		}},
		map[string]struct{}{"g1": {}},
		map[string]struct{}{"task_provider_v1": {}},
		1, BudgetVector{ToolCalls: 1}, nil,
	)
	if len(selected) != 1 || selected[0].ID != "task_consumer_v1" {
		t.Fatalf("executed dependency candidates = %#v", selected)
	}
}
