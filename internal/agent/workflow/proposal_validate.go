package workflow

import (
	"fmt"
	"sort"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/scope"
)

func (compiler *ProposalCompiler) validate(
	proposal agentapi.TaskGraphProposal,
	policy CompilationPolicy,
) (validatedProposal, error) {
	if err := compiler.validatePolicy(policy); err != nil {
		return validatedProposal{}, err
	}
	limits, err := validateStopPolicy(proposal.Stop, policy)
	if err != nil {
		return validatedProposal{}, err
	}
	if len(proposal.Tasks) == 0 {
		return validatedProposal{}, fmt.Errorf("task graph proposal requires tasks")
	}
	if len(proposal.Tasks) > limits.maxTasks {
		return validatedProposal{}, fmt.Errorf(
			"task graph proposal has %d tasks, limit is %d",
			len(proposal.Tasks),
			limits.maxTasks,
		)
	}

	tasks := make([]validatedTask, 0, len(proposal.Tasks))
	byID := make(map[string]validatedTask, len(proposal.Tasks))
	for _, task := range proposal.Tasks {
		validated, err := compiler.validateTask(task, policy, limits.maxAttempts)
		if err != nil {
			return validatedProposal{}, err
		}
		if _, duplicate := byID[task.ID]; duplicate {
			return validatedProposal{}, fmt.Errorf("task %q is duplicated", task.ID)
		}
		tasks = append(tasks, validated)
		byID[task.ID] = validated
	}
	edges, successors, predecessors, err := validateProposalEdges(proposal.Edges, byID)
	if err != nil {
		return validatedProposal{}, err
	}
	order, _, err := proposalOrder(byID, successors, predecessors)
	if err != nil {
		return validatedProposal{}, err
	}
	terminalID, err := uniqueTerminal(order, successors)
	if err != nil {
		return validatedProposal{}, err
	}
	if err := validateRequiredGoals(policy.RequiredGoals, tasks); err != nil {
		return validatedProposal{}, err
	}
	if err := validateGroups(tasks, successors); err != nil {
		return validatedProposal{}, err
	}
	joinTargets := compiler.compiledJoinTargets(byID, predecessors)
	joinCount := 0
	for _, required := range joinTargets {
		if required {
			joinCount++
		}
	}
	verifierCount := 0
	verifierTarget := ""
	if policy.VerifierID != "" {
		if !joinTargets[terminalID] {
			return validatedProposal{}, fmt.Errorf(
				"compiled verifier %q requires a join before terminal %q",
				policy.VerifierID,
				terminalID,
			)
		}
		verifierCount = 1
		verifierTarget = terminalID
	}
	riskGateCount := 0
	if policy.RiskGateID != "" {
		if policy.VerifierID == "" {
			return validatedProposal{}, fmt.Errorf(
				"compiled evidence risk gate %q requires a verifier",
				policy.RiskGateID,
			)
		}
		riskGateCount = 1
	}
	compiledDepth := compiledProposalDepth(
		order,
		predecessors,
		joinTargets,
		verifierTarget,
		riskGateCount,
	)
	if compiledDepth > limits.maxDepth {
		return validatedProposal{}, fmt.Errorf(
			"compiled task graph depth %d exceeds limit %d",
			compiledDepth,
			limits.maxDepth,
		)
	}
	compiledNodeCount := len(tasks) + joinCount + verifierCount + riskGateCount
	if compiledNodeCount > policy.Budget.MaxNodes {
		return validatedProposal{}, fmt.Errorf(
			"compiled task graph has %d nodes, workflow limit is %d",
			compiledNodeCount,
			policy.Budget.MaxNodes,
		)
	}
	budget := limits.workflowBudget
	budget.MaxNodes = compiledNodeCount
	budget.MaxParallelism = min(limits.maxParallelism, budget.MaxNodes)
	budget.MaxRounds = limits.maxRounds
	budget.MaxDepth = limits.maxDepth
	return validatedProposal{
		tasks: tasks, edges: edges,
		maxParallelism: limits.maxParallelism,
		maxDepth:       limits.maxDepth,
		workflowBudget: budget,
		joinTargets:    joinTargets,
		terminalID:     terminalID,
	}, nil
}

