package qa

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

const maxInvestigationTasks = 3

const taskGraphPrompt = `You are a constrained investigation planner.
Return JSON only with this exact shape:
{"tasks":[{"purpose":"specific investigation objective","capability":"allowed.capability","investigation_goal_ids":["allowed_goal_id"],"evidence_goal_ids":["allowed_goal_id"],"depends_on":[]}]}

Rules:
- Cover required evidence goals with the smallest useful set of allowed capabilities.
- When investigation goals are supplied, bind every task to one or more of them and cover every investigation goal.
- Sources listed as required_sources are mandatory for that evidence goal; sources listed only in sources are optional alternatives.
- A capability may cover multiple evidence goals and a user investigation goal may require multiple capabilities.
- Use at most max_investigation_tasks tasks.
- Investigation goal ids must be selected from the supplied investigation_goals.
- Evidence goal ids must be selected from the chosen capability's evidence_goal_ids.
- The request may include evidence from an earlier round and conflicting evidence identities.
- Do not repeat prior evidence unless the purpose explicitly resolves a listed conflict.
- When conflicts are listed, prefer a capability that can independently verify the affected facet or source.
- Purpose must be specific to the supplied objective and no longer than 500 characters.
- This is one parallel investigation round, so depends_on must be empty.
- Do not emit task ids, a synthesizer, tools, providers, schemas, permissions, budgets, retries, stop policies, or unknown fields.`

type taskGraphCapability struct {
	ID              string                               `json:"id"`
	EvidenceGoalIDs []string                             `json:"evidence_goal_ids"`
	RequiredFacets  []string                             `json:"required_facets"`
	EvidenceSources map[string][]agentapi.EvidenceSource `json:"evidence_sources,omitempty"`
}

type taskGraphPlannerRequest struct {
	Objective            string                      `json:"objective"`
	Entities             []EntityRef                 `json:"entities,omitempty"`
	InvestigationGoals   []InvestigationGoal         `json:"investigation_goals"`
	EvidenceGoals        []EvidenceGoal              `json:"evidence_goals"`
	AllowedCapabilities  []taskGraphCapability       `json:"allowed_capabilities"`
	MaxInvestigationTask int                         `json:"max_investigation_tasks"`
	PriorEvidenceCount   int                         `json:"prior_evidence_count,omitempty"`
	PriorConflictCount   int                         `json:"prior_conflict_count,omitempty"`
	ConflictIdentities   []agentapi.EvidenceIdentity `json:"conflict_identities,omitempty"`
}

type taskGraphDraft struct {
	Tasks []taskGraphDraftTask `json:"tasks"`
}

type taskGraphDraftTask struct {
	Purpose              string   `json:"purpose"`
	Capability           string   `json:"capability"`
	InvestigationGoalIDs []string `json:"investigation_goal_ids"`
	EvidenceGoalIDs      []string `json:"evidence_goal_ids"`
	DependsOn            []string `json:"depends_on"`
}

type taskGraphPlanningInput struct {
	Contract     TaskContract
	Capabilities []taskGraphCapability
	Required     []string
	Previous     InvestigationResult
}

type taskGraphPlanningOutput struct {
	Proposal agentapi.TaskGraphProposal
}

var taskGraphPlanningSpec = runtrace.Spec[taskGraphPlanningInput, taskGraphPlanningOutput]{
	Operation: "agent.task_graph_plan",
	Node:      "task_graph_plan",
	Input: func(input taskGraphPlanningInput) map[string]any {
		capabilities := make([]string, 0, len(input.Capabilities))
		for _, capability := range input.Capabilities {
			capabilities = append(capabilities, capability.ID)
		}
		return map[string]any{
			"investigation_goals":     len(input.Contract.InvestigationGoals),
			"required_evidence_goals": append([]string(nil), input.Required...),
			"allowed_capabilities":    capabilities,
			"max_investigation_tasks": maxInvestigationTasks,
			"prior_evidence_count":    len(input.Previous.EvidenceUnits),
			"prior_conflict_count":    len(input.Previous.EvidenceConflicts),
		}
	},
	Output: func(_ taskGraphPlanningInput, output taskGraphPlanningOutput, err error) map[string]any {
		if err != nil {
			return map[string]any{"accepted": false, "fallback": "deterministic_evidence_cover", "error": err.Error()}
		}
		tasks := make([]map[string]any, 0, len(output.Proposal.Tasks))
		for _, task := range output.Proposal.Tasks {
			tasks = append(tasks, map[string]any{
				"id": task.ID, "capability": task.Capability,
				"investigation_goal_ids": append([]string(nil), task.InvestigationGoalIDs...),
				"required_facets":        append([]string(nil), task.RequiredFacets...),
			})
		}
		return map[string]any{
			"accepted": true, "tasks": tasks,
		}
	},
	Status: func(_ taskGraphPlanningOutput, err error) string {
		if err != nil {
			return "degraded"
		}
		return ""
	},
}

