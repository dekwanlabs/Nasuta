package qa

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestDecideExecutionRouteUsesParentDelegationForComplexSuggestion(t *testing.T) {
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy:   retrieval.ExecutionMultiAgent,
			Complexity: 0.95,
			Tasks: []retrieval.ExecutionTask{
				{ID: "service-a", Objective: "Inspect service A.", IndependentlyUseful: true},
				{ID: "service-b", Objective: "Inspect service B.", IndependentlyUseful: true},
			},
		},
		DelegationAvailable: true,
		DelegationToolReady: true,
	})

	if decision.Path != executionPathSingle {
		t.Fatalf("path = %q, want %q", decision.Path, executionPathSingle)
	}
	if decision.Strategy != retrieval.ExecutionSingleAgent {
		t.Fatalf("strategy = %q, want %q", decision.Strategy, retrieval.ExecutionSingleAgent)
	}
	if decision.RouteReason != routeReasonParentDynamicDelegation {
		t.Fatalf("route reason = %q, want %q", decision.RouteReason, routeReasonParentDynamicDelegation)
	}
	if decision.DowngradeReason != "" {
		t.Fatalf("downgrade reason = %q, want empty", decision.DowngradeReason)
	}
}

func TestDecideExecutionRouteAllowsParallelMultiAgentSuggestion(t *testing.T) {
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent,
			Tasks: []retrieval.ExecutionTask{
				{ID: "service-a", Objective: "Inspect service A.", IndependentlyUseful: true},
				{ID: "service-b", Objective: "Inspect service B.", IndependentlyUseful: true},
			},
		},
		DelegationAvailable: true,
		DelegationToolReady: true,
	})

	if decision.RouteReason != routeReasonParentDynamicDelegation || decision.DowngradeReason != "" {
		t.Fatalf("decision = %+v, want parent dynamic delegation without downgrade", decision)
	}
}

func TestDecideExecutionRouteRejectsSerializedDelegation(t *testing.T) {
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent,
			Tasks: []retrieval.ExecutionTask{
				{ID: "service-a", Objective: "Inspect service A.", IndependentlyUseful: true},
				{ID: "service-b", Objective: "Inspect service B.", IndependentlyUseful: true},
			},
		},
		DelegationAvailable:     true,
		DelegationToolReady:     true,
		DelegationMaxConcurrent: 1,
	})

	if decision.Path != executionPathSingle || decision.Strategy != retrieval.ExecutionSingleAgent {
		t.Fatalf("decision = %+v, want single-agent route", decision)
	}
	if decision.RouteReason != routeReasonDelegationConcurrencyTooLow ||
		decision.DowngradeReason != routeReasonDelegationConcurrencyTooLow {
		t.Fatalf("decision = %+v, want concurrency downgrade", decision)
	}
}

func TestDecideExecutionRouteAllowsConfiguredParallelDelegation(t *testing.T) {
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent,
			Tasks: []retrieval.ExecutionTask{
				{ID: "service-a", Objective: "Inspect service A.", IndependentlyUseful: true},
				{ID: "service-b", Objective: "Inspect service B.", IndependentlyUseful: true},
			},
		},
		DelegationAvailable:     true,
		DelegationToolReady:     true,
		DelegationMaxConcurrent: 2,
	})

	if decision.RouteReason != routeReasonParentDynamicDelegation || decision.DowngradeReason != "" {
		t.Fatalf("decision = %+v, want parallel delegation", decision)
	}
}

func TestDecideExecutionRouteKeepsSingleAgentSuggestionSingle(t *testing.T) {
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy:   retrieval.ExecutionSingleAgent,
			Complexity: 0.1,
		},
		DelegationAvailable: true,
		DelegationToolReady: true,
	})

	if decision.Path != executionPathSingle || decision.Strategy != retrieval.ExecutionSingleAgent {
		t.Fatalf("decision = %+v, want single-agent route", decision)
	}
	if decision.RouteReason != routeReasonSingleAgentSuggestion || decision.DowngradeReason != "" {
		t.Fatalf("decision = %+v, want single-agent suggestion without downgrade", decision)
	}
}

