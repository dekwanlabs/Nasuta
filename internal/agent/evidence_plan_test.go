package agent

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
)

func TestEvidencePlanningTraceContract(t *testing.T) {
	tests := []struct {
		name              string
		input             evidencePlanningInput
		wantStatus        string
		wantOrigin        domain.DecisionOrigin
		wantSources       []string
		wantConfidence    float64
		wantPlanningError bool
		wantFallbackError bool
	}{
		{
			name: "rule short circuit",
			input: evidencePlanningInput{
				Question: "你能做什么？", AvailableTools: []string{"observe_logs"},
			},
			wantStatus: "completed", wantOrigin: domain.Rule, wantConfidence: 1,
		},
		{
			name:              "planner fallback",
			input:             evidencePlanningInput{Question: "How does checkout work?"},
			wantStatus:        "degraded",
			wantOrigin:        domain.Fallback,
			wantSources:       []string{"internal"},
			wantPlanningError: true,
			wantFallbackError: true,
		},
		{
			name: "explicit plan analysis failure",
			input: evidencePlanningInput{
				Question: "Continue the investigation", RouteContext: "prior turn",
				ExplicitPlan: evidencePlanPointer(domain.EvidencePlan{Sources: domain.Web}),
			},
			wantStatus:        "degraded",
			wantOrigin:        domain.Explicit,
			wantSources:       []string{"web"},
			wantConfidence:    1,
			wantPlanningError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scope := executiontrace.NewScope(executiontrace.Evaluation, nil)
			ctx := executiontrace.WithScope(t.Context(), scope)
			svc := &QA{routerConfidence: 0.9}

			result, err := svc.planEvidence(ctx, test.input)
			if err != nil {
				t.Fatalf("planEvidence: %v", err)
			}
			if (result.PlanningError != nil) != test.wantPlanningError {
				t.Fatalf("planning error = %v", result.PlanningError)
			}

			event := traceEventByNode(t, scope.Snapshot(), "evidence_plan")
			if event.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", event.Status, test.wantStatus)
			}
			assertTraceSources(t, event.Output["proposed_sources"], test.wantSources)
			assertTraceSources(t, event.Output["effective_sources"], test.wantSources)
			if event.Output["proposed_origin"] != test.wantOrigin || event.Output["effective_origin"] != test.wantOrigin {
				t.Fatalf("origins = (%v, %v), want %q", event.Output["proposed_origin"], event.Output["effective_origin"], test.wantOrigin)
			}
			if event.Output["effective_confidence"] != test.wantConfidence {
				t.Fatalf("effective_confidence = %v, want %v", event.Output["effective_confidence"], test.wantConfidence)
			}
			planningError, _ := event.Output["planning_error"].(string)
			fallbackError, _ := event.Output["fallback_error"].(string)
			if (planningError != "") != test.wantPlanningError || (fallbackError != "") != test.wantFallbackError {
				t.Fatalf("planning_error = %q, fallback_error = %q", planningError, fallbackError)
			}
		})
	}
}

func evidencePlanPointer(plan domain.EvidencePlan) *domain.EvidencePlan {
	return &plan
}

func assertTraceSources(t *testing.T, value any, want []string) {
	t.Helper()
	got, ok := value.([]string)
	if !ok || len(got) != len(want) {
		t.Fatalf("sources = %#v, want %#v", value, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("sources = %#v, want %#v", got, want)
		}
	}
}