func (svc *Service) planTaskGraph(ctx context.Context, contract TaskContract) (agentapi.TaskGraphProposal, error) {
	return svc.planTaskGraphWithResult(ctx, contract, InvestigationResult{})
}

func (svc *Service) planTaskGraphWithResult(
	ctx context.Context,
	contract TaskContract,
	previous InvestigationResult,
) (agentapi.TaskGraphProposal, error) {
	capabilities, capabilityErr := taskGraphCapabilities(contract)
	required := requiredEvidenceGoalIDs(contract)
	output, err := runtrace.Invoke(
		ctx,
		taskGraphPlanningSpec,
		taskGraphPlanningInput{
			Contract: contract, Capabilities: capabilities, Required: required,
			Previous: cloneInvestigationResult(previous),
		},
		func(ctx context.Context, input taskGraphPlanningInput) (taskGraphPlanningOutput, error) {
			if capabilityErr != nil {
				return taskGraphPlanningOutput{}, capabilityErr
			}
			if svc.fastLLM == nil {
				return taskGraphPlanningOutput{}, fmt.Errorf("task graph planner LLM is unavailable")
			}
			request, err := json.Marshal(taskGraphPlannerRequest{
				Objective:            contract.Objective,
				Entities:             append([]EntityRef(nil), contract.Entities...),
				InvestigationGoals:   append([]InvestigationGoal(nil), contract.InvestigationGoals...),
				EvidenceGoals:        append([]EvidenceGoal(nil), contract.EvidenceGoals...),
				AllowedCapabilities:  input.Capabilities,
				MaxInvestigationTask: maxInvestigationTasks,
				PriorEvidenceCount:   len(previous.EvidenceUnits),
				PriorConflictCount:   len(previous.EvidenceConflicts),
				ConflictIdentities:   evidenceConflictIdentities(previous.EvidenceConflicts),
			})
			if err != nil {
				return taskGraphPlanningOutput{}, fmt.Errorf("marshal task graph planner request: %w", err)
			}
			var draft taskGraphDraft
			maxTokens := svc.routerMaxTokens
			if maxTokens <= 0 {
				maxTokens = 512
			}
			if err := svc.fastLLM.ChatJSON(
				ctx,
				taskGraphPrompt,
				string(request),
				&draft,
				llm.CallOptions{
					MaxTokens: maxTokens, MaxAttempts: 1, DisallowUnknownFields: true,
				},
			); err != nil {
				return taskGraphPlanningOutput{}, fmt.Errorf("plan task graph: %w", err)
			}
			proposal, err := bindTaskGraphDraft(draft, input.Capabilities, contract, maxInvestigationTasks)
			if err != nil {
				return taskGraphPlanningOutput{}, err
			}
			return taskGraphPlanningOutput{Proposal: proposal}, nil
		},
	)
	if err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	return output.Proposal, nil
}

func evidenceConflictIdentities(
	conflicts []agentapi.EvidenceConflict,
) []agentapi.EvidenceIdentity {
	identities := make([]agentapi.EvidenceIdentity, 0, len(conflicts))
	seen := make(map[agentapi.EvidenceIdentity]struct{}, len(conflicts))
	for _, conflict := range conflicts {
		identity := conflict.Identity
		if identity.SourceKind == "" && identity.Target == "" && identity.Section == "" &&
			identity.Version == "" && identity.TimeRange == "" {
			continue
		}
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		identities = append(identities, identity)
	}
	return identities
}