func TestDecideExecutionRouteRejectsMultiAgentWithoutParallelBenefit(t *testing.T) {
	cases := []struct {
		name  string
		tasks []retrieval.ExecutionTask
	}{
		{
			name:  "one task",
			tasks: []retrieval.ExecutionTask{{ID: "one", Objective: "Inspect one subject.", IndependentlyUseful: true}},
		},
		{
			name: "sequential tasks",
			tasks: []retrieval.ExecutionTask{
				{ID: "one", Objective: "Inspect the first subject.", IndependentlyUseful: true},
				{ID: "two", Objective: "Use the first result.", IndependentlyUseful: true, DependsOn: []string{"one"}},
			},
		},
		{
			name: "only one independent task",
			tasks: []retrieval.ExecutionTask{
				{ID: "one", Objective: "Inspect the first subject.", IndependentlyUseful: true},
				{ID: "two", Objective: "Inspect the dependent subject.", IndependentlyUseful: true, DependsOn: []string{"one"}},
				{ID: "three", Objective: "Inspect another dependent subject.", IndependentlyUseful: false, DependsOn: []string{"one"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := decideExecutionRoute(executionRouteInput{
				Suggestion: retrieval.ExecutionSuggestion{
					Strategy: retrieval.ExecutionMultiAgent,
					Tasks:    tc.tasks,
				},
				DelegationAvailable: true,
				DelegationToolReady: true,
			})
			if decision.Path != executionPathSingle || decision.Strategy != retrieval.ExecutionSingleAgent {
				t.Fatalf("decision = %+v, want single-agent route", decision)
			}
			if decision.RouteReason != routeReasonMultiAgentNotWorthwhile ||
				decision.DowngradeReason != routeReasonMultiAgentNotWorthwhile {
				t.Fatalf("decision = %+v, want multi-agent-not-worthwhile downgrade", decision)
			}
		})
	}
}

func TestDecideExecutionRouteFallsBackWhenDelegationUnavailable(t *testing.T) {
	cases := []struct {
		name                string
		delegationAvailable bool
		toolReady           bool
	}{
		{name: "feature disabled", delegationAvailable: false, toolReady: true},
		{name: "tool not visible", delegationAvailable: true, toolReady: false},
		{name: "feature disabled and tool missing", delegationAvailable: false, toolReady: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := decideExecutionRoute(executionRouteInput{
				Suggestion: retrieval.ExecutionSuggestion{
					Strategy: retrieval.ExecutionMultiAgent,
					Tasks:    []retrieval.ExecutionTask{{ID: "investigate", Objective: "Inspect the system.", IndependentlyUseful: true}},
				},
				DelegationAvailable: tc.delegationAvailable,
				DelegationToolReady: tc.toolReady,
			})
			if decision.Path != executionPathSingle || decision.Strategy != retrieval.ExecutionSingleAgent {
				t.Fatalf("decision = %+v, want single-agent fallback", decision)
			}
			if decision.RouteReason != routeReasonDelegationUnavailable ||
				decision.DowngradeReason != routeReasonDelegationUnavailable {
				t.Fatalf("decision = %+v, want delegation-unavailable fallback", decision)
			}
		})
	}
}

func TestDecideExecutionRouteKeepsWriteRequestsOnParent(t *testing.T) {
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion: retrieval.ExecutionSuggestion{
			Strategy: retrieval.ExecutionMultiAgent,
			Tasks: []retrieval.ExecutionTask{
				{ID: "change-a", Objective: "Change A.", IndependentlyUseful: true},
				{ID: "change-b", Objective: "Change B.", IndependentlyUseful: true},
			},
		},
		WriteRequested:      true,
		DelegationAvailable: true,
		DelegationToolReady: true,
	})

	if decision.Path != executionPathSingle || decision.Strategy != retrieval.ExecutionSingleAgent {
		t.Fatalf("decision = %+v, write requests must remain on the parent run", decision)
	}
	if decision.RouteReason != routeReasonWriteRequested || decision.DowngradeReason != routeReasonWriteRequested {
		t.Fatalf("decision = %+v, want write-requested route", decision)
	}
}

func TestApplyExecutionRouteMarksRiskButNeverCreatesWorkflow(t *testing.T) {
	svc := &Service{delegationEnabled: true}
	prepared := &preparation{
		ctx:              context.Background(),
		request:          Request{RunID: "complex-parent-run", Question: "Inspect all services and compare their failure paths."},
		candidateToolSet: compactionToolSet{tools: []tool.Tool{{ID: "delegate_investigation"}}},
		planning: evidencePlanningOutput{
			Execution: retrieval.ExecutionSuggestion{
				Strategy: retrieval.ExecutionMultiAgent,
				Reasons:  []string{"requires_risk_sensitive_analysis"},
				Tasks: []retrieval.ExecutionTask{
					{ID: "service-a", Objective: "Inspect service A.", IndependentlyUseful: true},
					{ID: "service-b", Objective: "Inspect service B.", IndependentlyUseful: true},
				},
			},
		},
	}

	svc.applyExecutionRoute(prepared)

	if prepared.execution.Path != executionPathSingle ||
		prepared.execution.Strategy != retrieval.ExecutionSingleAgent ||
		prepared.execution.RouteReason != routeReasonParentDynamicDelegation {
		t.Fatalf("execution = %+v, want normal parent run with dynamic delegation available", prepared.execution)
	}
	if !prepared.execution.HighRisk {
		t.Fatalf("execution = %+v, risk signal was not preserved", prepared.execution)
	}
}