func (compiler *ProposalCompiler) validatePolicy(policy CompilationPolicy) error {
	if !canonicalID.MatchString(policy.WorkflowID) || policy.WorkflowVersion <= 0 {
		return fmt.Errorf("proposal compilation requires a canonical versioned workflow")
	}
	if strings.TrimSpace(policy.Purpose) == "" {
		return fmt.Errorf("proposal compilation workflow purpose is required")
	}
	if policy.NodeTimeout <= 0 {
		return fmt.Errorf("proposal compilation node timeout must be positive")
	}
	if policy.MaxTasks <= 0 || policy.MaxParallelism <= 0 ||
		policy.MaxAttempts <= 0 || policy.MaxRounds <= 0 || policy.MaxDepth <= 0 {
		return fmt.Errorf("proposal compilation limits must be positive")
	}
	if policy.MaxTasks > policy.Budget.MaxNodes {
		return fmt.Errorf("proposal task limit exceeds the workflow node budget")
	}
	if policy.MaxParallelism > policy.Budget.MaxParallelism {
		return fmt.Errorf("proposal parallelism limit exceeds the workflow budget")
	}
	if policy.MaxRounds > policy.Budget.MaxRounds {
		return fmt.Errorf("proposal round limit exceeds the workflow budget")
	}
	if policy.MaxDepth > policy.Budget.MaxDepth {
		return fmt.Errorf("proposal depth limit exceeds the workflow budget")
	}
	if err := validateSchema("proposal workflow input", policy.InputSchema, compiler.schemas); err != nil {
		return err
	}
	if err := validateSchema("proposal workflow output", policy.OutputSchema, compiler.schemas); err != nil {
		return err
	}
	if err := validateSchema("proposal join input", policy.JoinInputSchema, compiler.schemas); err != nil {
		return err
	}
	if err := validateSchema("proposal join output", policy.JoinOutputSchema, compiler.schemas); err != nil {
		return err
	}
	if !canonicalID.MatchString(policy.JoinID) {
		return fmt.Errorf("proposal join id %q is not canonical", policy.JoinID)
	}
	if policy.JoinMode != JoinPayloadList && policy.JoinMode != JoinEvidenceView {
		return fmt.Errorf("proposal join mode %q is invalid", policy.JoinMode)
	}
	verifierConfigured := policy.VerifierID != "" ||
		policy.VerifierInputSchema != (agentapi.SchemaRef{}) ||
		policy.VerifierOutputSchema != (agentapi.SchemaRef{}) ||
		len(policy.HighRiskGoals) > 0 ||
		policy.HighRiskMinimumTrustTier != 0 ||
		policy.RiskGateID != ""
	if verifierConfigured {
		if !canonicalID.MatchString(policy.VerifierID) {
			return fmt.Errorf(
				"proposal verifier id %q is not canonical",
				policy.VerifierID,
			)
		}
		if err := validateSchema(
			"proposal verifier input",
			policy.VerifierInputSchema,
			compiler.schemas,
		); err != nil {
			return err
		}
		if err := validateSchema(
			"proposal verifier output",
			policy.VerifierOutputSchema,
			compiler.schemas,
		); err != nil {
			return err
		}
		if err := compiler.schemas.ValidateCompatibility(
			policy.JoinOutputSchema,
			policy.VerifierInputSchema,
		); err != nil {
			return fmt.Errorf("proposal join to verifier schema: %w", err)
		}
	}
	if policy.RiskGateID != "" && !canonicalID.MatchString(policy.RiskGateID) {
		return fmt.Errorf(
			"proposal evidence risk gate id %q is not canonical",
			policy.RiskGateID,
		)
	}
	if policy.HighRiskMinimumTrustTier < 0 ||
		policy.HighRiskMinimumTrustTier > 100 {
		return fmt.Errorf(
			"proposal high-risk minimum trust tier must be between 0 and 100",
		)
	}
	if err := validatePermissions("proposal workflow", policy.Permissions); err != nil {
		return err
	}
	if err := validatePermissions("proposal caller", policy.CallerPermissions); err != nil {
		return err
	}
	if err := scope.EnsureSubset(
		policy.Permissions.Scopes,
		policy.CallerPermissions.Scopes,
	); err != nil {
		return fmt.Errorf("proposal workflow permissions: %w", err)
	}
	if policy.FailureMode != FailFast && policy.FailureMode != CollectAvailable {
		return fmt.Errorf("proposal failure mode %q is invalid", policy.FailureMode)
	}
	if err := validateBudget(policy.WorkflowID, policy.Budget); err != nil {
		return err
	}
	if policy.Budget.MaxNodes <= 0 || policy.Budget.MaxParallelism <= 0 ||
		policy.Budget.Timeout <= 0 || policy.Budget.MaxHandoffBytes <= 0 {
		return fmt.Errorf("proposal workflow budgets must be positive")
	}
	if err := validateCanonical("proposal required goal", policy.RequiredGoals); err != nil {
		return err
	}
	if err := validateCanonical("proposal high-risk goal", policy.HighRiskGoals); err != nil {
		return err
	}
	if err := ensureValuesSubset(policy.HighRiskGoals, policy.RequiredGoals); err != nil {
		return fmt.Errorf("proposal high-risk goals: %w", err)
	}
	for capabilityID, budget := range policy.CapabilityBudgets {
		if !canonicalID.MatchString(capabilityID) {
			return fmt.Errorf("proposal capability budget id %q is not canonical", capabilityID)
		}
		if err := validateTaskBudgets(capabilityID, budget); err != nil {
			return err
		}
	}
	for capabilityID, version := range policy.CapabilityVersions {
		if !canonicalID.MatchString(capabilityID) {
			return fmt.Errorf("proposal capability version id %q is not canonical", capabilityID)
		}
		if version <= 0 {
			return fmt.Errorf(
				"proposal capability %q version must be positive",
				capabilityID,
			)
		}
	}
	return nil
}