func taskGraphCapabilities(contract TaskContract) ([]taskGraphCapability, error) {
	capabilities := make([]taskGraphCapability, 0, len(contract.EvidenceGoals))
	byID := make(map[string]int, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		goalID := evidenceGoalID(goal)
		if goalID == "" {
			return nil, fmt.Errorf("evidence goal for facet %q has no canonical id", goal.Facet)
		}
		sources := evidenceGoalSourceSet(goal)
		covered := false
		for _, source := range sources {
			capabilityID := executionCapability(source, goal.Facet)
			if capabilityID == "" {
				continue // The server keeps unsupported source/facet pairs out of the graph.
			}
			covered = true
			index, exists := byID[capabilityID]
			if !exists {
				index = len(capabilities)
				byID[capabilityID] = index
				capabilities = append(capabilities, taskGraphCapability{
					ID: capabilityID, EvidenceSources: make(map[string][]agentapi.EvidenceSource),
				})
			}
			capability := &capabilities[index]
			if !containsString(capability.EvidenceGoalIDs, goalID) {
				capability.EvidenceGoalIDs = append(capability.EvidenceGoalIDs, goalID)
			}
			if !containsString(capability.RequiredFacets, goal.Facet) {
				capability.RequiredFacets = append(capability.RequiredFacets, goal.Facet)
			}
			capability.EvidenceSources[goalID] = appendUniqueSource(
				capability.EvidenceSources[goalID], source,
			)
		}
		if !covered {
			return nil, fmt.Errorf(
				"no investigation capability covers evidence goal %q facet %q",
				goalID,
				goal.Facet,
			)
		}
	}
	if len(capabilities) == 0 {
		return nil, fmt.Errorf("investigation evidence goals have no capabilities")
	}
	return capabilities, nil
}

func evidenceGoalSourceSet(goal EvidenceGoal) []agentapi.EvidenceSource {
	values := make([]agentapi.EvidenceSource, 0, len(goal.RequiredSources)+len(goal.Sources))
	for _, source := range goal.RequiredSources {
		values = appendUniqueSource(values, source)
	}
	for _, source := range goal.Sources {
		values = appendUniqueSource(values, source)
	}
	if len(values) == 0 {
		return []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}
	}
	return values
}

func appendUniqueSource(values []agentapi.EvidenceSource, source agentapi.EvidenceSource) []agentapi.EvidenceSource {
	for _, existing := range values {
		if existing == source {
			return values
		}
	}
	return append(values, source)
}

func requiredEvidenceGoalIDs(contract TaskContract) []string {
	required := make([]string, 0, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		if goal.Required {
			required = appendUnique(required, evidenceGoalID(goal))
		}
	}
	if len(required) > 0 {
		return required
	}
	for _, goal := range contract.EvidenceGoals {
		required = appendUnique(required, evidenceGoalID(goal))
	}
	return required
}

func evidenceGoalID(goal EvidenceGoal) string {
	if id := strings.TrimSpace(goal.ID); id != "" {
		return id
	}
	return strings.TrimSpace(goal.Facet)
}

func validateTaskGraphDraft(
	draft taskGraphDraft,
	allowed []taskGraphCapability,
	required []string,
	limit int,
) error {
	if len(draft.Tasks) == 0 {
		return fmt.Errorf("task graph requires at least one investigator task")
	}
	if limit <= 0 || len(draft.Tasks) > limit {
		return fmt.Errorf("task graph has %d investigator tasks, limit is %d", len(draft.Tasks), limit)
	}
	allowedByID := make(map[string]taskGraphCapability, len(allowed))
	for _, capability := range allowed {
		allowedByID[capability.ID] = capability
	}
	requiredSet := stringSet(required)
	for index, task := range draft.Tasks {
		purpose := strings.TrimSpace(task.Purpose)
		if purpose == "" || utf8.RuneCountInString(purpose) > 500 {
			return fmt.Errorf("task %d purpose must contain 1 to 500 characters", index+1)
		}
		capability, ok := allowedByID[task.Capability]
		if !ok {
			return fmt.Errorf("task %d capability %q is not allowed", index+1, task.Capability)
		}
		if len(task.EvidenceGoalIDs) == 0 || !subsetStringSet(task.EvidenceGoalIDs, capability.EvidenceGoalIDs) {
			return fmt.Errorf("task %d evidence goals are not allowed for capability %q", index+1, task.Capability)
		}
		for _, goalID := range task.EvidenceGoalIDs {
			if _, ok := requiredSet[goalID]; !ok {
				return fmt.Errorf("task %d evidence goal %q is not required", index+1, goalID)
			}
		}
		if len(task.DependsOn) != 0 {
			return fmt.Errorf("task %d dependencies are not supported in the single-round investigation planner", index+1)
		}
	}
	return nil
}

