package investigation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// CompileProposal converts a validated planner graph into the same executable
// plan representation used by deterministic planning. The proposal controls
// task identity, goals, edges, and limits; template, executor, schema, and
// permission choices remain server-owned.
func (compiler PlanCompiler) CompileProposal(
	contract InvestigationContract,
	proposal agentapi.TaskGraphProposal,
) (PlanRevision, error) {
	if len(proposal.Tasks) == 0 {
		return PlanRevision{}, fmt.Errorf("%w: proposal has no tasks", ErrPlanInvalid)
	}
	goalIndex, err := indexGoals(contract.Goals)
	if err != nil {
		return PlanRevision{}, err
	}
	maxTasks := compiler.MaxTasks
	if contract.MaxTasks > 0 && (maxTasks == 0 || contract.MaxTasks < maxTasks) {
		maxTasks = contract.MaxTasks
	}
	if proposal.Stop.MaxTasks > 0 && (maxTasks == 0 || proposal.Stop.MaxTasks < maxTasks) {
		maxTasks = proposal.Stop.MaxTasks
	}

	tasks := make(map[string]agentapi.TaskSpec, len(proposal.Tasks))
	for _, task := range proposal.Tasks {
		if strings.TrimSpace(task.ID) == "" || !templateIDPattern.MatchString(task.ID) {
			return PlanRevision{}, fmt.Errorf("%w: proposal task id %q is not canonical", ErrPlanInvalid, task.ID)
		}
		if _, duplicate := tasks[task.ID]; duplicate {
			return PlanRevision{}, fmt.Errorf("%w: proposal task %q is duplicated", ErrPlanInvalid, task.ID)
		}
		if task.Capability == "evidence.synthesize" {
			// Delivery owns composition. A planner may still declare this node to
			// describe its graph, but it must not create a second composer.
			continue
		}
		if strings.TrimSpace(task.Purpose) == "" {
			return PlanRevision{}, fmt.Errorf("%w: proposal task %q purpose is required", ErrPlanInvalid, task.ID)
		}
		if err := validateProposalOutputSchema(task); err != nil {
			return PlanRevision{}, err
		}
		if err := validateProposalGoalSelectors(task, goalIndex, contract.Goals); err != nil {
			return PlanRevision{}, err
		}
		if _, ok := proposalTemplate(task.Capability); !ok {
			return PlanRevision{}, fmt.Errorf("%w: capability %q is not registered for investigation", ErrCapabilityGap, task.Capability)
		}
		tasks[task.ID] = task
	}
	if len(tasks) == 0 {
		return PlanRevision{}, fmt.Errorf("%w: proposal has no executable investigation tasks", ErrPlanInvalid)
	}
	if maxTasks > 0 && len(tasks) > maxTasks {
		return PlanRevision{}, fmt.Errorf("%w: proposal has %d executable tasks, maximum is %d", ErrPlanInvalid, len(tasks), maxTasks)
	}

	dependencies := make(map[string][]string, len(tasks))
	requiredDependency := make(map[string]bool, len(tasks))
	seenEdges := make(map[string]bool, len(proposal.Edges))
	for _, edge := range proposal.Edges {
		if edge.From == "" || edge.To == "" {
			return PlanRevision{}, fmt.Errorf("%w: proposal edge has an empty endpoint", ErrPlanInvalid)
		}
		if edge.From == edge.To {
			return PlanRevision{}, fmt.Errorf("%w: proposal edge %q depends on itself", ErrPlanInvalid, edge.From)
		}
		edgeKey := edge.From + "\x00" + edge.To
		if seenEdges[edgeKey] {
			return PlanRevision{}, fmt.Errorf("%w: proposal edge %q -> %q is duplicated", ErrPlanInvalid, edge.From, edge.To)
		}
		seenEdges[edgeKey] = true
		if _, ok := tasks[edge.From]; !ok {
			return PlanRevision{}, fmt.Errorf("%w: proposal edge references unknown source %q", ErrPlanInvalid, edge.From)
		}
		if edge.To == "synthesize" {
			continue
		}
		if _, ok := tasks[edge.To]; !ok {
			return PlanRevision{}, fmt.Errorf("%w: proposal edge references unknown target %q", ErrPlanInvalid, edge.To)
		}
		dependencies[edge.To] = append(dependencies[edge.To], edge.From)
		if edge.Required {
			requiredDependency[edge.To] = true
		}
	}

	candidates := make([]TaskCandidate, 0, len(tasks))
	ids := make([]string, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		task := tasks[id]
		goalIDs := proposalGoalIDs(task, contract.Goals)
		if len(goalIDs) == 0 {
			return PlanRevision{}, fmt.Errorf("%w: proposal task %q does not bind an evidence goal", ErrPlanInvalid, id)
		}
		goalCopies := make([]EvidenceGoal, 0, len(goalIDs))
		for _, goalID := range goalIDs {
			goalCopies = append(goalCopies, goalIndex[goalID])
		}
		ref, ok := proposalTemplate(task.Capability)
		if !ok {
			return PlanRevision{}, fmt.Errorf("%w: capability %q is not registered", ErrCapabilityGap, task.Capability)
		}
		budget := BudgetVector{}
		if task.Budget.MaxInputTokens > 0 {
			budget.InputTokens = task.Budget.MaxInputTokens
		}
		if task.Budget.MaxOutputTokens > 0 {
			budget.OutputTokens = task.Budget.MaxOutputTokens
		}
		if task.Budget.MaxTotalTokens > 0 {
			budget.TotalTokens = task.Budget.MaxTotalTokens
		}
		if task.Budget.MaxToolCalls > 0 {
			budget.ToolCalls = int(task.Budget.MaxToolCalls)
		}
		if task.Budget.MaxCostMicros > 0 {
			budget.CostMicros = task.Budget.MaxCostMicros
		}
		maxAttempts := proposalMaxAttempts(task.MaxAttempts, proposal.Stop)
		candidate := TaskCandidate{
			ID:            id,
			Template:      ref,
			Objective:     strings.TrimSpace(task.Purpose),
			GoalIDs:       goalIDs,
			Capability:    task.Capability,
			EvidenceGoals: goalCopies,
			Optional:      task.Optional && !requiredDependency[id],
			MaxAttempts:   maxAttempts,
			InputRefs:     convertEvidenceRefs(task.InputRefs),
			Dependencies:  uniqueStrings(dependencies[id]),
			Budget:        budget,
		}
		candidates = append(candidates, candidate)
	}
	// Verification is server-owned and cannot be omitted by the planner. It is
	// appended after the proposal graph and receives all evidence-task outputs.
	verifyGoals := make([]string, 0, len(contract.Goals))
	for _, goal := range contract.Goals {
		verifyGoals = append(verifyGoals, goal.ID)
	}
	candidates = append(candidates, TaskCandidate{
		ID: "evidence.verify", Template: TaskTemplateRef{ID: "evidence.verify", Version: 1},
		Objective: "Verify the evidence produced by the proposed investigation tasks.",
		GoalIDs:   verifyGoals,
	})

	plan, err := compiler.Compile(contract, candidates)
	if err != nil {
		return PlanRevision{}, err
	}
	policy, err := proposalPolicy(proposal.Stop)
	if err != nil {
		return PlanRevision{}, err
	}
	if policy.MaxDepth > 0 {
		if err := validatePlanDepth(plan.Tasks, policy.MaxDepth); err != nil {
			return PlanRevision{}, err
		}
	}
	plan.Policy = policy
	plan.ProposalHash = proposalHash(proposal)
	return plan, nil
}

