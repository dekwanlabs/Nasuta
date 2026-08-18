package qa

import (
	"context"
	"fmt"
	"strings"

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
	executionPathSingle     executionPath = "single_agent"
	executionPathDelegation executionPath = "dynamic_delegation"
	executionPathWorkflow   executionPath = "durable_workflow"
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
	EstimatedCoordination  CoordinationEstimate
	Reasons                []string
}

type executionRouteInput struct {
	Suggestion                  retrieval.ExecutionSuggestion
	Assessment                  ExecutionAssessment
	Policy                      ExecutionPolicy
	WriteAuthorized             bool
	WriteRequested              bool
	LegacyWorkflowAvailable     bool
	EscalationWorkflowAvailable bool
	DelegationEnabled           bool
	DelegationShadow            bool
	DelegationAvailable         bool
	DelegationMaxChildren       int
}

type executionRouteDecision struct {
	Strategy                 retrieval.ExecutionStrategy
	Path                     executionPath
	ShadowPath               executionPath
	HighRisk                 bool
	RouteReason              string
	DowngradeReason          string
	DecisionOrigin           string
	PromotionReason          string
	EscalationCapability     agentapi.CapabilityRef
	EscalationCapabilityHash string
	EscalationFocusFacets    []string
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
			"proposed_strategy":             input.Suggestion.Strategy,
			"effective_strategy":            output.Strategy,
			"effective_path":                output.Path,
			"shadow_path":                   output.ShadowPath,
			"route_reason":                  output.RouteReason,
			"complexity":                    input.Suggestion.Complexity,
			"confidence":                    input.Suggestion.Confidence,
			"reason_codes":                  append([]string(nil), input.Assessment.Reasons...),
			"task_count":                    input.Assessment.TaskCount,
			"independent_tasks":             input.Assessment.IndependentTaskCount,
			"required_capabilities":         input.Assessment.RequiredCapabilities,
			"parallelizable":                input.Assessment.Parallelizable,
			"strong_task_dependencies":      input.Assessment.StrongTaskDependencies,
			"high_risk":                     input.Assessment.HighRisk,
			"requires_live_runtime":         input.Assessment.RequiresLiveRuntime,
			"shared_context_pressure":       input.Assessment.SharedContextPressure,
			"estimated_agent_runs":          input.Assessment.EstimatedCoordination.AgentRuns,
			"estimated_join_inputs":         input.Assessment.EstimatedCoordination.JoinInputs,
			"downgrade_reason":              output.DowngradeReason,
			"decision_origin":               output.DecisionOrigin,
			"promotion_reason":              output.PromotionReason,
			"escalation_capability":         output.EscalationCapability.ID,
			"escalation_capability_version": output.EscalationCapability.Version,
			"workflow_available":            effectiveWorkflowAvailability(input),
			"legacy_workflow_available":     input.LegacyWorkflowAvailable,
			"escalation_workflow_available": input.EscalationWorkflowAvailable,
			"delegation_enabled":            input.DelegationEnabled,
			"delegation_shadow":             input.DelegationShadow,
			"delegation_available":          input.DelegationAvailable,
			"delegation_max_children":       input.DelegationMaxChildren,
			"read_only":                     !(input.WriteAuthorized && input.WriteRequested),
			"write_authorized":              input.WriteAuthorized,
			"write_requested":               input.WriteRequested,
			"investigation_tasks":           tasks,
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
	contract := contractFromPreparation(prepared, nil)
	legacyWorkflowAvailable := false
	if policy.AllowMultiAgent &&
		svc.investigation != nil &&
		svc.scenarios != nil &&
		svc.coordinator != nil {
		legacyWorkflowAvailable = svc.investigation.Available()
	}
	escalationWorkflowAvailable := svc.delegationEnabled &&
		svc.workflowEscalation &&
		policy.AllowMultiAgent &&
		svc.workflowEscalator != nil &&
		svc.capabilities != nil &&
		svc.scenarios != nil &&
		svc.coordinator != nil
	delegationAvailable := policy.AllowMultiAgent &&
		scenarioToolsContain(prepared.candidateToolSet, "delegate_investigation")
	assessment := assessExecution(
		planning.Execution,
		contract,
	)
	prepared.execution = routeExecution(prepared.ctx, executionRouteInput{
		Suggestion: planning.Execution, Assessment: assessment, Policy: policy,
		WriteAuthorized:             prepared.request.WriteAuthorized,
		WriteRequested:              prepared.request.WriteRequested,
		LegacyWorkflowAvailable:     legacyWorkflowAvailable,
		EscalationWorkflowAvailable: escalationWorkflowAvailable,
		DelegationEnabled:           svc.delegationEnabled,
		DelegationShadow:            svc.delegationShadow,
		DelegationAvailable:         delegationAvailable,
		DelegationMaxChildren:       svc.delegationChildren,
	})
	prepared.execution.HighRisk = assessment.HighRisk
	if prepared.execution.Path == executionPathWorkflow &&
		svc.usesWorkflowEscalator() {
		var bindingChecker workflowEscalationBindingChecker
		if checker, ok := svc.workflowEscalator.(workflowEscalationBindingChecker); ok {
			bindingChecker = checker
		}
		capability, facets, err := resolveWorkflowEscalationCapability(
			contract,
			svc.capabilities,
			bindingChecker,
		)
		if err != nil {
			log.WarnfCtx(
				prepared.ctx,
				"[qa] workflow escalation capability unavailable: %v",
				err,
			)
			prepared.execution.Strategy = retrieval.ExecutionSingleAgent
			prepared.execution.Path = executionPathSingle
			prepared.execution.DowngradeReason = agentapi.WorkflowUnavailable
			prepared.execution.DecisionOrigin = "server_policy"
		} else {
			prepared.execution.EscalationCapability = agentapi.CapabilityRef{
				ID: capability.ID, Version: capability.Version,
			}
			prepared.execution.EscalationCapabilityHash = capability.ContentHash
			prepared.execution.EscalationFocusFacets = facets
		}
	}
	log.InfofCtx(
		prepared.ctx,
		"[qa] execution route proposed=%s effective=%s path=%s shadow=%s tasks=%d independent_tasks=%d capabilities=%d parallelizable=%t origin=%s reason=%s promotion=%s downgrade=%s",
		planning.Execution.Strategy,
		prepared.execution.Strategy,
		prepared.execution.Path,
		prepared.execution.ShadowPath,
		assessment.TaskCount,
		assessment.IndependentTaskCount,
		assessment.RequiredCapabilities,
		assessment.Parallelizable,
		prepared.execution.DecisionOrigin,
		prepared.execution.RouteReason,
		prepared.execution.PromotionReason,
		prepared.execution.DowngradeReason,
	)
	if prepared.execution.Path == executionPathWorkflow &&
		!svc.usesWorkflowEscalator() {
		planningCtx, cancel := context.WithTimeout(
			llm.WithUsagePhase(prepared.ctx, llm.PhaseRoute),
			helperTimeout,
		)
		proposal, err := svc.planTaskGraph(
			planningCtx,
			contract,
		)
		cancel()
		if err != nil {
			log.WarnfCtx(
				prepared.ctx,
				"[qa] task graph planner degraded; building deterministic goal mapping: %v",
				err,
			)
			proposal, err = buildTaskGraphFallback(contract)
			if err != nil {
				log.WarnfCtx(
					prepared.ctx,
					"[qa] deterministic goal mapping unavailable; workflow will use capability mapping: %v",
					err,
				)
			}
		}
		if err == nil {
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
			Status: "degraded",
			Reason: degradedReason, Complexity: planning.Execution.Complexity,
			Confidence: planning.Execution.Confidence,
		})
	}
}

