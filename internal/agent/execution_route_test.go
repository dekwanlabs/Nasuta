package agent

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

func TestDecideExecutionRoute(t *testing.T) {
	base := executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent, Complexity: 0.9, Confidence: 0.9,
			Reasons: []string{"requires_multiple_subproblems", "supports_parallel_investigation"},
		},
		Policy:            ExecutionPolicy{AllowMultiAgent: true, MinComplexity: 0.7, MinConfidence: 0.8},
		EvidencePlan:      domain.EvidencePlan{Sources: domain.Internal},
		WorkflowAvailable: true,
	}
	tests := []struct {
		name       string
		mutate     func(*executionRouteInput)
		want       retrieval.ExecutionStrategy
		wantReason string
	}{
		{name: "eligible", want: retrieval.ExecutionMultiAgent},
		{name: "model selects single", mutate: func(input *executionRouteInput) {
			input.Suggestion.Strategy = retrieval.ExecutionSingleAgent
		}, want: retrieval.ExecutionSingleAgent},
		{name: "policy disabled", mutate: func(input *executionRouteInput) {
			input.Policy.AllowMultiAgent = false
		}, want: retrieval.ExecutionSingleAgent, wantReason: "policy_disallows_multi_agent"},
		{name: "low complexity", mutate: func(input *executionRouteInput) {
			input.Suggestion.Complexity = 0.69
		}, want: retrieval.ExecutionSingleAgent, wantReason: "complexity_below_threshold"},
		{name: "low confidence", mutate: func(input *executionRouteInput) {
			input.Suggestion.Confidence = 0.79
		}, want: retrieval.ExecutionSingleAgent, wantReason: "confidence_below_threshold"},
		{name: "write request", mutate: func(input *executionRouteInput) {
			input.AllowWrite = true
		}, want: retrieval.ExecutionSingleAgent, wantReason: "write_requested"},
		{name: "mixed evidence", mutate: func(input *executionRouteInput) {
			input.EvidencePlan.Sources = domain.Internal | domain.Web
		}, want: retrieval.ExecutionSingleAgent, wantReason: "evidence_not_internal_only"},
		{name: "temporal tool", mutate: func(input *executionRouteInput) {
			input.ToolCandidates = []retrieval.ToolRouteCandidate{{ID: "logs", Temporal: true}}
			input.RoutedToolIDs = []string{"logs"}
		}, want: retrieval.ExecutionSingleAgent, wantReason: "runtime_evidence_required"},
		{name: "history dependency", mutate: func(input *executionRouteInput) {
			input.HistoryValid = true
			input.History.NeedsPriorEvidence = true
		}, want: retrieval.ExecutionSingleAgent, wantReason: "history_dependency_required"},
		{name: "workflow unavailable", mutate: func(input *executionRouteInput) {
			input.WorkflowAvailable = false
		}, want: retrieval.ExecutionSingleAgent, wantReason: "workflow_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			if test.mutate != nil {
				test.mutate(&input)
			}
			got := decideExecutionRoute(input)
			if got.Strategy != test.want || got.DowngradeReason != test.wantReason {
				t.Fatalf("decision = %+v, want strategy=%q reason=%q", got, test.want, test.wantReason)
			}
		})
	}
}

func TestExecutionRouteTraceContract(t *testing.T) {
	scope := executiontrace.NewScope(executiontrace.Evaluation, nil)
	ctx := executiontrace.WithScope(t.Context(), scope)
	decision := routeExecution(ctx, executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent, Complexity: 0.9, Confidence: 0.9,
			Reasons: []string{"supports_parallel_investigation"},
		},
		Policy:       ExecutionPolicy{AllowMultiAgent: true, MinComplexity: 0.7, MinConfidence: 0.8},
		EvidencePlan: domain.EvidencePlan{Sources: domain.Internal},
	})
	if decision.DowngradeReason != "workflow_unavailable" {
		t.Fatalf("decision = %+v", decision)
	}
	event := traceEventByNode(t, scope.Snapshot(), "execution_route")
	if event.Output["proposed_strategy"] != retrieval.ExecutionMultiAgent ||
		event.Output["effective_strategy"] != retrieval.ExecutionSingleAgent ||
		event.Output["downgrade_reason"] != "workflow_unavailable" ||
		event.Output["internal_only"] != true || event.Output["read_only"] != true {
		t.Fatalf("output = %#v", event.Output)
	}
}