func validateProposalOutputSchema(task agentapi.TaskSpec) error {
	if task.OutputSchema.ID == "" {
		return nil
	}
	// Investigator implementations are server-owned. Accept the public report
	// schema used by QA and reject any other planner-selected output contract.
	if task.OutputSchema.ID != "investigation.report" || task.OutputSchema.Version != 1 {
		return fmt.Errorf("%w: proposal task %q output schema %q@%d is not the server-owned investigation report schema", ErrPlanInvalid, task.ID, task.OutputSchema.ID, task.OutputSchema.Version)
	}
	return nil
}

func validateProposalGoalSelectors(task agentapi.TaskSpec, goals map[string]EvidenceGoal, allGoals []EvidenceGoal) error {
	for _, goalID := range task.InvestigationGoalIDs {
		goalID = strings.TrimSpace(goalID)
		if goalID == "" {
			return fmt.Errorf("%w: proposal task %q has an empty investigation goal id", ErrPlanInvalid, task.ID)
		}
		if _, ok := goals[goalID]; !ok {
			return fmt.Errorf("%w: proposal task %q references unknown investigation goal %q", ErrPlanInvalid, task.ID, goalID)
		}
	}
	for _, facet := range task.RequiredFacets {
		facet = strings.TrimSpace(facet)
		if facet == "" {
			return fmt.Errorf("%w: proposal task %q has an empty required facet", ErrPlanInvalid, task.ID)
		}
		found := false
		for _, goal := range allGoals {
			if goalMatchesFacets(goal, map[string]struct{}{facet: {}}) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: proposal task %q references unknown facet %q", ErrPlanInvalid, task.ID, facet)
		}
	}
	return nil
}

func proposalTemplate(capability string) (TaskTemplateRef, bool) {
	if !strings.HasPrefix(capability, "knowledge.") {
		return TaskTemplateRef{}, false
	}
	return TaskTemplateRef{ID: "proposal." + strings.TrimPrefix(capability, "knowledge."), Version: 1}, true
}