func TestApplyExecutionRouteHidesDelegationWhenConcurrencyIsOne(t *testing.T) {
	svc := &Service{delegationEnabled: true, delegationMaxConcurrent: 1}
	prepared := &preparation{
		ctx:              context.Background(),
		request:          Request{RunID: "serialized-route", Question: "Inspect two services."},
		candidateToolSet: compactionToolSet{tools: []tool.Tool{{ID: "delegate_investigation"}, {ID: "search_code"}}},
		planning: evidencePlanningOutput{
			Execution: retrieval.ExecutionSuggestion{
				Strategy: retrieval.ExecutionMultiAgent,
				Tasks: []retrieval.ExecutionTask{
					{ID: "service-a", Objective: "Inspect service A.", IndependentlyUseful: true},
					{ID: "service-b", Objective: "Inspect service B.", IndependentlyUseful: true},
				},
			},
		},
	}

	svc.applyExecutionRoute(prepared)

	if prepared.execution.RouteReason != routeReasonDelegationConcurrencyTooLow {
		t.Fatalf("execution = %+v, want concurrency downgrade", prepared.execution)
	}
	if scenarioToolsContain(prepared.candidateToolSet, "delegate_investigation") {
		t.Fatalf("candidate tools = %+v, delegation tool should be hidden", prepared.candidateToolSet)
	}
	if !scenarioToolsContain(prepared.candidateToolSet, "search_code") {
		t.Fatalf("candidate tools = %+v, non-delegation tool was removed", prepared.candidateToolSet)
	}
}

func TestApplyExecutionRouteHidesDelegationForNonParallelSuggestion(t *testing.T) {
	svc := &Service{delegationEnabled: true}
	prepared := &preparation{
		ctx:              context.Background(),
		request:          Request{RunID: "single-route", Question: "Inspect one service."},
		candidateToolSet: compactionToolSet{tools: []tool.Tool{{ID: "delegate_investigation"}, {ID: "search_code"}}},
		planning: evidencePlanningOutput{
			Execution: retrieval.ExecutionSuggestion{
				Strategy: retrieval.ExecutionSingleAgent,
				Tasks:    []retrieval.ExecutionTask{{ID: "service", Objective: "Inspect one service.", IndependentlyUseful: true}},
			},
		},
	}

	svc.applyExecutionRoute(prepared)

	if scenarioToolsContain(prepared.candidateToolSet, "delegate_investigation") {
		t.Fatalf("candidate tools = %v, delegation tool must be hidden on a single-agent route", scenarioToolIDs(prepared.candidateToolSet.Tools()))
	}
	if scenarioToolsContain(prepared.candidateToolSet, "search_code") == false {
		t.Fatalf("candidate tools = %v, regular read tools must remain visible", scenarioToolIDs(prepared.candidateToolSet.Tools()))
	}
	if parentDelegationInstruction(prepared) != "" {
		t.Fatal("delegation instruction must be empty on a single-agent route")
	}
}

func TestApplyExecutionRouteFallsBackWhenDelegateToolIsNotVisible(t *testing.T) {
	svc := &Service{delegationEnabled: true}
	prepared := &preparation{
		ctx:              context.Background(),
		request:          Request{RunID: "delegate-tool-missing", Question: "Inspect two independent services."},
		candidateToolSet: compactionToolSet{tools: []tool.Tool{{ID: "search_code"}}},
		planning: evidencePlanningOutput{
			Execution: retrieval.ExecutionSuggestion{
				Strategy: retrieval.ExecutionMultiAgent,
				Tasks:    []retrieval.ExecutionTask{{ID: "services", Objective: "Inspect services.", IndependentlyUseful: true}},
			},
		},
	}

	svc.applyExecutionRoute(prepared)

	if prepared.execution.Path != executionPathSingle ||
		prepared.execution.Strategy != retrieval.ExecutionSingleAgent ||
		prepared.execution.RouteReason != routeReasonDelegationUnavailable {
		t.Fatalf("execution = %+v, want delegation-unavailable normal run", prepared.execution)
	}
	if prepared.execution.DowngradeReason != routeReasonDelegationUnavailable {
		t.Fatalf("execution = %+v, want delegation-unavailable downgrade", prepared.execution)
	}
}
