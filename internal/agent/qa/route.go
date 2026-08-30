package qa

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
)

// ExecutionPolicy records the request-level conditions that affect whether
// the parent agent may use its delegation tool. QA itself always remains a
// normal agent run; it never creates a durable QA workflow.
type ExecutionPolicy struct {
	AllowMultiAgent bool
}

type executionPath string

const (
	executionPathSingle                executionPath = "single_agent"
	routeReasonParentDynamicDelegation               = "parent_dynamic_delegation"
	routeReasonDelegationUnavailable                 = "delegation_unavailable"
	routeReasonWriteRequested                        = "write_requested"
)

// executionRouteInput is deliberately limited to parent-run advisory data.
// Task-graph planning and workflow promotion no longer belong to QA.
type executionRouteInput struct {
	Suggestion          retrieval.ExecutionSuggestion
	WriteRequested      bool
	DelegationAvailable bool
	DelegationToolReady bool
}

type executionRouteDecision struct {
	Strategy        retrieval.ExecutionStrategy
	Path            executionPath
	HighRisk        bool
	RouteReason     string
	DowngradeReason string
	DecisionOrigin  string
}

var executionRouteSpec = runtrace.Spec[executionRouteInput, executionRouteDecision]{
	Operation: "agent.execution_route",
	Node:      "execution_route",
	Output: func(input executionRouteInput, output executionRouteDecision, _ error) map[string]any {
		tasks := make([]map[string]any, 0, len(input.Suggestion.Tasks))
		for _, task := range input.Suggestion.Tasks {
			tasks = append(tasks, map[string]any{
				"id": task.ID, "objective": task.Objective,
				"independently_useful": task.IndependentlyUseful,
				"depends_on":           append([]string(nil), task.DependsOn...),
			})
		}
		return map[string]any{
			"proposed_strategy":     input.Suggestion.Strategy,
			"effective_strategy":    output.Strategy,
			"effective_path":        output.Path,
			"route_reason":          output.RouteReason,
			"complexity":            input.Suggestion.Complexity,
			"confidence":            input.Suggestion.Confidence,
			"downgrade_reason":      output.DowngradeReason,
			"decision_origin":       output.DecisionOrigin,
			"delegation_available":  input.DelegationAvailable,
			"delegation_tool_ready": input.DelegationToolReady,
			"write_requested":       input.WriteRequested,
			"delegation_tasks":      tasks,
		}
	},
}

func (svc *Service) applyExecutionRoute(prepared *preparation) {
	planning := prepared.planning
	decision := planning.Decision
	if decision.Origin == domain.Model &&
		decision.Plan.Direct() && decision.Confidence < svc.routerConfidence {
		log.WarnfCtx(prepared.ctx, "[qa] evidence planner direct confidence %.2f below %.2f; using internal fallback",
			decision.Confidence, svc.routerConfidence,
		)
	}
	if planning.PlanningError != nil {
		logPlannerFailure(prepared.ctx, planning.PlanningTime, planning.PlanningError)
		planning.RoutedToolIDs = nil
		prepared.planning.RoutedToolIDs = nil
	}

	toolReady := scenarioToolsContain(prepared.candidateToolSet, "delegate_investigation")
	prepared.execution = routeExecution(prepared.ctx, executionRouteInput{
		Suggestion:          planning.Execution,
		WriteRequested:      prepared.request.WriteRequested,
		DelegationAvailable: svc.delegationEnabled,
		DelegationToolReady: toolReady,
	})
	prepared.execution.HighRisk = executionReasonPresent(
		planning.Execution.Reasons, "requires_risk_sensitive_analysis",
	)
	log.InfofCtx(
		prepared.ctx,
		"[qa] execution route proposed=%s effective=%s path=%s delegation_available=%t delegation_tool_ready=%t origin=%s reason=%s downgrade=%s",
		planning.Execution.Strategy,
		prepared.execution.Strategy,
		prepared.execution.Path,
		svc.delegationEnabled,
		toolReady,
		prepared.execution.DecisionOrigin,
		prepared.execution.RouteReason,
		prepared.execution.DowngradeReason,
	)

	// QA routing is intentionally advisory only. The parent always runs the
	// normal agent loop; dynamic delegation, when available, is exposed as a
	// tool to that loop rather than represented as a QA Durable Workflow.
	svc.emitEvent(EventExecutionRouted, ExecutionEvent{
		RunID: prepared.request.RunID, Strategy: svc.executionEventStrategy(prepared.execution),
		Status: "completed", Reason: prepared.execution.RouteReason,
		Complexity: planning.Execution.Complexity, Confidence: planning.Execution.Confidence,
	})
	degradedReason := prepared.execution.DowngradeReason
	if degradedReason == "" && planning.PlanningError != nil {
		degradedReason = "route_degraded"
	}
	if degradedReason != "" {
		svc.emitEvent(EventExecutionDegraded, ExecutionEvent{
			RunID: prepared.request.RunID, Strategy: svc.executionEventStrategy(prepared.execution),
			Status: "degraded", Reason: degradedReason,
			Complexity: planning.Execution.Complexity, Confidence: planning.Execution.Confidence,
		})
	}
}

func executionReasonPresent(reasons []string, wanted string) bool {
	for _, reason := range reasons {
		if reason == wanted {
			return true
		}
	}
	return false
}

func routeExecution(ctx context.Context, input executionRouteInput) executionRouteDecision {
	decision, _ := runtrace.Invoke(ctx, executionRouteSpec, input, func(_ context.Context, input executionRouteInput) (executionRouteDecision, error) {
		return decideExecutionRoute(input), nil
	})
	return decision
}

func decideExecutionRoute(input executionRouteInput) executionRouteDecision {
	decision := executionRouteDecision{
		Strategy:       retrieval.ExecutionSingleAgent,
		Path:           executionPathSingle,
		DecisionOrigin: "server_assessment",
	}
	if input.WriteRequested {
		decision.RouteReason = routeReasonWriteRequested
		decision.DowngradeReason = routeReasonWriteRequested
		return decision
	}
	if input.DelegationAvailable && input.DelegationToolReady {
		decision.RouteReason = routeReasonParentDynamicDelegation
		return decision
	}
	decision.RouteReason = routeReasonDelegationUnavailable
	decision.DowngradeReason = routeReasonDelegationUnavailable
	return decision
}

func (svc *Service) executionEventStrategy(decision executionRouteDecision) string {
	return string(decision.Path)
}