func validateTaskGraphCoverage(
	draft taskGraphDraft,
	allowed []taskGraphCapability,
	required []string,
	limit int,
) error {
	if err := validateTaskGraphDraft(draft, allowed, required, limit); err != nil {
		return err
	}
	covered := make(map[string]struct{}, len(required))
	for _, task := range draft.Tasks {
		for _, goalID := range task.EvidenceGoalIDs {
			covered[goalID] = struct{}{}
		}
	}
	missing := make([]string, 0, len(required))
	for _, goalID := range required {
		if _, ok := covered[goalID]; !ok {
			missing = append(missing, goalID)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"task graph does not cover required evidence goals: %s",
			strings.Join(missing, ", "),
		)
	}
	return nil
}

func validateTaskGraphContractCoverage(
	draft taskGraphDraft,
	allowed []taskGraphCapability,
	contract TaskContract,
	limit int,
) error {
	required := requiredEvidenceGoalIDs(contract)
	if err := validateTaskGraphCoverage(draft, allowed, required, limit); err != nil {
		return err
	}
	if err := validateInvestigationGoalBindings(draft, contract); err != nil {
		return err
	}
	if err := validateContractEntityCoverage(contract); err != nil {
		return err
	}
	selected := make(map[string]taskGraphCapability, len(draft.Tasks))
	for _, task := range draft.Tasks {
		for _, capability := range allowed {
			if capability.ID == task.Capability {
				selected[task.Capability] = capability
				break
			}
		}
	}
	for _, goal := range contract.EvidenceGoals {
		if !goal.Required || len(goal.RequiredSources) == 0 {
			continue
		}
		goalID := evidenceGoalID(goal)
		for _, source := range goal.RequiredSources {
			covered := false
			for _, task := range draft.Tasks {
				if !containsString(task.EvidenceGoalIDs, goalID) {
					continue
				}
				if containsEvidenceSource(selected[task.Capability].EvidenceSources[goalID], source) {
					covered = true
					break
				}
			}
			if !covered {
				return fmt.Errorf(
					"task graph does not cover required source %q for evidence goal %q",
					source, goalID,
				)
			}
		}
	}
	return nil
}

func validateInvestigationGoalBindings(
	draft taskGraphDraft,
	contract TaskContract,
) error {
	if len(contract.InvestigationGoals) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(contract.InvestigationGoals))
	for _, goal := range contract.InvestigationGoals {
		id := strings.TrimSpace(goal.ID)
		if id == "" {
			return fmt.Errorf("investigation goal has no canonical id")
		}
		allowed[id] = struct{}{}
	}
	covered := make(map[string]struct{}, len(allowed))
	for index, task := range draft.Tasks {
		if len(task.InvestigationGoalIDs) == 0 {
			return fmt.Errorf(
				"task %d must bind at least one investigation goal",
				index+1,
			)
		}
		seen := make(map[string]struct{}, len(task.InvestigationGoalIDs))
		for _, goalID := range task.InvestigationGoalIDs {
			if _, ok := allowed[goalID]; !ok {
				return fmt.Errorf(
					"task %d investigation goal %q is not allowed",
					index+1,
					goalID,
				)
			}
			if _, duplicate := seen[goalID]; duplicate {
				return fmt.Errorf(
					"task %d investigation goal %q is duplicated",
					index+1,
					goalID,
				)
			}
			seen[goalID] = struct{}{}
			covered[goalID] = struct{}{}
		}
	}
	missing := make([]string, 0, len(allowed))
	for _, goal := range contract.InvestigationGoals {
		if _, ok := covered[goal.ID]; !ok {
			missing = append(missing, goal.ID)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"task graph does not cover investigation goals: %s",
			strings.Join(missing, ", "),
		)
	}
	return nil
}