func proposalGoalIDs(task agentapi.TaskSpec, goals []EvidenceGoal) []string {
	wantedGoals := make(map[string]struct{}, len(task.InvestigationGoalIDs))
	for _, goalID := range task.InvestigationGoalIDs {
		if goalID = strings.TrimSpace(goalID); goalID != "" {
			wantedGoals[goalID] = struct{}{}
		}
	}
	wantedFacets := make(map[string]struct{}, len(task.RequiredFacets))
	for _, facet := range task.RequiredFacets {
		if facet = strings.TrimSpace(facet); facet != "" {
			wantedFacets[facet] = struct{}{}
		}
	}
	out := make([]string, 0, len(goals))
	for _, goal := range goals {
		if len(wantedGoals) > 0 {
			if _, ok := wantedGoals[goal.ID]; !ok {
				continue
			}
		}
		if len(wantedFacets) > 0 && !goalMatchesFacets(goal, wantedFacets) {
			continue
		}
		out = append(out, goal.ID)
	}
	// Older planner proposals did not populate either binding field. In that
	// case preserve compatibility by binding the task to the whole contract;
	// once either field is present it is an explicit allow-list.
	if len(wantedGoals) == 0 && len(wantedFacets) == 0 {
		out = out[:0]
		for _, goal := range goals {
			out = append(out, goal.ID)
		}
	}
	return uniqueStrings(out)
}

func goalMatchesFacets(goal EvidenceGoal, wanted map[string]struct{}) bool {
	if _, ok := wanted[goal.ID]; ok {
		return true
	}
	if _, ok := wanted[goal.Kind]; ok {
		return true
	}
	for _, facet := range goal.Facets {
		if _, ok := wanted[facet]; ok {
			return true
		}
	}
	return false
}

func convertEvidenceRefs(refs []agentapi.EvidenceRef) []EvidenceRef {
	out := make([]EvidenceRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, EvidenceRef{
			SourceKind: ref.SourceKind, Target: ref.Target, Section: ref.Section,
			Version: ref.Version, TimeRange: ref.TimeRange, ContentHash: ref.ContentHash,
		})
	}
	return out
}

func proposalMaxAttempts(taskAttempts int, stop agentapi.StopPolicy) int {
	limit := stop.MaxAttempts
	if stop.MaxRetries > 0 {
		retryLimit := int(stop.MaxRetries) + 1
		if limit == 0 || retryLimit < limit {
			limit = retryLimit
		}
	}
	return boundedAttempts(taskAttempts, limit)
}

func boundedAttempts(value, limit int) int {
	if value <= 0 {
		value = 1
	}
	if limit > 0 && value > limit {
		return limit
	}
	return value
}

func proposalPolicy(stop agentapi.StopPolicy) (PlanExecutionPolicy, error) {
	if stop.MaxTasks < 0 || stop.MaxParallelism < 0 || stop.MaxAttempts < 0 ||
		stop.MaxRounds < 0 || stop.MaxDepth < 0 || stop.MaxInputTokens < 0 ||
		stop.MaxOutputTokens < 0 || stop.MaxTotalTokens < 0 || stop.MaxToolCalls < 0 ||
		stop.MaxCostMicros < 0 || stop.MaxRetries < 0 {
		return PlanExecutionPolicy{}, fmt.Errorf("%w: stop limits cannot be negative", ErrPlanInvalid)
	}
	if stop.MaxDuplicateRatio < 0 || stop.MaxDuplicateRatio > 1 {
		return PlanExecutionPolicy{}, fmt.Errorf("%w: max duplicate ratio must be between 0 and 1", ErrPlanInvalid)
	}
	maxRetries := int(stop.MaxRetries)
	if stop.MaxAttempts > 0 && maxRetries > 0 && stop.MaxAttempts < maxRetries+1 {
		maxRetries = stop.MaxAttempts - 1
	}
	return PlanExecutionPolicy{
		MaxParallelism:    stop.MaxParallelism,
		MaxRounds:         stop.MaxRounds,
		MaxDepth:          stop.MaxDepth,
		MaxDuplicateRatio: stop.MaxDuplicateRatio,
		MaxRetries:        maxRetries,
		Budget: BudgetVector{
			InputTokens: stop.MaxInputTokens, OutputTokens: stop.MaxOutputTokens,
			TotalTokens: stop.MaxTotalTokens, ToolCalls: int(stop.MaxToolCalls),
			CostMicros: stop.MaxCostMicros,
		},
	}, nil
}

func validatePlanDepth(tasks []ExecutableTask, maxDepth int) error {
	if maxDepth <= 0 {
		return nil
	}
	byID := make(map[string]ExecutableTask, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	depth := make(map[string]int, len(tasks))
	var visit func(string, map[string]struct{}) (int, error)
	visit = func(id string, path map[string]struct{}) (int, error) {
		if cached, ok := depth[id]; ok {
			return cached, nil
		}
		if _, ok := path[id]; ok {
			return 0, fmt.Errorf("%w: task dependency graph contains a cycle", ErrPlanInvalid)
		}
		path[id] = struct{}{}
		best := 1
		for _, dep := range byID[id].Dependencies {
			d, err := visit(dep, path)
			if err != nil {
				return 0, err
			}
			if d+1 > best {
				best = d + 1
			}
		}
		delete(path, id)
		depth[id] = best
		if best > maxDepth {
			return 0, fmt.Errorf("%w: plan depth %d exceeds maximum %d", ErrPlanInvalid, best, maxDepth)
		}
		return best, nil
	}
	for _, task := range tasks {
		if _, err := visit(task.ID, make(map[string]struct{})); err != nil {
			return err
		}
	}
	return nil
}

func proposalHash(proposal agentapi.TaskGraphProposal) string {
	data, err := json.Marshal(proposal)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
