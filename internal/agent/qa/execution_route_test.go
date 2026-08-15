package qa

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

func TestDecideExecutionRoute(t *testing.T) {
	base := executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent, Complexity: 0.9, Confidence: 0.9,
			Tasks: []retrieval.ExecutionTask{
				{
					ID: "design", Objective: "Establish the intended behavior.",
					IndependentlyUseful: true,
				},
				{
					ID: "implementation", Objective: "Verify the implementation behavior.",
					IndependentlyUseful: true,
				},
			},
			Reasons: []string{"requires_multiple_subproblems", "supports_parallel_investigation"},
		},
		Assessment: ExecutionAssessment{
			Strategy:             retrieval.ExecutionMultiAgent,
			IndependentTaskCount: 2, RequiredCapabilities: 2,
			Parallelizable: true,
		},
		Policy:            ExecutionPolicy{AllowMultiAgent: true},
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
		}, want: retrieval.ExecutionMultiAgent},
		{name: "policy disabled", mutate: func(input *executionRouteInput) {
			input.Policy.AllowMultiAgent = false
		}, want: retrieval.ExecutionSingleAgent, wantReason: "policy_disallows_multi_agent"},
		{name: "low complexity remains eligible", mutate: func(input *executionRouteInput) {
			input.Suggestion.Complexity = 0.69
		}, want: retrieval.ExecutionMultiAgent},
		{name: "low confidence remains eligible", mutate: func(input *executionRouteInput) {
			input.Suggestion.Confidence = 0.79
		}, want: retrieval.ExecutionMultiAgent},
		{name: "one independent task", mutate: func(input *executionRouteInput) {
			input.Assessment.IndependentTaskCount = 1
			input.Assessment.Parallelizable = false
		}, want: retrieval.ExecutionSingleAgent, wantReason: "insufficient_independent_tasks"},
		{name: "one investigation capability", mutate: func(input *executionRouteInput) {
			input.Assessment.RequiredCapabilities = 1
		}, want: retrieval.ExecutionMultiAgent},
		{name: "sequential tasks", mutate: func(input *executionRouteInput) {
			input.Assessment.Parallelizable = false
		}, want: retrieval.ExecutionSingleAgent, wantReason: "tasks_not_parallelizable"},
		{name: "write request", mutate: func(input *executionRouteInput) {
			input.WriteRequested = true
		}, want: retrieval.ExecutionSingleAgent, wantReason: "write_requested"},
		{name: "write authorization remains eligible", mutate: func(input *executionRouteInput) {
			input.WriteAuthorized = true
		}, want: retrieval.ExecutionMultiAgent},
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
			Tasks: []retrieval.ExecutionTask{
				{
					ID: "design", Objective: "Establish the intended behavior.",
					IndependentlyUseful: true,
				},
				{
					ID: "implementation", Objective: "Verify the implementation behavior.",
					IndependentlyUseful: true,
				},
			},
			Reasons: []string{"supports_parallel_investigation"},
		},
		Assessment: ExecutionAssessment{
			Strategy:             retrieval.ExecutionMultiAgent,
			IndependentTaskCount: 2, RequiredCapabilities: 2,
			Parallelizable: true,
			EstimatedCoordination: CoordinationEstimate{
				AgentRuns: 3, JoinInputs: 2,
			},
		},
		Policy: ExecutionPolicy{AllowMultiAgent: true},
	})
	if decision.DowngradeReason != "workflow_unavailable" {
		t.Fatalf("decision = %+v", decision)
	}
	event := traceEventByNode(t, scope.Snapshot(), "execution_route")
	if event.Output["proposed_strategy"] != retrieval.ExecutionMultiAgent ||
		event.Output["effective_strategy"] != retrieval.ExecutionSingleAgent ||
		event.Output["downgrade_reason"] != "workflow_unavailable" ||
		event.Output["workflow_available"] != false ||
		event.Output["independent_tasks"] != 2 ||
		event.Output["required_capabilities"] != 2 ||
		event.Output["estimated_agent_runs"] != 3 ||
		event.Output["read_only"] != true ||
		event.Output["write_authorized"] != false ||
		event.Output["write_requested"] != false ||
		event.Output["decision_origin"] != "server_policy" {
		t.Fatalf("output = %#v", event.Output)
	}
}