func validateContractEntityCoverage(contract TaskContract) error {
	seen := make(map[string]struct{}, len(contract.Entities))
	for _, entity := range contract.Entities {
		id := strings.TrimSpace(entity.ID)
		if id == "" {
			return fmt.Errorf("task contract contains an entity without an id")
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("task contract contains duplicate entity %q", id)
		}
		seen[id] = struct{}{}
	}
	for _, goal := range contract.EvidenceGoals {
		if !goal.Required || goal.MinimumCoverage <= 1 {
			continue
		}
		if len(contract.Entities) < goal.MinimumCoverage {
			return fmt.Errorf(
				"evidence goal %q requires %d subjects but task contract contains %d entities",
				evidenceGoalID(goal), goal.MinimumCoverage, len(contract.Entities),
			)
		}
	}
	return nil
}

func containsEvidenceSource(values []agentapi.EvidenceSource, target agentapi.EvidenceSource) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func selectCapabilityCover(
	capabilities []taskGraphCapability,
	required []string,
	limit int,
) ([]taskGraphCapability, []string, error) {
	if limit <= 0 {
		return nil, append([]string(nil), required...), fmt.Errorf("max investigation tasks must be positive")
	}
	uncovered := stringSet(required)
	selected := make([]taskGraphCapability, 0, min(limit, len(capabilities)))
	used := make(map[string]struct{}, len(capabilities))
	for len(uncovered) > 0 && len(selected) < limit {
		best := -1
		bestCoverage := 0
		for index, capability := range capabilities {
			if _, ok := used[capability.ID]; ok {
				continue
			}
			coverage := 0
			for _, goalID := range capability.EvidenceGoalIDs {
				if _, ok := uncovered[goalID]; ok {
					coverage++
				}
			}
			if coverage > bestCoverage {
				best, bestCoverage = index, coverage
			}
		}
		if best < 0 {
			break
		}
		capability := capabilities[best]
		selected = append(selected, capability)
		used[capability.ID] = struct{}{}
		for _, goalID := range capability.EvidenceGoalIDs {
			delete(uncovered, goalID)
		}
	}
	if len(selected) == 0 {
		return nil, orderedUncovered(required, uncovered), fmt.Errorf("no investigation capability covers required evidence goals")
	}
	return selected, orderedUncovered(required, uncovered), nil
}

func buildTaskGraphFallback(contract TaskContract) (agentapi.TaskGraphProposal, error) {
	capabilities, err := taskGraphCapabilities(contract)
	if err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	required := requiredEvidenceGoalIDs(contract)
	selected, err := selectContractCapabilityCover(capabilities, contract, maxInvestigationTasks)
	if err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	selected = expandCapabilityCoverForGoals(
		selected,
		len(contract.InvestigationGoals),
		maxInvestigationTasks,
	)
	draft := taskGraphDraft{Tasks: make([]taskGraphDraftTask, 0, len(selected))}
	goalBindings := fallbackInvestigationGoalBindings(contract.InvestigationGoals, len(selected))
	for index, capability := range selected {
		investigationGoalIDs := goalBindings[index]
		draft.Tasks = append(draft.Tasks, taskGraphDraftTask{
			Purpose: defaultTaskPurpose(
				capability.RequiredFacets,
				investigationGoalsByID(contract.InvestigationGoals, investigationGoalIDs),
			),
			Capability:           capability.ID,
			InvestigationGoalIDs: investigationGoalIDs,
			EvidenceGoalIDs:      intersectStrings(capability.EvidenceGoalIDs, required),
		})
	}
	return bindTaskGraphDraft(draft, capabilities, contract, maxInvestigationTasks)
}

func expandCapabilityCoverForGoals(
	selected []taskGraphCapability,
	goalCount int,
	limit int,
) []taskGraphCapability {
	target := min(goalCount, limit)
	if len(selected) == 0 || len(selected) >= target {
		return selected
	}
	base := append([]taskGraphCapability(nil), selected...)
	out := append([]taskGraphCapability(nil), selected...)
	for len(out) < target {
		out = append(out, base[len(out)%len(base)])
	}
	return out
}

func fallbackInvestigationGoalBindings(
	goals []InvestigationGoal,
	taskCount int,
) [][]string {
	if len(goals) == 0 || taskCount == 0 {
		return make([][]string, taskCount)
	}
	bindings := make([][]string, taskCount)
	for index, goal := range goals {
		taskIndex := index % taskCount
		bindings[taskIndex] = append(bindings[taskIndex], goal.ID)
	}
	// Required source coverage can force more tasks than deliverables. Keep
	// every investigator scoped to a deliverable instead of reopening the
	// whole contract for the extra evidence source.
	for index := range bindings {
		if len(bindings[index]) == 0 {
			bindings[index] = append(bindings[index], goals[index%len(goals)].ID)
		}
	}
	return bindings
}

