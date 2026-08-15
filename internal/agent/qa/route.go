package qa

import (
	"context"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
)

// ExecutionPolicy bounds the fixed execution strategies available to QA.
type ExecutionPolicy struct {
	AllowMultiAgent bool
}

// CoordinationEstimate makes the predicted orchestration overhead auditable.
type CoordinationEstimate struct {
	AgentRuns  int
	JoinInputs int
}

// ExecutionAssessment is the server-owned routing decision input.
type ExecutionAssessment struct {
	Strategy              retrieval.ExecutionStrategy
	IndependentTaskCount  int
	RequiredCapabilities  int
	Parallelizable        bool
	SharedContextPressure bool
	EstimatedCoordination CoordinationEstimate
	Reasons               []string
}

type executionRouteInput struct {
	Suggestion        retrieval.ExecutionSuggestion
	Assessment        ExecutionAssessment
	Policy            ExecutionPolicy
	WriteAuthorized   bool
	WriteRequested    bool
	WorkflowAvailable bool
}

type executionRouteDecision struct {
	Strategy        retrieval.ExecutionStrategy
	DowngradeReason string
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
			"proposed_strategy":       input.Suggestion.Strategy,
			"effective_strategy":      output.Strategy,
			"complexity":              input.Suggestion.Complexity,
			"confidence":              input.Suggestion.Confidence,
			"reason_codes":            append([]string(nil), input.Assessment.Reasons...),
			"independent_tasks":       input.Assessment.IndependentTaskCount,
			"required_capabilities":   input.Assessment.RequiredCapabilities,
			"parallelizable":          input.Assessment.Parallelizable,
			"shared_context_pressure": input.Assessment.SharedContextPressure,
			"estimated_agent_runs":    input.Assessment.EstimatedCoordination.AgentRuns,
			"estimated_join_inputs":   input.Assessment.EstimatedCoordination.JoinInputs,
			"downgrade_reason":        output.DowngradeReason,
			"workflow_available":      input.WorkflowAvailable,
			"read_only":               !(input.WriteAuthorized && input.WriteRequested),
			"write_authorized":        input.WriteAuthorized,
			"write_requested":         input.WriteRequested,
			"investigation_tasks":     tasks,
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

	policy := ExecutionPolicy{
		AllowMultiAgent: standardRequest(prepared.request, svc.agentRef),
	}
	workflowAvailable := false
	if policy.AllowMultiAgent && svc.investigation != nil &&
		svc.scenarios != nil && svc.coordinator != nil {
		workflowAvailable = svc.investigation.Available()
	}
	assessment := assessExecution(
		planning.Execution,
		contractFromPreparation(prepared, nil),
	)
	prepared.execution = routeExecution(prepared.ctx, executionRouteInput{
		Suggestion: planning.Execution, Assessment: assessment, Policy: policy,
		WriteAuthorized:   prepared.request.WriteAuthorized,
		WriteRequested:    prepared.request.WriteRequested,
		WorkflowAvailable: workflowAvailable,
	})
	log.InfofCtx(
		prepared.ctx,
		"[qa] execution route proposed=%s effective=%s independent_tasks=%d capabilities=%d parallelizable=%t downgrade=%s",
		planning.Execution.Strategy,
		prepared.execution.Strategy,
		assessment.IndependentTaskCount,
		assessment.RequiredCapabilities,
		assessment.Parallelizable,
		prepared.execution.DowngradeReason,
	)
	if prepared.execution.Strategy == retrieval.ExecutionMultiAgent {
		planningCtx, cancel := context.WithTimeout(
			llm.WithUsagePhase(prepared.ctx, llm.PhaseRoute),
			helperTimeout,
		)
		proposal, err := svc.planTaskGraph(
			planningCtx,
			contractFromPreparation(prepared, nil),
		)
		cancel()
		if err != nil {
			log.WarnfCtx(
				prepared.ctx,
				"[qa] task graph planner degraded; using deterministic goal mapping: %v",
				err,
			)
		} else {
			prepared.taskGraphProposal = &proposal
		}
	}

	svc.emitEvent(EventExecutionRouted, ExecutionEvent{
		RunID: prepared.request.RunID, Strategy: string(prepared.execution.Strategy), Status: "completed",
		Complexity: planning.Execution.Complexity, Confidence: planning.Execution.Confidence,
	})
	degradedReason := prepared.execution.DowngradeReason
	if degradedReason == "" && planning.PlanningError != nil {
		degradedReason = "route_degraded"
	}
	if degradedReason != "" {
		svc.emitEvent(EventExecutionDegraded, ExecutionEvent{
			RunID: prepared.request.RunID, Strategy: string(prepared.execution.Strategy), Status: "degraded",
			Reason: degradedReason, Complexity: planning.Execution.Complexity,
			Confidence: planning.Execution.Confidence,
		})
	}
}