func TestDecideExecutionRoutePromotesModelSingleSuggestion(t *testing.T) {
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{Strategy: retrieval.ExecutionSingleAgent},
		Assessment: ExecutionAssessment{
			IndependentTaskCount: 2, RequiredCapabilities: 1,
			Parallelizable: true,
		},
		Policy: ExecutionPolicy{AllowMultiAgent: true}, WorkflowAvailable: true,
	})
	if decision.Strategy != retrieval.ExecutionMultiAgent ||
		decision.DecisionOrigin != "server_assessment" ||
		decision.PromotionReason != "independent_task_decomposition" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestExecutionRouteTraceKeepsUnauthorizedWriteRequestReadOnly(t *testing.T) {
	scope := runtrace.NewScope(runtrace.Evaluation, nil)
	ctx := runtrace.WithScope(t.Context(), scope)
	routeExecution(ctx, executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent,
		},
		Assessment: ExecutionAssessment{
			IndependentTaskCount: 2, RequiredCapabilities: 2,
			Parallelizable: true,
		},
		Policy:         ExecutionPolicy{AllowMultiAgent: true},
		WriteRequested: true, WorkflowAvailable: true,
	})

	event := traceEventByNode(t, scope.Snapshot(), "execution_route")
	if event.Output["read_only"] != true ||
		event.Output["write_authorized"] != false ||
		event.Output["write_requested"] != true {
		t.Fatalf("output = %#v", event.Output)
	}
}

func TestAssessExecutionSeparatesGoalsFromCapabilities(t *testing.T) {
	assessment := assessExecution(
		retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent,
			Reasons:  []string{"requires_cross_source_analysis"},
		},
		TaskContract{
			Entities: []EntityRef{{ID: "Checkout.Place"}, {ID: "Inventory.Reserve"}},
			InvestigationGoals: []InvestigationGoal{
				{
					ID: "design", Objective: "Establish the intended behavior.",
					IndependentlyUseful: true,
				},
				{
					ID: "implementation", Objective: "Verify the implementation behavior.",
					IndependentlyUseful: true,
				},
			},
			EvidenceGoals: []EvidenceGoal{
				{
					ID: "core_flow", Facet: "core_flow", Required: true,
					Sources: []agentapi.EvidenceSource{
						agentapi.EvidenceSourceInternal,
						agentapi.EvidenceSourceRuntime,
					},
				},
				{
					ID: "documentation", Facet: "documentation", Required: true,
					Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
				},
			},
		},
	)
	if assessment.Strategy != retrieval.ExecutionMultiAgent ||
		assessment.IndependentTaskCount != 2 ||
		assessment.RequiredCapabilities != 3 ||
		!assessment.Parallelizable ||
		!assessment.SharedContextPressure ||
		assessment.EstimatedCoordination.AgentRuns != 3 ||
		assessment.EstimatedCoordination.JoinInputs != 2 {
		t.Fatalf("assessment = %+v", assessment)
	}
}

func TestAssessExecutionDoesNotTreatCapabilityFanoutAsIndependentTasks(t *testing.T) {
	assessment := assessExecution(
		retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent,
			Reasons:  []string{"requires_cross_source_analysis"},
		},
		TaskContract{
			InvestigationGoals: []InvestigationGoal{{
				ID: "flow", Objective: "Verify the end-to-end flow.",
				IndependentlyUseful: true,
			}},
			EvidenceGoals: []EvidenceGoal{{
				ID: "core_flow", Facet: "core_flow", Required: true,
				Sources: []agentapi.EvidenceSource{
					agentapi.EvidenceSourceInternal,
					agentapi.EvidenceSourceRuntime,
					agentapi.EvidenceSourceWeb,
				},
			}},
		},
	)
	if assessment.IndependentTaskCount != 1 ||
		assessment.RequiredCapabilities != 3 ||
		assessment.Parallelizable ||
		assessment.Strategy != retrieval.ExecutionSingleAgent {
		t.Fatalf("assessment = %+v", assessment)
	}
}

func TestAssessExecutionExcludesDependentAndNonUsefulGoals(t *testing.T) {
	assessment := assessExecution(
		retrieval.ExecutionSuggestion{Strategy: retrieval.ExecutionMultiAgent},
		TaskContract{InvestigationGoals: []InvestigationGoal{
			{ID: "independent", IndependentlyUseful: true},
			{ID: "dependent", IndependentlyUseful: true, DependsOn: []string{"independent"}},
			{ID: "partial", IndependentlyUseful: false},
		}},
	)
	if assessment.IndependentTaskCount != 1 || assessment.Parallelizable ||
		assessment.Strategy != retrieval.ExecutionSingleAgent {
		t.Fatalf("assessment = %+v", assessment)
	}
}