func investigationGoalsByID(
	goals []InvestigationGoal,
	ids []string,
) []InvestigationGoal {
	byID := make(map[string]InvestigationGoal, len(goals))
	for _, goal := range goals {
		byID[goal.ID] = goal
	}
	out := make([]InvestigationGoal, 0, len(ids))
	for _, id := range ids {
		if goal, ok := byID[id]; ok {
			out = append(out, goal)
		}
	}
	return out
}

func selectContractCapabilityCover(
	capabilities []taskGraphCapability,
	contract TaskContract,
	limit int,
) ([]taskGraphCapability, error) {
	keys := requiredContractCoverageKeys(contract)
	if len(keys) == 0 {
		return nil, fmt.Errorf("investigation contract has no required coverage")
	}
	coverage := make([]taskGraphCapability, len(capabilities))
	for index, capability := range capabilities {
		coverage[index] = capability
		coverage[index].EvidenceGoalIDs = capabilityContractCoverageKeys(capability, contract)
	}
	selectedCoverage, uncovered, err := selectCapabilityCover(coverage, keys, limit)
	if err != nil {
		return nil, err
	}
	if len(uncovered) > 0 {
		return nil, fmt.Errorf("investigation task budget leaves required coverage unresolved: %s", strings.Join(uncovered, ", "))
	}
	byID := make(map[string]taskGraphCapability, len(capabilities))
	for _, capability := range capabilities {
		byID[capability.ID] = capability
	}
	selected := make([]taskGraphCapability, 0, len(selectedCoverage))
	for _, capability := range selectedCoverage {
		selected = append(selected, byID[capability.ID])
	}
	return selected, nil
}

func requiredContractCoverageKeys(contract TaskContract) []string {
	keys := make([]string, 0, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		if !goal.Required {
			continue
		}
		goalID := evidenceGoalID(goal)
		if len(goal.RequiredSources) == 0 {
			keys = append(keys, goalID+"\x00*")
			continue
		}
		for _, source := range goal.RequiredSources {
			keys = append(keys, goalID+"\x00"+string(source))
		}
	}
	if len(keys) > 0 {
		return keys
	}
	for _, goal := range contract.EvidenceGoals {
		keys = append(keys, evidenceGoalID(goal)+"\x00*")
	}
	return keys
}

func capabilityContractCoverageKeys(
	capability taskGraphCapability,
	contract TaskContract,
) []string {
	goals := make(map[string]EvidenceGoal, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		goals[evidenceGoalID(goal)] = goal
	}
	keys := make([]string, 0, len(capability.EvidenceGoalIDs))
	for _, goalID := range capability.EvidenceGoalIDs {
		goal := goals[goalID]
		sources := capability.EvidenceSources[goalID]
		if len(goal.RequiredSources) == 0 {
			if len(sources) > 0 {
				keys = append(keys, goalID+"\x00*")
			}
			continue
		}
		for _, source := range goal.RequiredSources {
			if containsEvidenceSource(sources, source) {
				keys = append(keys, goalID+"\x00"+string(source))
			}
		}
	}
	return keys
}