func (compiler *ProposalCompiler) validateTask(
	task agentapi.TaskSpec,
	policy CompilationPolicy,
	maxAttempts int,
) (validatedTask, error) {
	if !canonicalID.MatchString(task.ID) {
		return validatedTask{}, fmt.Errorf("task id %q is not canonical", task.ID)
	}
	if strings.TrimSpace(task.Purpose) == "" {
		return validatedTask{}, fmt.Errorf("task %q purpose is required", task.ID)
	}
	if !canonicalID.MatchString(task.Capability) {
		return validatedTask{}, fmt.Errorf(
			"task %q capability id %q is not canonical",
			task.ID,
			task.Capability,
		)
	}
	if task.ParallelGroup != "" && !canonicalID.MatchString(task.ParallelGroup) {
		return validatedTask{}, fmt.Errorf(
			"task %q parallel group %q is not canonical",
			task.ID,
			task.ParallelGroup,
		)
	}
	if err := validateCanonical(
		"task "+task.ID+" investigation goal",
		task.InvestigationGoalIDs,
	); err != nil {
		return validatedTask{}, err
	}
	if err := validateCanonical(
		"task "+task.ID+" evidence goal",
		task.EvidenceGoalIDs,
	); err != nil {
		return validatedTask{}, err
	}
	if err := validateCanonical(
		"task "+task.ID+" required facet",
		task.RequiredFacets,
	); err != nil {
		return validatedTask{}, err
	}
	if err := validateEvidenceRefs(task.ID, task.InputRefs); err != nil {
		return validatedTask{}, err
	}
	capability, err := compiler.capabilities.Resolve(agentapi.CapabilityRef{
		ID:      task.Capability,
		Version: policy.CapabilityVersions[task.Capability],
	})
	if err != nil {
		return validatedTask{}, fmt.Errorf("task %q capability: %w", task.ID, err)
	}
	if !capability.Enabled {
		return validatedTask{}, fmt.Errorf(
			"task %q capability %q version %d is disabled",
			task.ID,
			capability.ID,
			capability.Version,
		)
	}
	if task.OutputSchema != capability.OutputSchema {
		return validatedTask{}, fmt.Errorf(
			"task %q output schema must match capability %q version %d",
			task.ID,
			capability.ID,
			capability.Version,
		)
	}
	if err := ensureValuesSubset(task.RequiredFacets, capability.InputFacets); err != nil {
		return validatedTask{}, fmt.Errorf("task %q required facets: %w", task.ID, err)
	}
	if err := scope.EnsureSubset(
		capability.PermissionScope,
		policy.Permissions.Scopes,
	); err != nil {
		return validatedTask{}, fmt.Errorf("task %q workflow permissions: %w", task.ID, err)
	}
	if err := scope.EnsureSubset(
		capability.PermissionScope,
		policy.CallerPermissions.Scopes,
	); err != nil {
		return validatedTask{}, fmt.Errorf("task %q caller permissions: %w", task.ID, err)
	}
	defaultBudget, ok := policy.CapabilityBudgets[capability.ID]
	if !ok {
		return validatedTask{}, fmt.Errorf(
			"task %q capability %q has no server budget",
			task.ID,
			capability.ID,
		)
	}
	budget, err := tightenNodeBudget(task.ID, task.Budget, defaultBudget)
	if err != nil {
		return validatedTask{}, err
	}
	attempts := task.MaxAttempts
	if attempts == 0 {
		attempts = maxAttempts
	}
	if attempts < 1 || attempts > maxAttempts {
		return validatedTask{}, fmt.Errorf(
			"task %q max attempts %d exceeds limit %d",
			task.ID,
			task.MaxAttempts,
			maxAttempts,
		)
	}
	if capability.SideEffects == agentapi.SideEffectWrite &&
		attempts > 1 && !capability.RetrySafe {
		return validatedTask{}, fmt.Errorf(
			"task %q write capability %q is not retry-safe",
			task.ID,
			capability.ID,
		)
	}
	return validatedTask{
		spec: task, capability: capability, budget: budget, attempts: attempts,
	}, nil
}

