package qa

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
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
	CleanQuestion       string
	Terms               retrieval.QueryTerms
	QuerySemantics      *domain.QuerySemantics
	QuerySemanticsError error
	Time                retrieval.TimeExpr
	Decision            domain.PlanDecision
	Effective           domain.PlanDecision
	Execution           retrieval.ExecutionSuggestion
	History             retrieval.HistoryRelation
	HistoryValid        bool
	RoutedToolIDs       []string
	PlanningError       error
	PlanningTime        time.Duration
}

var evidencePlanningSpec = runtrace.Spec[evidencePlanningInput, evidencePlanningOutput]{
	Operation: "agent.evidence_plan",
	Node:      "evidence_plan",
	Output: func(input evidencePlanningInput, output evidencePlanningOutput, _ error) map[string]any {
		planningError := ""
		semanticsError := ""
		fallbackError := ""
		if output.QuerySemanticsError != nil {
			semanticsError = output.QuerySemanticsError.Error()
		}
		if output.PlanningError != nil {
			planningError = output.PlanningError.Error()
			if output.Effective.Origin == domain.Fallback {
				fallbackError = planningError
			}
		}
		return map[string]any{
			"proposed_plan": output.Decision.Plan.String(), "proposed_sources": output.Decision.Plan.SourceNames(),
			"proposed_confidence": output.Decision.Confidence, "proposed_origin": output.Decision.Origin,
			"effective_plan": output.Effective.Plan.String(), "effective_sources": output.Effective.Plan.SourceNames(),
			"effective_confidence": output.Effective.Confidence, "effective_origin": output.Effective.Origin,
			"preferred_tool_ids": output.RoutedToolIDs, "available_tool_ids": input.AvailableTools,
			"planning_error": planningError, "query_semantics_error": semanticsError,
			"fallback_error": fallbackError,
		}
	},
	Status: func(output evidencePlanningOutput, _ error) string {
		if output.PlanningError != nil || output.QuerySemanticsError != nil {
			return "degraded"
		}
		return ""
	},
}

func (svc *Service) planEvidence(ctx context.Context, input evidencePlanningInput) (evidencePlanningOutput, error) {
	return runtrace.Invoke(ctx, evidencePlanningSpec, input, func(ctx context.Context, input evidencePlanningInput) (evidencePlanningOutput, error) {
		output := evidencePlanningOutput{
			CleanQuestion: strings.TrimSpace(input.Question),
			Decision:      domain.InternalFallbackDecision(),
			Execution: retrieval.ExecutionSuggestion{
				Strategy: retrieval.ExecutionSingleAgent,
			},
		}
		termsQuestion := strings.TrimSpace(input.Question)
		if input.ExplicitPlan != nil {
			started := time.Now()
			analysis, err := retrieval.AnalyzeForPlan(
				ctx, svc.fastLLM, input.Question, input.RouteContext, termsQuestion,
				input.ToolCandidates, svc.routerMaxTokens, *input.ExplicitPlan,
			)
			output.PlanningTime = time.Since(started)
			if err != nil {
				output.PlanningError = err
				output.Decision = domain.PlanDecision{Plan: *input.ExplicitPlan, Confidence: 1, Origin: domain.Explicit}
				output.Effective = output.Decision
				return output, nil
			}
			output.CleanQuestion, output.Terms, output.Time, output.Decision = analysis.Question, analysis.Terms, analysis.Time, analysis.Decision
			output.QuerySemantics, output.QuerySemanticsError = analysis.QuerySemantics, analysis.QuerySemanticsError
			output.Execution = analysis.Execution
			output.History, output.HistoryValid = analysis.History, input.RouteContext != ""
			output.RoutedToolIDs = analysis.ToolIDs
		} else if shouldShortCircuitMeta(input.Question) {
			output.Decision = domain.PlanDecision{Plan: domain.DirectPlan(), Confidence: 1, Origin: domain.Rule}
		} else {
			started := time.Now()
			analysis, err := retrieval.AnalyzeEvidence(
				ctx, svc.fastLLM, input.Question, input.RouteContext, termsQuestion,
				retrieval.RoutingCapabilities{
					Memory: svc.memory != nil && svc.memory.Enabled() && input.UserID != 0,
					Web:    svc.cfg.WebSearchEnabled,
				},
				input.ToolCandidates,
				svc.routerMaxTokens,
			)
			output.PlanningTime = time.Since(started)
			if err != nil {
				output.PlanningError = err
				output.Decision = domain.InternalFallbackDecision()
				output.Effective = output.Decision
				return output, nil
			}
			output.CleanQuestion, output.Terms, output.Time, output.Decision = analysis.Question, analysis.Terms, analysis.Time, analysis.Decision
			output.QuerySemantics, output.QuerySemanticsError = analysis.QuerySemantics, analysis.QuerySemanticsError
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

func logPlannerFailure(ctx context.Context, duration time.Duration, err error) {
	if errors.Is(err, llm.ErrInvalidJSON) {
		log.WarnfCtx(ctx,
			"[qa] evidence planner failed duration=%s error_kind=invalid_json retry_policy=chat_json error=%v",
			duration, err,
		)
		return
	}

	var callErr *llm.CallError
	if !errors.As(err, &callErr) {
		log.WarnfCtx(ctx,
			"[qa] evidence planner failed duration=%s error_kind=other retry_policy=chat_json error=%v",
			duration, err,
		)
		return
	}

	kind := "unknown"
	switch callErr.Kind {
	case llm.ErrKindNetwork:
		kind = "network"
	case llm.ErrKindStatus:
		kind = "status"
	case llm.ErrKindEmpty:
		kind = "empty"
	case llm.ErrKindEnvelope:
		kind = "envelope"
	}
	log.WarnfCtx(ctx,
		"[qa] evidence planner failed duration=%s error_kind=%s status=%d retryable=%t retry_policy=chat_json error=%v",
		duration, kind, callErr.Status, callErr.Retryable(), err,
	)
}
