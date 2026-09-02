package qa

import (
	"context"
	"reflect"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

type capturingManagedRuntime struct {
	start agentapi.RunStart
}

func (runtime *capturingManagedRuntime) Run(
	context.Context,
	agentapi.RunRequest,
) (agentapi.RunResult, error) {
	return agentapi.RunResult{}, nil
}

func (runtime *capturingManagedRuntime) Begin(
	_ context.Context,
	start agentapi.RunStart,
) (agentapi.ManagedRun, error) {
	runtime.start = start
	return nil, nil
}

func TestQueryAnalysisTraceCarriesDerivedDiagnostics(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := runtrace.WithScope(t.Context(), runtrace.NewScope(runtrace.Evaluation, func(event domain.EvaluationTrace) {
		events = append(events, event)
	}))
	analysis, err := analyzeQuery(ctx, queryAnalysisInput{
		Question:       "分别比较 Domestic.Control 和 Overseas.Control 的差异",
		CleanQuestion:  "分别比较 Domestic.Control 和 Overseas.Control 的差异",
		QuerySemantics: &domain.QuerySemantics{Kind: domain.QueryComparison},
		Terms: retrieval.QueryTerms{
			Identifiers: []string{"Domestic.Control", "Overseas.Control"},
			DomainTerms: []string{"比较"},
		},
	})
	if err != nil {
		t.Fatalf("analyzeQuery: %v", err)
	}
	if analysis.QueryPlan.Kind != domain.QueryComparison {
		t.Fatalf("query kind = %q, want %q", analysis.QueryPlan.Kind, domain.QueryComparison)
	}
	if len(events) != 1 || events[0].Node != "query_analysis" {
		t.Fatalf("events = %#v", events)
	}
	output := events[0].Output
	if output["query_kind"] != domain.QueryComparison ||
		output["resolution_origin"] != domain.QueryResolutionPlanner ||
		output["matched_rule_kind"] != domain.QueryKind("") ||
		output["entity_count"] != 2 {
		t.Fatalf("query analysis trace = %#v", output)
	}
	facets, ok := output["required_facets"].([]string)
	if !ok || len(facets) != len(domain.RequiredFacetsFor(domain.QueryComparison)) {
		t.Fatalf("required facets = %#v", output["required_facets"])
	}
}

func TestFlowOutputContractCarriesBoundedSubjects(t *testing.T) {
	contract := outputContractForQuery(domain.QueryPlan{
		Kind:     domain.QueryFlow,
		Entities: []string{"fallback", "Second"},
		EntitySpecs: []domain.EntitySpec{
			{ID: "first", Label: "First"},
			{ID: "second", Label: "Second"},
			{ID: "first-duplicate", Label: "first"},
		},
	})
	if contract.Kind != "flow" || !contract.RequireMermaid || contract.MaxHops != 6 {
		t.Fatalf("flow output contract = %+v", contract)
	}
	if got, want := contract.Subjects, []string{"First", "Second", "fallback"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("subjects = %v, want %v", got, want)
	}
	if got := outputContractForQuery(domain.QueryPlan{Kind: domain.QueryComparison}); !reflect.DeepEqual(got, agentapi.RunOutputContract{}) {
		t.Fatalf("non-flow contract = %+v", got)
	}
}

func TestBeginSingleRunPinsFlowOutputContract(t *testing.T) {
	runtime := &capturingManagedRuntime{}
	service := &Service{runtime: runtime}
	query := domain.QueryPlan{
		Kind: domain.QueryFlow,
		EntitySpecs: []domain.EntitySpec{
			{ID: "rgb", Label: "RGB 灯效"},
			{ID: "tts", Label: "TTS"},
		},
	}
	prepared := &preparation{
		ctx: context.Background(),
		request: Request{
			RunID:    "flow-run",
			Question: "分析 RGB 灯效和 TTS 流程",
			UserID:   1,
		},
		candidateToolSet: compactionToolSet{},
		analysis:         queryAnalysisOutput{QueryPlan: query},
		runLimits: agentapi.RunLimits{
			Deadline: time.Now().Add(time.Minute),
			MaxSteps: 8,
		},
	}
	definition := agentapi.Definition{
		ID: "qa.answerer", Version: 1, ContentHash: "definition-hash",
	}

	if _, err := service.beginSingleRun(prepared, definition, agentapi.DefinitionSelection{}); err != nil {
		t.Fatalf("beginSingleRun: %v", err)
	}

	want := outputContractForQuery(query)
	if !reflect.DeepEqual(runtime.start.Policy.OutputContract, want) {
		t.Fatalf("begin output contract = %+v, want %+v", runtime.start.Policy.OutputContract, want)
	}
}
