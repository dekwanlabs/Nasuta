package qa

import (
	"context"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
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

type executionPath string

const (
	executionPathSingle   executionPath = "single_agent"
	executionPathWorkflow executionPath = "durable_workflow"
)

// CoordinationEstimate makes the predicted orchestration overhead auditable.
type CoordinationEstimate struct {
	AgentRuns  int
	JoinInputs int
}

// ExecutionAssessment is the server-owned routing decision input.
type ExecutionAssessment struct {
	Strategy               retrieval.ExecutionStrategy
	TaskCount              int
	IndependentTaskCount   int
	RequiredCapabilities   int
	Parallelizable         bool
	StrongTaskDependencies bool
	HighRisk               bool
	RequiresLiveRuntime    bool
	SharedContextPressure  bool
	EvidenceDecomposable   bool
	EstimatedCoordination  CoordinationEstimate
	Reasons                []string
}

type executionRouteInput struct {
	Suggestion             retrieval.ExecutionSuggestion
	Assessment             ExecutionAssessment
	Policy                 ExecutionPolicy
	WriteAuthorized        bool
	WriteRequested         bool
	InvestigationAvailable bool
}

type executionRouteDecision struct {
	Strategy        retrieval.ExecutionStrategy
	Path            executionPath
	ShadowPath      executionPath
	HighRisk        bool
	RouteReason     string
	DowngradeReason string
	DecisionOrigin  string
	PromotionReason string
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
			"proposed_strategy":        input.Suggestion.Strategy,
			"effective_strategy":       output.Strategy,
			"effective_path":           output.Path,
			"shadow_path":              output.ShadowPath,
			"route_reason":             output.RouteReason,
			"complexity":               input.Suggestion.Complexity,
			"confidence":               input.Suggestion.Confidence,
			"reason_codes":             append([]string(nil), input.Assessment.Reasons...),
			"task_count":               input.Assessment.TaskCount,
			"independent_tasks":        input.Assessment.IndependentTaskCount,
			"required_capabilities":    input.Assessment.RequiredCapabilities,
			"parallelizable":           input.Assessment.Parallelizable,
			"strong_task_dependencies": input.Assessment.StrongTaskDependencies,
			"high_risk":                input.Assessment.HighRisk,
			"requires_live_runtime":    input.Assessment.RequiresLiveRuntime,
			"shared_context_pressure":  input.Assessment.SharedContextPressure,
			"evidence_decomposable":    input.Assessment.EvidenceDecomposable,
			"estimated_agent_runs":     input.Assessment.EstimatedCoordination.AgentRuns,
			"estimated_join_inputs":    input.Assessment.EstimatedCoordination.JoinInputs,
			"downgrade_reason":         output.DowngradeReason,
			"decision_origin":          output.DecisionOrigin,
			"promotion_reason":         output.PromotionReason,
			"workflow_available":       input.InvestigationAvailable,
			"read_only":                !(input.WriteAuthorized && input.WriteRequested),
			"write_authorized":         input.WriteAuthorized,
			"write_requested":          input.WriteRequested,
			"investigation_tasks":      tasks,
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

	policy := ExecutionPolicy{AllowMultiAgent: standardRequest(prepared.request, svc.agentRef)}
	contract := contractFromPreparation(prepared, nil)
	available := policy.AllowMultiAgent && svc.investigation != nil && svc.investigation.Available()
	assessment := assessExecution(planning.Execution, contract)
	prepared.execution = routeExecution(prepared.ctx, executionRouteInput{
		Suggestion: planning.Execution, Assessment: assessment, Policy: policy,
		WriteAuthorized:        prepared.request.WriteAuthorized,
		WriteRequested:         prepared.request.WriteRequested,
		InvestigationAvailable: available,
	})
	prepared.execution.HighRisk = assessment.HighRisk
	log.InfofCtx(
		prepared.ctx,
		"[qa] execution route proposed=%s effective=%s path=%s tasks=%d independent_tasks=%d capabilities=%d parallelizable=%t evidence_decomposable=%t origin=%s reason=%s promotion=%s downgrade=%s",
		planning.Execution.Strategy,
		prepared.execution.Strategy,
		prepared.execution.Path,
		assessment.TaskCount,
		assessment.IndependentTaskCount,
		assessment.RequiredCapabilities,
		assessment.Parallelizable,
		assessment.EvidenceDecomposable,
		prepared.execution.DecisionOrigin,
		prepared.execution.RouteReason,
		prepared.execution.PromotionReason,
		prepared.execution.DowngradeReason,
	)
	if prepared.execution.Path == executionPathWorkflow {
		planningCtx, cancel := context.WithTimeout(
			llm.WithUsagePhase(prepared.ctx, llm.PhaseRoute),
			helperTimeout,
		)
		proposal, err := svc.planTaskGraph(planningCtx, contract)
		cancel()
		if err != nil {
			log.WarnfCtx(prepared.ctx, "[qa] task graph planner degraded; building deterministic goal mapping: %v", err)
			proposal, err = buildTaskGraphFallback(contract)
		}
		if err != nil {
			log.WarnfCtx(prepared.ctx, "[qa] deterministic goal mapping unavailable: %v", err)
		} else {
			prepared.taskGraphProposal = &proposal
		}
	}

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

func assessExecution(suggestion retrieval.ExecutionSuggestion, contract TaskContract) ExecutionAssessment {
	independentTasks := 0
	strongDependencies := false
	for _, task := range suggestion.Tasks {
		if len(task.DependsOn) > 0 {
			strongDependencies = true
		}
		if task.IndependentlyUseful && len(task.DependsOn) == 0 {
			independentTasks++
		}
	}
	capabilities := make(map[string]struct{}, len(contract.EvidenceGoals))
	highRisk := executionReasonPresent(suggestion.Reasons, "requires_risk_sensitive_analysis")
	requiresLiveRuntime := false
	for _, goal := range contract.EvidenceGoals {
		if goal.HighRisk {
			highRisk = true
		}
		sources := goal.Sources
		if len(sources) == 0 {
			sources = []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}
		}
		for _, source := range sources {
			if capability := executionCapability(source, goal.Facet); capability != "" {
				capabilities[capability] = struct{}{}
			}
			if source == agentapi.EvidenceSourceRuntime && goal.Freshness == agentapi.FreshnessBoundedLive {
				requiresLiveRuntime = true
			}
		}
	}
	parallelizable := independentTasks >= 2
	sharedContextPressure := len(contract.EvidenceGoals) >= 3 || len(contract.Entities) >= 2
	// The model's candidate list is advisory. A broad request can still arrive
	// as one umbrella task, while the server already knows that several
	// independent capabilities must cover the evidence contract. Let the
	// workflow planner split that contract instead of silently collapsing the
	// request into single-agent execution.
	// Candidate dependencies describe the model's answer composition, not the
	// evidence collection graph. The server-owned capability map decides whether
	// evidence can be collected in parallel; otherwise one umbrella task can
	// incorrectly block multi-agent routing.
	evidenceDecomposable := sharedContextPressure && len(capabilities) >= 2
	decomposable := parallelizable || evidenceDecomposable
	strategy := retrieval.ExecutionSingleAgent
	if decomposable {
		strategy = retrieval.ExecutionMultiAgent
	}
	estimatedRuns := independentTasks + 1
	estimatedJoins := independentTasks
	if evidenceDecomposable && estimatedRuns < 2 {
		estimatedRuns = len(capabilities) + 1
		estimatedJoins = len(capabilities)
	}
	return ExecutionAssessment{
		Strategy: strategy, TaskCount: len(suggestion.Tasks),
		IndependentTaskCount: independentTasks, RequiredCapabilities: len(capabilities),
		Parallelizable: parallelizable, StrongTaskDependencies: strongDependencies,
		HighRisk: highRisk, RequiresLiveRuntime: requiresLiveRuntime,
		SharedContextPressure: sharedContextPressure, EvidenceDecomposable: evidenceDecomposable,
		EstimatedCoordination: CoordinationEstimate{AgentRuns: estimatedRuns, JoinInputs: estimatedJoins},
		Reasons:               append([]string(nil), suggestion.Reasons...),
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

func executionCapability(source agentapi.EvidenceSource, facet string) string {
	selection, ok := investigation.Select(source, facet)
	if ok {
		return selection.CapabilityID
	}
	return ""
}

func routeExecution(ctx context.Context, input executionRouteInput) executionRouteDecision {
	decision, _ := runtrace.Invoke(ctx, executionRouteSpec, input, func(_ context.Context, input executionRouteInput) (executionRouteDecision, error) {
		return decideExecutionRoute(input), nil
	})
	return decision
}

func decideExecutionRoute(input executionRouteInput) executionRouteDecision {
	single := func(reason, origin string) executionRouteDecision {
		return executionRouteDecision{Strategy: retrieval.ExecutionSingleAgent, Path: executionPathSingle, DowngradeReason: reason, DecisionOrigin: origin}
	}
	assessedSingle := func(reason string) executionRouteDecision {
		return executionRouteDecision{
			Strategy: retrieval.ExecutionSingleAgent, Path: executionPathSingle,
			RouteReason: reason, DecisionOrigin: "server_assessment",
		}
	}
	suggestedMulti := input.Suggestion.Strategy == retrieval.ExecutionMultiAgent
	canDecompose := input.Assessment.IndependentTaskCount >= 2 || input.Assessment.EvidenceDecomposable
	if !suggestedMulti && !canDecompose {
		return assessedSingle("insufficient_independent_tasks")
	}
	if !suggestedMulti && !input.Assessment.Parallelizable && !input.Assessment.EvidenceDecomposable {
		return assessedSingle("tasks_not_parallelizable")
	}
	if !input.Policy.AllowMultiAgent {
		return single("policy_disallows_multi_agent", "server_policy")
	}
	if input.WriteRequested {
		return single("write_requested", "server_policy")
	}
	if !input.InvestigationAvailable {
		return single("investigation_unavailable", "server_policy")
	}
	if !canDecompose {
		return single("insufficient_independent_tasks", "server_assessment")
	}
	if !input.Assessment.Parallelizable && !input.Assessment.EvidenceDecomposable {
		return single("tasks_not_parallelizable", "server_assessment")
	}
	decision := executionRouteDecision{Strategy: retrieval.ExecutionMultiAgent, Path: executionPathWorkflow, DecisionOrigin: "server_assessment"}
	if input.Assessment.EvidenceDecomposable && input.Assessment.IndependentTaskCount < 2 {
		decision.RouteReason = "evidence_goal_decomposition"
	}
	if !suggestedMulti {
		decision.PromotionReason = "independent_task_decomposition"
		if input.Assessment.IndependentTaskCount < 2 {
			decision.PromotionReason = "evidence_goal_decomposition"
		}
	}
	return decision
}

func (svc *Service) executionEventStrategy(decision executionRouteDecision) string {
	return string(decision.Path)
}
