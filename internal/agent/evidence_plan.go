package agent

import (
	"context"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

type evidencePlanningInput struct {
	Question       string
	RouteContext   string
	ExplicitPlan   *domain.EvidencePlan
	ToolCandidates []retrieval.ToolRouteCandidate
	AvailableTools []string
	UserID         int64
}

type evidencePlanningOutput struct {
	CleanQuestion string
	Terms         retrieval.QueryTerms
	Time          retrieval.TimeExpr
	Decision      domain.PlanDecision
	Effective     domain.PlanDecision
	Execution     retrieval.ExecutionSuggestion
	History       retrieval.HistoryRelation
	HistoryValid  bool
	RoutedToolIDs []string
	PlanningError error
}

var evidencePlanningSpec = executiontrace.Spec[evidencePlanningInput, evidencePlanningOutput]{
	Operation: "agent.evidence_plan",
	Node:      "evidence_plan",
	Output: func(input evidencePlanningInput, output evidencePlanningOutput, _ error) map[string]any {
		planningError := ""
		fallbackError := ""
		if output.PlanningError != nil {
			planningError = output.PlanningError.Error()
			if output.Effective.Origin == domain.Fallback {
				fallbackError = planningError
			}
		}
		return map[string]any{
			"response_mode": ClassifyResponseMode(input.Question),
			"proposed_plan": output.Decision.Plan.String(), "proposed_sources": output.Decision.Plan.SourceNames(),
			"proposed_confidence": output.Decision.Confidence, "proposed_origin": output.Decision.Origin,
			"effective_plan": output.Effective.Plan.String(), "effective_sources": output.Effective.Plan.SourceNames(),
			"effective_confidence": output.Effective.Confidence, "effective_origin": output.Effective.Origin,
			"preferred_tool_ids": output.RoutedToolIDs, "available_tool_ids": input.AvailableTools,
			"planning_error": planningError, "fallback_error": fallbackError,
		}
	},
	Status: func(output evidencePlanningOutput, _ error) string {
		if output.PlanningError != nil {
			return "degraded"
		}
		return ""
	},
}

func (svc *QA) planEvidence(ctx context.Context, input evidencePlanningInput) (evidencePlanningOutput, error) {
	return executiontrace.Invoke(ctx, evidencePlanningSpec, input, func(ctx context.Context, input evidencePlanningInput) (evidencePlanningOutput, error) {
		output := evidencePlanningOutput{
			CleanQuestion: strings.TrimSpace(input.Question),
			Decision:      domain.InternalFallbackDecision(),
			Execution: retrieval.ExecutionSuggestion{
				Strategy: retrieval.ExecutionSingleAgent,
			},
		}
		termsQuestion := strings.TrimSpace(input.Question)
		if input.ExplicitPlan != nil {
			analysis, err := retrieval.AnalyzeForPlan(
				ctx, svc.fastLLM, input.Question, input.RouteContext, termsQuestion,
				input.ToolCandidates, svc.routerMaxTokens, *input.ExplicitPlan,
			)
			if err != nil {
				output.PlanningError = err
				output.Decision = domain.PlanDecision{Plan: *input.ExplicitPlan, Confidence: 1, Origin: domain.Explicit}
				output.Effective = output.Decision
				return output, nil
			}
			output.CleanQuestion, output.Terms, output.Time, output.Decision = analysis.Question, analysis.Terms, analysis.Time, analysis.Decision
			output.Execution = analysis.Execution
			output.History, output.HistoryValid = analysis.History, input.RouteContext != ""
			output.RoutedToolIDs = analysis.ToolIDs
		} else if shouldShortCircuitMeta(input.Question) {
			output.Decision = domain.PlanDecision{Plan: domain.DirectPlan(), Confidence: 1, Origin: domain.Rule}
		} else {
			analysis, err := retrieval.AnalyzeEvidence(
				ctx, svc.fastLLM, input.Question, input.RouteContext, termsQuestion,
				retrieval.RoutingCapabilities{
					Memory: svc.memory != nil && svc.memory.Enabled() && input.UserID != 0,
					Web:    svc.cfg.WebSearchEnabled,
				},
				input.ToolCandidates,
				svc.routerMaxTokens,
			)
			if err != nil {
				output.PlanningError = err
				output.Decision = domain.InternalFallbackDecision()
				output.Effective = output.Decision
				return output, nil
			}
			output.CleanQuestion, output.Terms, output.Time, output.Decision = analysis.Question, analysis.Terms, analysis.Time, analysis.Decision
			output.Execution = analysis.Execution
			output.History, output.HistoryValid = analysis.History, input.RouteContext != ""
			output.RoutedToolIDs = analysis.ToolIDs
		}

		output.Effective = output.Decision
		if output.Decision.Origin == domain.Model && output.Decision.Plan.Direct() && output.Decision.Confidence < svc.routerConfidence {
			output.Effective = domain.InternalFallbackDecision()
		}
		return output, nil
	})
}