func bindTaskGraphDraft(
	draft taskGraphDraft,
	allowed []taskGraphCapability,
	contract TaskContract,
	limit int,
) (agentapi.TaskGraphProposal, error) {
	if err := validateTaskGraphContractCoverage(draft, allowed, contract, limit); err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	goalFacet := make(map[string]string, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		goalFacet[evidenceGoalID(goal)] = goal.Facet
	}
	proposal := agentapi.TaskGraphProposal{
		Tasks: make([]agentapi.TaskSpec, 0, len(draft.Tasks)+1),
		Edges: make([]agentapi.TaskEdge, 0, len(draft.Tasks)),
		Stop: agentapi.StopPolicy{
			MaxTasks: len(draft.Tasks) + 1, MaxParallelism: len(draft.Tasks), MaxRounds: 1,
		},
	}
	kindCounts := make(map[string]int)
	goalTaskCounts := make(map[string]int)
	for _, task := range draft.Tasks {
		kind := capabilityTaskKind(task.Capability)
		kindCounts[kind]++
		id := capabilityTaskID(
			task.InvestigationGoalIDs,
			kind,
			kindCounts,
			goalTaskCounts,
			len(contract.InvestigationGoals) > 0,
		)
		facets := make([]string, 0, len(task.EvidenceGoalIDs))
		for _, goalID := range task.EvidenceGoalIDs {
			facet := goalFacet[goalID]
			if facet != "" {
				facets = appendUnique(facets, facet)
			}
		}
		proposal.Tasks = append(proposal.Tasks, agentapi.TaskSpec{
			ID: id, Purpose: strings.TrimSpace(task.Purpose),
			InvestigationGoalIDs: append([]string(nil), task.InvestigationGoalIDs...),
			RequiredFacets:       facets, Capability: task.Capability,
			OutputSchema:  agentapi.SchemaRef{ID: "investigation.report", Version: 1},
			ParallelGroup: "investigation", Optional: true, MaxAttempts: 2,
		})
		proposal.Edges = append(proposal.Edges, agentapi.TaskEdge{From: id, To: "synthesize"})
	}
	proposal.Tasks = append(proposal.Tasks, agentapi.TaskSpec{
		ID: "synthesize", Purpose: "Synthesize the available evidence.",
		Capability:   "evidence.synthesize",
		OutputSchema: agentapi.SchemaRef{ID: "investigation.answer", Version: 3},
		MaxAttempts:  2,
	})
	return proposal, nil
}

func capabilityTaskKind(capability string) string {
	switch capability {
	case "knowledge.code.inspect":
		return "code"
	case "knowledge.service.trace":
		return "service"
	case "knowledge.docs.verify":
		return "docs"
	case "knowledge.web.research":
		return "web"
	case "knowledge.memory.recall":
		return "memory"
	case "knowledge.runtime.observe":
		return "runtime"
	default:
		return "source"
	}
}

func defaultTaskPurpose(facets []string, goals []InvestigationGoal) string {
	if len(goals) > 0 {
		objectives := make([]string, 0, len(goals))
		for _, goal := range goals {
			if objective := strings.TrimSpace(goal.Objective); objective != "" {
				objectives = append(objectives, objective)
			}
		}
		if len(objectives) > 0 {
			purpose := fmt.Sprintf(
				"Support investigation deliverable(s): %s",
				strings.Join(objectives, "; "),
			)
			if len(facets) > 0 {
				purpose += fmt.Sprintf(" Focus on evidence facets: %s.", strings.Join(facets, ", "))
			}
			if utf8.RuneCountInString(purpose) > 500 {
				purpose = string([]rune(purpose)[:500])
			}
			return purpose
		}
	}
	if len(facets) == 0 {
		return "Investigate the required evidence."
	}
	return fmt.Sprintf("Investigate required evidence facets: %s.", strings.Join(facets, ", "))
}

func capabilityTaskID(
	investigationGoalIDs []string,
	kind string,
	kindCounts map[string]int,
	goalTaskCounts map[string]int,
	boundToGoals bool,
) string {
	if !boundToGoals || len(investigationGoalIDs) == 0 {
		return fmt.Sprintf("investigate.%s.%d", kind, kindCounts[kind])
	}
	goalID := investigationGoalIDs[0]
	goalTaskCounts[goalID]++
	return fmt.Sprintf(
		"investigate.%s.%s.%d",
		goalID, kind, goalTaskCounts[goalID],
	)
}

func orderedUncovered(required []string, uncovered map[string]struct{}) []string {
	out := make([]string, 0, len(uncovered))
	for _, goalID := range required {
		if _, ok := uncovered[goalID]; ok {
			out = append(out, goalID)
		}
	}
	return out
}

func intersectStrings(values, allowed []string) []string {
	allowedSet := stringSet(allowed)
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := allowedSet[value]; ok {
			out = appendUnique(out, value)
		}
	}
	return out
}

func subsetStringSet(values, allowed []string) bool {
	allowedSet := stringSet(allowed)
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := allowedSet[value]; !ok {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}

func appendUnique(values []string, value string) []string {
	if value == "" || containsString(values, value) {
		return values
	}
	return append(values, value)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