func routeExecution(ctx context.Context, input executionRouteInput) executionRouteDecision {
	decision, _ := runtrace.Invoke(ctx, executionRouteSpec, input, func(_ context.Context, input executionRouteInput) (executionRouteDecision, error) {
		return decideExecutionRoute(input), nil
	})
	return decision
}

func decideExecutionRoute(input executionRouteInput) executionRouteDecision {
	single := func(reason string) executionRouteDecision {
		return executionRouteDecision{Strategy: retrieval.ExecutionSingleAgent, DowngradeReason: reason}
	}
	if input.Suggestion.Strategy != retrieval.ExecutionMultiAgent {
		return single("")
	}
	if !input.Policy.AllowMultiAgent {
		return single("policy_disallows_multi_agent")
	}
	if input.WriteRequested {
		return single("write_requested")
	}
	if !input.WorkflowAvailable {
		return single("workflow_unavailable")
	}
	if input.Assessment.IndependentTaskCount < 2 {
		return single("insufficient_independent_tasks")
	}
	if input.Assessment.RequiredCapabilities < 2 {
		return single("insufficient_investigation_capabilities")
	}
	if !input.Assessment.Parallelizable {
		return single("tasks_not_parallelizable")
	}
	return executionRouteDecision{Strategy: retrieval.ExecutionMultiAgent}
}

func assessExecution(
	suggestion retrieval.ExecutionSuggestion,
	contract TaskContract,
) ExecutionAssessment {
	independentTasks := len(contract.InvestigationGoals)
	capabilities := make(map[string]struct{}, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		sources := goal.Sources
		if len(sources) == 0 {
			sources = []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}
		}
		for _, source := range sources {
			capability := executionCapability(source, goal.Facet)
			if capability != "" {
				capabilities[capability] = struct{}{}
			}
		}
	}
	parallelizable := independentTasks >= 2 &&
		!hasExecutionReason(suggestion.Reasons, "subproblems_are_sequential")
	strategy := retrieval.ExecutionSingleAgent
	if suggestion.Strategy == retrieval.ExecutionMultiAgent && parallelizable {
		strategy = retrieval.ExecutionMultiAgent
	}
	return ExecutionAssessment{
		Strategy:             strategy,
		IndependentTaskCount: independentTasks,
		RequiredCapabilities: len(capabilities),
		Parallelizable:       parallelizable,
		SharedContextPressure: len(contract.EvidenceGoals) >= 3 ||
			len(contract.Entities) >= 2,
		EstimatedCoordination: CoordinationEstimate{
			AgentRuns:  independentTasks + 1,
			JoinInputs: independentTasks,
		},
		Reasons: append([]string(nil), suggestion.Reasons...),
	}
}

func executionCapability(
	source agentapi.EvidenceSource,
	facet string,
) string {
	switch source {
	case agentapi.EvidenceSourceMemory:
		return "knowledge.memory.recall"
	case agentapi.EvidenceSourceWeb:
		return "knowledge.web.research"
	case agentapi.EvidenceSourceRuntime:
		return "knowledge.runtime.observe"
	case agentapi.EvidenceSourceInternal:
		switch facet {
		case "implementation", "entrypoint", "core_flow", "data_and_state":
			return "knowledge.code.inspect"
		case "service.topology", "system_boundary", "external_dependency",
			"runtime_and_operations":
			return "knowledge.service.trace"
		case "documentation", "business_domain":
			return "knowledge.docs.verify"
		default:
			return "knowledge.internal.inspect"
		}
	default:
		return ""
	}
}

func hasExecutionReason(reasons []string, target string) bool {
	for _, reason := range reasons {
		if reason == target {
			return true
		}
	}
	return false
}