func validateProposalEdges(
	edges []agentapi.TaskEdge,
	tasks map[string]validatedTask,
) (
	[]agentapi.TaskEdge,
	map[string][]string,
	map[string][]string,
	error,
) {
	prepared := make([]agentapi.TaskEdge, 0, len(edges))
	successors := make(map[string][]string, len(tasks))
	predecessors := make(map[string][]string, len(tasks))
	seen := make(map[string]struct{}, len(edges))
	for _, edge := range edges {
		if !canonicalID.MatchString(edge.From) || !canonicalID.MatchString(edge.To) {
			return nil, nil, nil, fmt.Errorf(
				"task edge %q -> %q is not canonical",
				edge.From,
				edge.To,
			)
		}
		source, sourceOK := tasks[edge.From]
		if !sourceOK {
			return nil, nil, nil, fmt.Errorf(
				"task edge %q -> %q references an unknown source",
				edge.From,
				edge.To,
			)
		}
		if _, targetOK := tasks[edge.To]; !targetOK {
			return nil, nil, nil, fmt.Errorf(
				"task edge %q -> %q references an unknown target",
				edge.From,
				edge.To,
			)
		}
		key := edge.From + "\x00" + edge.To
		if _, duplicate := seen[key]; duplicate {
			return nil, nil, nil, fmt.Errorf(
				"task edge %q -> %q is duplicated",
				edge.From,
				edge.To,
			)
		}
		seen[key] = struct{}{}
		edge.Required = edge.Required || !source.spec.Optional
		prepared = append(prepared, edge)
		successors[edge.From] = append(successors[edge.From], edge.To)
		predecessors[edge.To] = append(predecessors[edge.To], edge.From)
	}
	return prepared, successors, predecessors, nil
}

