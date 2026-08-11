package qa

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
)

const (
	defaultMultiAgentMinComplexity = 0.7
	defaultMultiAgentMinConfidence = 0.8
)

// ExecutionPolicy bounds the fixed execution strategies available to QA.
type ExecutionPolicy struct {
	AllowMultiAgent bool
	MinComplexity   float64
	MinConfidence   float64
}

type executionRouteInput struct {
	Suggestion           retrieval.ExecutionSuggestion
	Policy               ExecutionPolicy
	EvidencePlan         domain.EvidencePlan
	AllowWrite           bool
	WorkflowAvailable    bool
	History              retrieval.HistoryRelation
	TimeResolutionFailed bool
	ToolCandidates       []retrieval.ToolRouteCandidate
	RoutedToolIDs        []string
}

type executionRouteDecision struct {
	Strategy        retrieval.ExecutionStrategy
	DowngradeReason string
}

var executionRouteSpec = executiontrace.Spec[executionRouteInput, executionRouteDecision]{
	Operation: "agent.execution_route",
	Node:      "execution_route",
	Output: func(input executionRouteInput, output executionRouteDecision, _ error) map[string]any {
		return map[string]any{
			"proposed_strategy":      input.Suggestion.Strategy,
			"effective_strategy":     output.Strategy,
			"complexity":             input.Suggestion.Complexity,
			"confidence":             input.Suggestion.Confidence,
			"min_complexity":         input.Policy.MinComplexity,
			"min_confidence":         input.Policy.MinConfidence,
			"reason_codes":           append([]string(nil), input.Suggestion.Reasons...),
			"downgrade_reason":       output.DowngradeReason,
			"workflow_available":     input.WorkflowAvailable,
			"read_only":              !input.AllowWrite,
			"internal_only":          input.EvidencePlan.Sources == domain.Internal,
			"time_resolution_failed": input.TimeResolutionFailed,
		}
	},
}

func (svc *QA) routeQAExecution(prepared *qaPreparation) {
	planning := prepared.planning
	decision, effectiveDecision := planning.Decision, planning.Effective
	if decision.Origin == domain.Model &&
		decision.Plan.Direct() && decision.Confidence < svc.routerConfidence {
		log.WarnfCtx(prepared.ctx, "[qa] evidence planner direct confidence %.2f below %.2f; using internal fallback",
			decision.Confidence, svc.routerConfidence,
		)
	}
	if planning.PlanningError != nil {
		logEvidencePlannerFailure(prepared.ctx, planning.PlanningTime, planning.PlanningError)
		planning.RoutedToolIDs = nil
		prepared.planning.RoutedToolIDs = nil
	}

	policy := ExecutionPolicy{
		AllowMultiAgent: standardQARequest(prepared.request, svc.agentRef),
		MinComplexity:   defaultMultiAgentMinComplexity,
		MinConfidence:   defaultMultiAgentMinConfidence,
	}
	workflowAvailable := false
	if policy.AllowMultiAgent && svc.investigation != nil && svc.scenarios != nil {
		workflowAvailable = svc.investigation.Available()
	}
	prepared.execution = routeExecution(prepared.ctx, executionRouteInput{
		Suggestion: planning.Execution, Policy: policy,
		EvidencePlan: effectiveDecision.Plan, AllowWrite: prepared.request.AllowWrite,
		WorkflowAvailable: workflowAvailable,
		History:           prepared.analysis.History, TimeResolutionFailed: prepared.analysis.TimeError != nil,
		ToolCandidates: prepared.toolCandidates, RoutedToolIDs: planning.RoutedToolIDs,
	})

	svc.emitExecutionEvent(EventExecutionRouted, ExecutionEvent{
		RunID: prepared.request.RunID, Strategy: string(prepared.execution.Strategy), Status: "completed",
		Complexity: planning.Execution.Complexity, Confidence: planning.Execution.Confidence,
	})
	degradedReason := prepared.execution.DowngradeReason
	if degradedReason == "" && planning.PlanningError != nil {
		degradedReason = "route_degraded"
	}
	if degradedReason != "" {
		svc.emitExecutionEvent(EventExecutionDegraded, ExecutionEvent{
			RunID: prepared.request.RunID, Strategy: string(prepared.execution.Strategy), Status: "degraded",
			Reason: degradedReason, Complexity: planning.Execution.Complexity,
			Confidence: planning.Execution.Confidence,
		})
	}
}

func routeExecution(ctx context.Context, input executionRouteInput) executionRouteDecision {
	decision, _ := executiontrace.Invoke(ctx, executionRouteSpec, input, func(_ context.Context, input executionRouteInput) (executionRouteDecision, error) {
		return decideExecutionRoute(input), nil
	})
	return decision
}

func decideExecutionRoute(input executionRouteInput) executionRouteDecision {
	single := func(reason string) executionRouteDecision {
		return executionRouteDecision{Strategy: retrieval.ExecutionSingleAgent, DowngradeReason: reason}
	}
	if input.TimeResolutionFailed {
		return single("time_resolution_failed")
	}
	if input.Suggestion.Strategy != retrieval.ExecutionMultiAgent {
		return single("")
	}
	if !input.Policy.AllowMultiAgent {
		return single("policy_disallows_multi_agent")
	}
	if input.Suggestion.Complexity < input.Policy.MinComplexity {
		return single("complexity_below_threshold")
	}
	if input.Suggestion.Confidence < input.Policy.MinConfidence {
		return single("confidence_below_threshold")
	}
	if input.AllowWrite {
		return single("write_requested")
	}
	if input.EvidencePlan.Sources != domain.Internal {
		return single("evidence_not_internal_only")
	}
	if routedTemporalTool(input.ToolCandidates, input.RoutedToolIDs) {
		return single("runtime_evidence_required")
	}
	if historyNeedsContinuity(input.History) {
		return single("history_dependency_required")
	}
	if !input.WorkflowAvailable {
		return single("workflow_unavailable")
	}
	return executionRouteDecision{Strategy: retrieval.ExecutionMultiAgent}
}

func routedTemporalTool(candidates []retrieval.ToolRouteCandidate, selected []string) bool {
	if len(selected) == 0 {
		return false
	}
	temporal := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.Temporal {
			temporal[candidate.ID] = struct{}{}
		}
	}
	for _, id := range selected {
		if _, ok := temporal[id]; ok {
			return true
		}
	}
	return false
}