type workflowEscalationBindingChecker interface {
	SupportsWorkflowEscalation(agentapi.CapabilityRef, string) bool
}

func resolveWorkflowEscalationCapability(
	contract TaskContract,
	resolver WorkflowCapabilityResolver,
	bindingChecker workflowEscalationBindingChecker,
) (agentapi.Capability, []string, error) {
	if resolver == nil {
		return agentapi.Capability{}, nil, fmt.Errorf(
			"workflow capability resolver is unavailable",
		)
	}
	type candidate struct {
		id     string
		facets []string
	}
	candidates := make([]candidate, 0, len(contract.EvidenceGoals))
	indexByID := make(map[string]int)
	for _, goal := range contract.EvidenceGoals {
		for _, source := range goal.Sources {
			id := executionCapability(source, goal.Facet)
			if id == "" {
				continue
			}
			index, ok := indexByID[id]
			if !ok {
				index = len(candidates)
				indexByID[id] = index
				candidates = append(candidates, candidate{id: id})
			}
			if !stringPresent(candidates[index].facets, goal.Facet) {
				candidates[index].facets = append(
					candidates[index].facets,
					goal.Facet,
				)
			}
		}
	}
	var resolutionErrors []string
	for _, candidate := range candidates {
		capability, err := resolver.Resolve(agentapi.CapabilityRef{
			ID: candidate.id,
		})
		if err != nil {
			resolutionErrors = append(
				resolutionErrors,
				fmt.Sprintf("%s: %v", candidate.id, err),
			)
			continue
		}
		if !capability.Enabled ||
			capability.Role != agentapi.RoleInvestigator ||
			capability.SideEffects != agentapi.SideEffectNone ||
			capability.Version <= 0 ||
			capability.ContentHash == "" {
			resolutionErrors = append(
				resolutionErrors,
				fmt.Sprintf("%s: capability is not an enabled read-only investigator", candidate.id),
			)
			continue
		}
		if bindingChecker != nil && !bindingChecker.SupportsWorkflowEscalation(
			agentapi.CapabilityRef{ID: capability.ID, Version: capability.Version},
			capability.ContentHash,
		) {
			resolutionErrors = append(
				resolutionErrors,
				fmt.Sprintf("%s: workflow binding is unavailable", candidate.id),
			)
			continue
		}
		return capability, append([]string(nil), candidate.facets...), nil
	}
	if len(resolutionErrors) == 0 {
		return agentapi.Capability{}, nil, fmt.Errorf(
			"task contract has no workflow escalation capability",
		)
	}
	return agentapi.Capability{}, nil, fmt.Errorf(
		"%s",
		strings.Join(resolutionErrors, "; "),
	)
}