func proposalOrder(
	tasks map[string]validatedTask,
	successors map[string][]string,
	predecessors map[string][]string,
) ([]string, int, error) {
	indegree := make(map[string]int, len(tasks))
	depths := make(map[string]int, len(tasks))
	ready := make([]string, 0, len(tasks))
	for id := range tasks {
		indegree[id] = len(predecessors[id])
		if indegree[id] == 0 {
			ready = append(ready, id)
			depths[id] = 1
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(tasks))
	maxDepth := 0
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		if depths[id] > maxDepth {
			maxDepth = depths[id]
		}
		for _, next := range successors[id] {
			if depths[next] < depths[id]+1 {
				depths[next] = depths[id] + 1
			}
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(tasks) {
		return nil, 0, fmt.Errorf("task graph proposal contains a cycle")
	}
	return order, maxDepth, nil
}

func uniqueTerminal(
	order []string,
	successors map[string][]string,
) (string, error) {
	terminal := ""
	for _, id := range order {
		if len(successors[id]) != 0 {
			continue
		}
		if terminal != "" {
			return "", fmt.Errorf(
				"task graph proposal has multiple terminals %q and %q",
				terminal,
				id,
			)
		}
		terminal = id
	}
	if terminal == "" {
		return "", fmt.Errorf("task graph proposal has no terminal")
	}
	return terminal, nil
}

func validateRequiredGoals(goals []string, tasks []validatedTask) error {
	required := make(map[string]bool, len(goals))
	for _, goal := range goals {
		required[goal] = false
	}
	for _, task := range tasks {
		for _, facet := range task.spec.RequiredFacets {
			if _, tracked := required[facet]; tracked {
				required[facet] = true
			}
		}
	}
	for _, goal := range goals {
		if !required[goal] {
			return fmt.Errorf(
				"required goal %q has no assigned task",
				goal,
			)
		}
	}
	return nil
}

func validateGroups(
	tasks []validatedTask,
	successors map[string][]string,
) error {
	groups := make(map[string][]validatedTask)
	for _, task := range tasks {
		if task.spec.ParallelGroup != "" {
			groups[task.spec.ParallelGroup] = append(
				groups[task.spec.ParallelGroup],
				task,
			)
		}
	}
	for groupID, group := range groups {
		capabilityCounts := make(map[string]int, len(group))
		writes := make(map[string]string)
		for index, task := range group {
			capabilityCounts[task.capability.ID]++
			if capabilityCounts[task.capability.ID] > task.capability.MaxConcurrency {
				return fmt.Errorf(
					"parallel group %q exceeds capability %q concurrency %d",
					groupID,
					task.capability.ID,
					task.capability.MaxConcurrency,
				)
			}
			for _, target := range task.capability.WriteSet {
				if owner, conflict := writes[target]; conflict {
					return fmt.Errorf(
						"parallel group %q tasks %q and %q both write %q",
						groupID,
						owner,
						task.spec.ID,
						target,
					)
				}
				writes[target] = task.spec.ID
			}
			for previous := 0; previous < index; previous++ {
				other := group[previous]
				if pathExists(task.spec.ID, other.spec.ID, successors) ||
					pathExists(other.spec.ID, task.spec.ID, successors) {
					return fmt.Errorf(
						"parallel group %q contains dependent tasks %q and %q",
						groupID,
						other.spec.ID,
						task.spec.ID,
					)
				}
			}
		}
	}
	return nil
}

func pathExists(from, to string, successors map[string][]string) bool {
	seen := make(map[string]struct{}, len(successors))
	queue := append([]string(nil), successors[from]...)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == to {
			return true
		}
		if _, visited := seen[current]; visited {
			continue
		}
		seen[current] = struct{}{}
		queue = append(queue, successors[current]...)
	}
	return false
}

func (compiler *ProposalCompiler) compiledJoinTargets(
	tasks map[string]validatedTask,
	predecessors map[string][]string,
) map[string]bool {
	targets := make(map[string]bool, len(tasks))
	for targetID, sources := range predecessors {
		if len(sources) > 1 {
			targets[targetID] = true
			continue
		}
		if len(sources) != 1 {
			continue
		}
		source := tasks[sources[0]]
		target := tasks[targetID]
		if err := compiler.schemas.ValidateCompatibility(
			source.capability.OutputSchema,
			target.capability.InputSchema,
		); err != nil {
			targets[targetID] = true
		}
	}
	return targets
}

func compiledProposalDepth(
	order []string,
	predecessors map[string][]string,
	joinTargets map[string]bool,
	verifierTarget string,
	postVerifierDepth int,
) int {
	depths := make(map[string]int, len(order))
	maxDepth := 0
	for _, id := range order {
		depth := 1
		for _, predecessor := range predecessors[id] {
			if depths[predecessor]+1 > depth {
				depth = depths[predecessor] + 1
			}
		}
		if joinTargets[id] {
			depth++
		}
		if id == verifierTarget {
			depth += 1 + postVerifierDepth
		}
		depths[id] = depth
		if depth > maxDepth {
			maxDepth = depth
		}
	}
	return maxDepth
}

func validateEvidenceRefs(taskID string, refs []agentapi.EvidenceRef) error {
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		if ref.SourceKind == "" || ref.SourceKind != strings.TrimSpace(ref.SourceKind) ||
			ref.Target == "" || ref.Target != strings.TrimSpace(ref.Target) {
			return fmt.Errorf("task %q contains an invalid evidence reference", taskID)
		}
		key := ref.SourceKind + "\x00" + ref.Target + "\x00" + ref.Section +
			"\x00" + ref.Version + "\x00" + ref.TimeRange + "\x00" + ref.ContentHash
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("task %q contains a duplicate evidence reference", taskID)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateTaskBudgets(id string, budget NodeBudget) error {
	if budget.MaxInputTokens < 0 || budget.MaxOutputTokens < 0 ||
		budget.MaxTotalTokens < 0 || budget.MaxToolCalls < 0 ||
		budget.MaxCostMicros < 0 {
		return fmt.Errorf("capability %q node budget cannot be negative", id)
	}
	return nil
}

func tightenNodeBudget(
	taskID string,
	requested agentapi.TaskBudget,
	ceiling NodeBudget,
) (NodeBudget, error) {
	input, err := tightenInt64(
		"task "+taskID+" input tokens",
		requested.MaxInputTokens,
		ceiling.MaxInputTokens,
	)
	if err != nil {
		return NodeBudget{}, err
	}
	output, err := tightenInt64(
		"task "+taskID+" output tokens",
		requested.MaxOutputTokens,
		ceiling.MaxOutputTokens,
	)
	if err != nil {
		return NodeBudget{}, err
	}
	total, err := tightenInt64(
		"task "+taskID+" total tokens",
		requested.MaxTotalTokens,
		ceiling.MaxTotalTokens,
	)
	if err != nil {
		return NodeBudget{}, err
	}
	tools, err := tightenInt64(
		"task "+taskID+" tool calls",
		requested.MaxToolCalls,
		ceiling.MaxToolCalls,
	)
	if err != nil {
		return NodeBudget{}, err
	}
	cost, err := tightenInt64(
		"task "+taskID+" cost",
		requested.MaxCostMicros,
		ceiling.MaxCostMicros,
	)
	if err != nil {
		return NodeBudget{}, err
	}
	return NodeBudget{
		MaxInputTokens: input, MaxOutputTokens: output,
		MaxTotalTokens: total, MaxToolCalls: tools, MaxCostMicros: cost,
	}, nil
}

func ensureValuesSubset(subset, superset []string) error {
	allowed := make(map[string]struct{}, len(superset))
	for _, value := range superset {
		allowed[value] = struct{}{}
	}
	for _, value := range subset {
		if _, ok := allowed[value]; !ok {
			return fmt.Errorf("%q is outside the capability facets", value)
		}
	}
	return nil
}

type proposalLimits struct {
	maxTasks       int
	maxParallelism int
	maxAttempts    int
	maxRounds      int
	maxDepth       int
	workflowBudget Budget
}

func validateStopPolicy(
	stop agentapi.StopPolicy,
	policy CompilationPolicy,
) (proposalLimits, error) {
	maxTasks, err := tightenInt("stop max tasks", stop.MaxTasks, policy.MaxTasks)
	if err != nil {
		return proposalLimits{}, err
	}
	maxParallelism, err := tightenInt(
		"stop max parallelism",
		stop.MaxParallelism,
		policy.MaxParallelism,
	)
	if err != nil {
		return proposalLimits{}, err
	}
	maxAttempts, err := tightenInt(
		"stop max attempts",
		stop.MaxAttempts,
		policy.MaxAttempts,
	)
	if err != nil {
		return proposalLimits{}, err
	}
	maxRounds, err := tightenInt("stop max rounds", stop.MaxRounds, policy.MaxRounds)
	if err != nil {
		return proposalLimits{}, err
	}
	maxDepth, err := tightenInt("stop max depth", stop.MaxDepth, policy.MaxDepth)
	if err != nil {
		return proposalLimits{}, err
	}
	budget := policy.Budget
	budget.MaxDuplicateRatio, err = tightenRatio(
		"stop max duplicate ratio",
		stop.MaxDuplicateRatio,
		budget.MaxDuplicateRatio,
	)
	if err != nil {
		return proposalLimits{}, err
	}
	budget.MaxInputTokens, err = tightenInt64(
		"stop max input tokens",
		stop.MaxInputTokens,
		budget.MaxInputTokens,
	)
	if err != nil {
		return proposalLimits{}, err
	}
	budget.MaxOutputTokens, err = tightenInt64(
		"stop max output tokens",
		stop.MaxOutputTokens,
		budget.MaxOutputTokens,
	)
	if err != nil {
		return proposalLimits{}, err
	}
	budget.MaxTotalTokens, err = tightenInt64(
		"stop max total tokens",
		stop.MaxTotalTokens,
		budget.MaxTotalTokens,
	)
	if err != nil {
		return proposalLimits{}, err
	}
	budget.MaxToolCalls, err = tightenInt64(
		"stop max tool calls",
		stop.MaxToolCalls,
		budget.MaxToolCalls,
	)
	if err != nil {
		return proposalLimits{}, err
	}
	budget.MaxCostMicros, err = tightenInt64(
		"stop max cost",
		stop.MaxCostMicros,
		budget.MaxCostMicros,
	)
	if err != nil {
		return proposalLimits{}, err
	}
	budget.MaxRetries, err = tightenInt64(
		"stop max retries",
		stop.MaxRetries,
		budget.MaxRetries,
	)
	if err != nil {
		return proposalLimits{}, err
	}
	return proposalLimits{
		maxTasks: maxTasks, maxParallelism: maxParallelism,
		maxAttempts: maxAttempts, maxRounds: maxRounds, maxDepth: maxDepth,
		workflowBudget: budget,
	}, nil
}

func tightenInt(label string, requested, ceiling int) (int, error) {
	if requested < 0 {
		return 0, fmt.Errorf("%s cannot be negative", label)
	}
	if requested == 0 {
		return ceiling, nil
	}
	if requested > ceiling {
		return 0, fmt.Errorf("%s %d exceeds server limit %d", label, requested, ceiling)
	}
	return requested, nil
}

func tightenInt64(label string, requested, ceiling int64) (int64, error) {
	if requested < 0 {
		return 0, fmt.Errorf("%s cannot be negative", label)
	}
	if requested == 0 {
		return ceiling, nil
	}
	if requested > ceiling {
		return 0, fmt.Errorf("%s %d exceeds server limit %d", label, requested, ceiling)
	}
	return requested, nil
}

func tightenRatio(label string, requested, ceiling float64) (float64, error) {
	if requested < 0 || requested > 1 {
		return 0, fmt.Errorf("%s must be within [0,1]", label)
	}
	if ceiling < 0 || ceiling > 1 {
		return 0, fmt.Errorf("%s server limit must be within [0,1]", label)
	}
	if requested == 0 {
		return ceiling, nil
	}
	if ceiling == 0 || requested > ceiling {
		return 0, fmt.Errorf(
			"%s %.4f exceeds server limit %.4f",
			label,
			requested,
			ceiling,
		)
	}
	return requested, nil
}
