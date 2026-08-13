package qa

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

func TestDecideExecutionRoute(t *testing.T) {
	base := executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent, Complexity: 0.9, Confidence: 0.9,
			Reasons: []string{"requires_multiple_subproblems", "supports_parallel_investigation"},
		},
		Policy:            ExecutionPolicy{AllowMultiAgent: true, MinComplexity: 0.7, MinConfidence: 0.8},
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
	scope := runtrace.NewScope(runtrace.Evaluation, nil)
	ctx := runtrace.WithScope(t.Context(), scope)
	decision := routeExecution(ctx, executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent, Complexity: 0.9, Confidence: 0.9,
			Reasons: []string{"supports_parallel_investigation"},
		},
		Policy: ExecutionPolicy{AllowMultiAgent: true, MinComplexity: 0.7, MinConfidence: 0.8},
	})
	if decision.DowngradeReason != "workflow_unavailable" {
		t.Fatalf("decision = %+v", decision)
	}
	event := traceEventByNode(t, scope.Snapshot(), "execution_route")
	if event.Output["proposed_strategy"] != retrieval.ExecutionMultiAgent ||
		event.Output["effective_strategy"] != retrieval.ExecutionSingleAgent ||
		event.Output["downgrade_reason"] != "workflow_unavailable" ||
		event.Output["workflow_available"] != false || event.Output["read_only"] != true {
		t.Fatalf("output = %#v", event.Output)
	}
}
