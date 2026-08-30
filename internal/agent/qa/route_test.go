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

func TestDecideExecutionRouteDoesNotPromoteAnySuggestionToQAWorkflow(t *testing.T) {
	cases := []struct {
		name  string
		input executionRouteInput
	}{
		{
			name: "single agent suggestion",
			input: executionRouteInput{
				Suggestion:          retrieval.ExecutionSuggestion{Strategy: retrieval.ExecutionSingleAgent},
				DelegationAvailable: true,
				DelegationToolReady: true,
			},
		},
		{
			name: "complex multi agent suggestion",
			input: executionRouteInput{
				Suggestion: retrieval.ExecutionSuggestion{
					Strategy: retrieval.ExecutionMultiAgent,
					Tasks: []retrieval.ExecutionTask{
						{ID: "one", Objective: "First independent investigation.", IndependentlyUseful: true},
						{ID: "two", Objective: "Second independent investigation.", IndependentlyUseful: true},
					},
				},
				DelegationAvailable: true,
				DelegationToolReady: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := decideExecutionRoute(tc.input)
			if decision.Path != executionPathSingle || decision.Strategy != retrieval.ExecutionSingleAgent {
				t.Fatalf("decision = %+v, QA must stay on the normal parent run", decision)
			}
			if decision.RouteReason != routeReasonParentDynamicDelegation {
				t.Fatalf("route reason = %q, want %q", decision.RouteReason, routeReasonParentDynamicDelegation)
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