func stringPresent(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (svc *Service) executionEventStrategy(decision executionRouteDecision) string {
	if svc.delegationEnabled {
		return string(decision.Path)
	}
	return string(decision.Strategy)
}

func routeExecution(ctx context.Context, input executionRouteInput) executionRouteDecision {
	decision, _ := runtrace.Invoke(ctx, executionRouteSpec, input, func(_ context.Context, input executionRouteInput) (executionRouteDecision, error) {
		return decideExecutionRoute(input), nil
	})
	return decision
}

func decideExecutionRoute(input executionRouteInput) executionRouteDecision {
	legacy := decideLegacyExecutionRoute(input)
	if !input.DelegationEnabled {
		return legacy
	}

	candidate := decideDelegationRoute(input)
	if input.DelegationShadow {
		if candidate.Path == executionPathDelegation {
			legacy.ShadowPath = executionPathDelegation
			legacy.RouteReason = candidate.RouteReason
		}
		return legacy
	}
	return candidate
}

func decideLegacyExecutionRoute(input executionRouteInput) executionRouteDecision {
	single := func(reason, origin string) executionRouteDecision {
		return executionRouteDecision{
			Strategy: retrieval.ExecutionSingleAgent, Path: executionPathSingle,
			DowngradeReason: reason,
			DecisionOrigin:  origin,
		}
	}
	suggestedMulti := input.Suggestion.Strategy == retrieval.ExecutionMultiAgent
	if !suggestedMulti && (input.Assessment.IndependentTaskCount < 2 ||
		!input.Assessment.Parallelizable) {
		return single("", "server_assessment")
	}
	if !input.Policy.AllowMultiAgent {
		return single("policy_disallows_multi_agent", "server_policy")
	}
	if input.WriteRequested {
		return single("write_requested", "server_policy")
	}
	if !input.LegacyWorkflowAvailable {
		return single("workflow_unavailable", "server_policy")
	}
	if input.Assessment.IndependentTaskCount < 2 {
		return single("insufficient_independent_tasks", "server_assessment")
	}
	if !input.Assessment.Parallelizable {
		return single("tasks_not_parallelizable", "server_assessment")
	}
	decision := executionRouteDecision{
		Strategy: retrieval.ExecutionMultiAgent, Path: executionPathWorkflow,
		DecisionOrigin: "server_assessment",
	}
	if !suggestedMulti {
		decision.PromotionReason = "independent_task_decomposition"
	}
	return decision
}

func decideDelegationRoute(
	input executionRouteInput,
) executionRouteDecision {
	single := func(reason, origin string) executionRouteDecision {
		return executionRouteDecision{
			Strategy: retrieval.ExecutionSingleAgent,
			Path:     executionPathSingle, DowngradeReason: reason,
			DecisionOrigin: origin,
		}
	}
	workflow := func(reason string) executionRouteDecision {
		if !input.EscalationWorkflowAvailable {
			decision := single("workflow_unavailable", "server_policy")
			decision.RouteReason = reason
			return decision
		}
		return executionRouteDecision{
			Strategy: retrieval.ExecutionMultiAgent,
			Path:     executionPathWorkflow, RouteReason: reason,
			DecisionOrigin: "server_policy",
		}
	}

	suggestedMulti := input.Suggestion.Strategy == retrieval.ExecutionMultiAgent
	delegationRequested := suggestedMulti ||
		input.Assessment.IndependentTaskCount >= 2
	if !delegationRequested {
		return single("", "server_assessment")
	}
	if !input.Policy.AllowMultiAgent {
		return single("policy_disallows_multi_agent", "server_policy")
	}
	if input.WriteRequested {
		return single("write_requested", "server_policy")
	}
	if input.Assessment.StrongTaskDependencies {
		return workflow(string(agentapi.EscalationStrongTaskDependencies))
	}
	if input.Assessment.HighRisk || input.Assessment.RequiresLiveRuntime {
		return workflow(string(agentapi.EscalationHighRiskVerificationRequired))
	}
	if input.Assessment.TaskCount > input.DelegationMaxChildren {
		return workflow(string(agentapi.EscalationChildLimitExceeded))
	}
	if input.Assessment.TaskCount == 0 ||
		input.Assessment.IndependentTaskCount != input.Assessment.TaskCount {
		return single("insufficient_independent_tasks", "server_assessment")
	}
	if !input.DelegationAvailable {
		return workflow(string(agentapi.EscalationScenarioRequiresWorkflow))
	}

	decision := executionRouteDecision{
		Strategy:       retrieval.ExecutionSingleAgent,
		Path:           executionPathDelegation,
		RouteReason:    "independent_bounded_investigations",
		DecisionOrigin: "server_assessment",
	}
	if !suggestedMulti {
		decision.PromotionReason = "independent_task_decomposition"
	}
	return decision
}

func effectiveWorkflowAvailability(input executionRouteInput) bool {
	if input.DelegationEnabled && !input.DelegationShadow {
		return input.EscalationWorkflowAvailable
	}
	return input.LegacyWorkflowAvailable
}

func (svc *Service) usesWorkflowEscalator() bool {
	return svc.delegationEnabled &&
		!svc.delegationShadow &&
		svc.workflowEscalation
}

func assessExecution(
	suggestion retrieval.ExecutionSuggestion,
	contract TaskContract,
) ExecutionAssessment {
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
	highRisk := executionReasonPresent(
		suggestion.Reasons,
		"requires_risk_sensitive_analysis",
	)
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
			capability := executionCapability(source, goal.Facet)
			if capability != "" {
				capabilities[capability] = struct{}{}
			}
			if source == agentapi.EvidenceSourceRuntime &&
				goal.Freshness == agentapi.FreshnessBoundedLive {
				requiresLiveRuntime = true
			}
		}
	}
	parallelizable := independentTasks >= 2
	strategy := retrieval.ExecutionSingleAgent
	if parallelizable {
		strategy = retrieval.ExecutionMultiAgent
	}
	return ExecutionAssessment{
		Strategy:               strategy,
		TaskCount:              len(suggestion.Tasks),
		IndependentTaskCount:   independentTasks,
		RequiredCapabilities:   len(capabilities),
		Parallelizable:         parallelizable,
		StrongTaskDependencies: strongDependencies,
		HighRisk:               highRisk,
		RequiresLiveRuntime:    requiresLiveRuntime,
		SharedContextPressure: len(contract.EvidenceGoals) >= 3 ||
			len(contract.Entities) >= 2,
		EstimatedCoordination: CoordinationEstimate{
			AgentRuns:  independentTasks + 1,
			JoinInputs: independentTasks,
		},
		Reasons: append([]string(nil), suggestion.Reasons...),
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

func executionCapability(
	source agentapi.EvidenceSource,
	facet string,
) string {
	selection, ok := investigation.Select(source, facet)
	if ok {
		return selection.CapabilityID
	}
	return ""
}
