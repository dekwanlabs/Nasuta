package investigation

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

type SchemaResolver interface {
	Resolve(agentapi.SchemaRef) (agentapi.SchemaDefinition, error)
}

type schemaValidator interface {
	Validate(agentapi.SchemaRef, json.RawMessage) error
}

type PlanCompiler struct {
	Catalog  *TaskTemplateCatalog
	Schemas  SchemaResolver
	Tools    tool.Snapshot
	Ledger   *BudgetLedger
	MaxTasks int
	// Overhead is the non-execution stage reserve the initial plan must leave
	// room for (planning, verification, fallback). Composition is already
	// hard-reserved before planning and is therefore excluded.
	Overhead BudgetVector
}

func (compiler PlanCompiler) CompileGenerated(contract InvestigationContract) (PlanRevision, error) {
	if compiler.Catalog == nil {
		return PlanRevision{}, fmt.Errorf("%w: task template catalog is required", ErrPlanInvalid)
	}
	candidates, err := compiler.Catalog.GenerateCandidates(contract)
	if err != nil {
		return PlanRevision{}, err
	}
	// The verifier is a fixed server-owned stage, not one of the evidence
	// collection slots. Select evidence tasks first, then append exactly one
	// verifier so MaxTasks cannot accidentally remove the safety gate. A
	// reduced test catalog without the canonical template keeps the legacy
	// low-level planning behavior; the production catalog always registers it.
	if !compiler.Catalog.Has(TaskTemplateRef{ID: "evidence.verify", Version: 1}) {
		selected := selectInitialCandidates(contract, candidates, compiler.MaxTasks)
		return compiler.compile(contract, dropUnresolvedCandidates(selected), nil, 1)
	}
	evidenceCandidates, err := splitVerifierCandidates(compiler.Catalog, candidates)
	if err != nil {
		return PlanRevision{}, err
	}
	selected := selectInitialCandidates(contract, evidenceCandidates, compiler.MaxTasks)
	selected = dropUnresolvedCandidates(selected)
	selected, err = appendServerVerifierCandidate(compiler.Catalog, contract, selected)
	if err != nil {
		return PlanRevision{}, err
	}
	plan, err := compiler.compile(contract, selected, nil, 1)
	if err != nil {
		return PlanRevision{}, err
	}
	if err := validateVerifierPlan(plan.Tasks); err != nil {
		return PlanRevision{}, err
	}
	return plan, nil
}

func (compiler PlanCompiler) Compile(contract InvestigationContract, candidates []TaskCandidate) (PlanRevision, error) {
	return compiler.compile(contract, candidates, nil, 1)
}

func splitVerifierCandidates(
	catalog *TaskTemplateCatalog,
	candidates []TaskCandidate,
) ([]TaskCandidate, error) {
	evidence := make([]TaskCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		template, err := catalog.Resolve(candidate.Template)
		if err != nil {
			return nil, fmt.Errorf("%w: task %q template: %v", ErrPlanInvalid, candidate.ID, err)
		}
		// Generated and planner-provided verifier candidates are discarded here;
		// appendServerVerifierCandidate installs the one server-owned stage.
		if template.Executor == ExecutorVerifier {
			continue
		}
		evidence = append(evidence, candidate)
	}
	return evidence, nil
}

func selectVerifierCandidate(
	contract InvestigationContract,
	catalog *TaskTemplateCatalog,
) (TaskCandidate, error) {
	// The canonical verifier is resolved through the catalog when available;
	// TaskTemplateCatalog.Resolve supplies its server-owned definition for a
	// reduced catalog as well.
	template, err := catalog.Resolve(TaskTemplateRef{ID: "evidence.verify", Version: 1})
	if err != nil {
		return TaskCandidate{}, fmt.Errorf("%w: server-owned verifier: %v", ErrCapabilityGap, err)
	}
	return verifierCandidate(template, contract), nil
}

func verifierCandidate(template TaskTemplate, contract InvestigationContract) TaskCandidate {
	goalIDs := make([]string, 0, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		if templateMatchesGoal(template, goal) {
			goalIDs = append(goalIDs, goal.ID)
		}
	}
	return TaskCandidate{
		ID:              "evidence.verify",
		Template:        TaskTemplateRef{ID: "evidence.verify", Version: 1},
		Objective:       "Verify the evidence produced by the investigation tasks.",
		EvidenceGoalIDs: goalIDs,
		AllowedTools:    append([]tool.ToolID(nil), template.ToolGrant...),
		Budget:          taskCandidateBudget(template),
	}
}

func (compiler PlanCompiler) CompileReplan(
	contract InvestigationContract,
	candidates []TaskCandidate,
	coveredRequired map[string]struct{},
	revision int,
) (PlanRevision, error) {
	if revision < 2 {
		return PlanRevision{}, fmt.Errorf("%w: replan revision must be at least 2", ErrPlanInvalid)
	}
	var err error
	serverOwnedVerifier := compiler.Catalog.Has(TaskTemplateRef{ID: "evidence.verify", Version: 1})
	if serverOwnedVerifier {
		candidates, err = appendServerVerifierCandidate(compiler.Catalog, contract, candidates)
		if err != nil {
			return PlanRevision{}, err
		}
	}
	plan, err := compiler.compile(contract, candidates, coveredRequired, revision)
	if err != nil {
		return PlanRevision{}, err
	}
	if serverOwnedVerifier {
		if err := validateVerifierPlan(plan.Tasks); err != nil {
			return PlanRevision{}, err
		}
	}
	return plan, nil
}

func appendServerVerifierCandidate(
	catalog *TaskTemplateCatalog,
	contract InvestigationContract,
	candidates []TaskCandidate,
) ([]TaskCandidate, error) {
	evidence, err := splitVerifierCandidates(catalog, candidates)
	if err != nil {
		return nil, err
	}
	verifier, err := selectVerifierCandidate(contract, catalog)
	if err != nil {
		return nil, err
	}
	return append(evidence, verifier), nil
}

func (compiler PlanCompiler) compile(
	contract InvestigationContract,
	candidates []TaskCandidate,
	coveredRequired map[string]struct{},
	revision int,
) (PlanRevision, error) {
	if compiler.Catalog == nil {
		return PlanRevision{}, fmt.Errorf("%w: task template catalog is required", ErrPlanInvalid)
	}
	if strings.TrimSpace(contract.ID) == "" || strings.TrimSpace(contract.Question) == "" {
		return PlanRevision{}, fmt.Errorf("%w: contract id and question are required", ErrPlanInvalid)
	}
	if compiler.Schemas == nil {
		return PlanRevision{}, fmt.Errorf("%w: schema registry is required", ErrCapabilityGap)
	}
	goals, err := indexGoals(contract.EvidenceGoals)
	if err != nil {
		return PlanRevision{}, err
	}
	maxTasks := compiler.MaxTasks
	if contract.MaxTasks > 0 && (maxTasks == 0 || contract.MaxTasks < maxTasks) {
		maxTasks = contract.MaxTasks
	}
	evidenceTaskCount := len(candidates)
	for _, candidate := range candidates {
		template, err := compiler.Catalog.Resolve(candidate.Template)
		if err != nil {
			return PlanRevision{}, fmt.Errorf("%w: task %q template: %v", ErrPlanInvalid, candidate.ID, err)
		}
		if template.Executor == ExecutorVerifier {
			evidenceTaskCount--
		}
	}
	if maxTasks > 0 && evidenceTaskCount > maxTasks {
		return PlanRevision{}, fmt.Errorf("%w: plan has %d evidence tasks, maximum is %d", ErrPlanInvalid, evidenceTaskCount, maxTasks)
	}
	tasks := make([]ExecutableTask, 0, len(candidates))
	seenTasks := make(map[string]struct{}, len(candidates))
	coveredGoals := make(map[string]struct{}, len(goals))
	totalBudget := BudgetVector{}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.ID) == "" {
			return PlanRevision{}, fmt.Errorf("%w: task id is required", ErrPlanInvalid)
		}
		if _, duplicate := seenTasks[candidate.ID]; duplicate {
			return PlanRevision{}, fmt.Errorf("%w: task id %q is duplicated", ErrPlanInvalid, candidate.ID)
		}
		seenTasks[candidate.ID] = struct{}{}
		template, err := compiler.Catalog.Resolve(candidate.Template)
		if err != nil {
			return PlanRevision{}, fmt.Errorf("%w: task %q template: %v", ErrPlanInvalid, candidate.ID, err)
		}
		if strings.TrimSpace(candidate.Objective) == "" {
			return PlanRevision{}, fmt.Errorf("%w: task %q objective is required", ErrPlanInvalid, candidate.ID)
		}
		for _, goalID := range candidate.EvidenceGoalIDs {
			goal, ok := goals[goalID]
			if !ok {
				return PlanRevision{}, fmt.Errorf("%w: task %q references unknown goal %q", ErrPlanInvalid, candidate.ID, goalID)
			}
			if !templateMatchesGoal(template, goal) {
				return PlanRevision{}, fmt.Errorf("%w: task %q template %q does not cover goal %q", ErrPlanInvalid, candidate.ID, template.ID, goalID)
			}
			coveredGoals[goalID] = struct{}{}
		}
		if len(candidate.EvidenceGoalIDs) == 0 {
			return PlanRevision{}, fmt.Errorf("%w: task %q must target at least one goal", ErrPlanInvalid, candidate.ID)
		}
		allowedTools, err := compiler.compileToolGrant(contract, template, candidate.AllowedTools)
		if err != nil {
			return PlanRevision{}, fmt.Errorf("%w: task %q tools: %v", ErrPlanInvalid, candidate.ID, err)
		}
		budget := candidate.Budget
		explicitBudget := !isZeroBudget(budget)
		// Agent-backed tasks reuse the Single-Agent definition's step/tool
		// limits. They must not inherit a template CostProfile as an implicit
		// token/tool quota, because that would recreate the old per-agent
		// allocation bug (for example ToolCalls: 1). Explicit proposal limits
		// are still honored as an intentional narrowing.
		if !explicitBudget && !isAgentExecutor(template.Executor) {
			budget = template.CostProfile
		}
		budgetLimit := template.CostProfile
		if !budgetWithin(budget, budgetLimit) {
			return PlanRevision{}, fmt.Errorf("%w: task %q budget exceeds template cost profile", ErrPlanInvalid, candidate.ID)
		}
		if err := validateBudgetVector(budget); err != nil {
			return PlanRevision{}, fmt.Errorf("%w: task %q budget: %v", ErrPlanInvalid, candidate.ID, err)
		}
		if _, err := compiler.Schemas.Resolve(template.InputSchema); err != nil {
			return PlanRevision{}, fmt.Errorf("%w: task %q input schema: %v", ErrPlanInvalid, candidate.ID, err)
		}
		if _, err := compiler.Schemas.Resolve(template.OutputSchema); err != nil {
			return PlanRevision{}, fmt.Errorf("%w: task %q output schema: %v", ErrPlanInvalid, candidate.ID, err)
		}
		task := ExecutableTask{
			ID:                   candidate.ID,
			Template:             candidate.Template,
			Objective:            candidate.Objective,
			EvidenceGoalIDs:      append([]string(nil), candidate.EvidenceGoalIDs...),
			InvestigationGoalIDs: append([]string(nil), candidate.InvestigationGoalIDs...),
			Capability:           candidate.Capability,
			EvidenceGoals:        cloneEvidenceGoals(candidate.EvidenceGoals),
			Entities:             append([]string(nil), candidate.Entities...),
			AllowedTools:         allowedTools,
			InputRefs:            append([]EvidenceRef(nil), candidate.InputRefs...),
			Dependencies:         uniqueStrings(candidate.Dependencies),
			Budget: TaskBudget{
				Limit:       budget,
				MaxAttempts: maxTaskAttempts(firstPositive(candidate.MaxAttempts, template.MaxAttempts)),
			},
			InputSchema:   template.InputSchema,
			OutputSchema:  template.OutputSchema,
			Executor:      template.Executor,
			ToolCalls:     append([]ToolCallSpec(nil), template.ToolCalls...),
			Optional:      candidate.Optional,
			AllowParallel: candidate.AllowParallel || template.AllowParallel || (template.Executor == ExecutorInvestigator),
			Status:        TaskPending,
		}
		tasks = append(tasks, task)
		totalBudget = addVector(totalBudget, budget)
	}
	for _, goal := range contract.EvidenceGoals {
		if goal.Required {
			if _, covered := coveredRequired[goal.ID]; covered {
				continue
			}
			if _, ok := coveredGoals[goal.ID]; !ok {
				return PlanRevision{}, fmt.Errorf("%w: required goal %q has no candidate task", ErrPlanInvalid, goal.ID)
			}
		}
	}
	tasks = ensureVerifierRunsAfterEvidence(tasks)
	if err := validateDependencies(tasks); err != nil {
		return PlanRevision{}, err
	}
	if compiler.Ledger != nil {
		if err := compiler.Ledger.CanReserve(StageExecution, "plan-total", totalBudget); err != nil {
			return PlanRevision{}, fmt.Errorf("%w: planned task budget: %v", ErrBudgetExceeded, err)
		}
		if !isZeroBudget(compiler.Overhead) {
			if err := compiler.Ledger.CanReserveRun("plan-overhead", compiler.Overhead); err != nil {
				return PlanRevision{}, fmt.Errorf("%w: planned stage overhead: %v", ErrBudgetExceeded, err)
			}
		}
	}
	return PlanRevision{
		Revision:   revision,
		ContractID: contract.ID,
		Tasks:      tasks,
		CreatedAt:  time.Now().UTC(),
	}, nil
}

func validateVerifierPlan(tasks []ExecutableTask) error {
	count := 0
	for _, task := range tasks {
		if task.Executor == ExecutorVerifier {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("%w: executable plan must contain exactly one verifier, found %d", ErrPlanInvalid, count)
	}
	return nil
}

func ensureVerifierRunsAfterEvidence(tasks []ExecutableTask) []ExecutableTask {
	evidenceTasks := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if task.Executor != ExecutorVerifier {
			evidenceTasks = append(evidenceTasks, task.ID)
		}
	}
	for index := range tasks {
		if tasks[index].Executor != ExecutorVerifier {
			continue
		}
		dependencies := make(map[string]struct{}, len(tasks[index].Dependencies)+len(evidenceTasks))
		for _, dependency := range tasks[index].Dependencies {
			dependencies[dependency] = struct{}{}
		}
		for _, dependency := range evidenceTasks {
			dependencies[dependency] = struct{}{}
		}
		merged := make([]string, 0, len(dependencies))
		for dependency := range dependencies {
			merged = append(merged, dependency)
		}
		sort.Strings(merged)
		tasks[index].Dependencies = merged
	}
	return tasks
}

func (compiler PlanCompiler) compileToolGrant(
	contract InvestigationContract,
	template TaskTemplate,
	candidate []tool.ToolID,
) ([]tool.ToolID, error) {
	grant := candidate
	if len(grant) == 0 {
		grant = template.ToolGrant
	}
	templateSet := makeSet(template.ToolGrant)
	contractSet := makeSet(contract.AllowedToolIDs)
	principalSet := makeSet(contract.PrincipalToolIDs)
	workspaceSet := makeSet(contract.WorkspaceToolIDs)
	seen := make(map[tool.ToolID]struct{}, len(grant))
	for _, id := range grant {
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("tool %q is duplicated", id)
		}
		seen[id] = struct{}{}
		if _, allowed := templateSet[id]; !allowed {
			return nil, fmt.Errorf("tool %q is outside template grant", id)
		}
		if !allowedBySet(contractSet, contract.AllowedToolIDs, id) ||
			!allowedBySet(principalSet, contract.PrincipalToolIDs, id) ||
			!allowedBySet(workspaceSet, contract.WorkspaceToolIDs, id) {
			return nil, fmt.Errorf("tool %q is outside effective permission intersection", id)
		}
		if _, available := compiler.Tools.Get(id); !available {
			return nil, fmt.Errorf("tool %q is unavailable in pinned registry snapshot", id)
		}
	}
	sort.Slice(grant, func(i, j int) bool { return grant[i] < grant[j] })
	return append([]tool.ToolID(nil), grant...), nil
}

func indexGoals(goals []EvidenceGoal) (map[string]EvidenceGoal, error) {
	indexed := make(map[string]EvidenceGoal, len(goals))
	if len(goals) == 0 {
		return nil, fmt.Errorf("%w: at least one evidence goal is required", ErrPlanInvalid)
	}
	for _, goal := range goals {
		if strings.TrimSpace(goal.ID) == "" || strings.TrimSpace(goal.Kind) == "" {
			return nil, fmt.Errorf("%w: goal id and kind are required", ErrPlanInvalid)
		}
		if _, duplicate := indexed[goal.ID]; duplicate {
			return nil, fmt.Errorf("%w: goal %q is duplicated", ErrPlanInvalid, goal.ID)
		}
		indexed[goal.ID] = goal
	}
	return indexed, nil
}

func validateDependencies(tasks []ExecutableTask) error {
	known := make(map[string]struct{}, len(tasks))
	for _, task := range tasks {
		known[task.ID] = struct{}{}
	}
	indegree := make(map[string]int, len(tasks))
	children := make(map[string][]string, len(tasks))
	for _, task := range tasks {
		for _, dependency := range task.Dependencies {
			if dependency == task.ID {
				return fmt.Errorf("%w: task %q depends on itself", ErrPlanInvalid, task.ID)
			}
			if _, ok := known[dependency]; !ok {
				return fmt.Errorf("%w: task %q depends on unknown task %q", ErrPlanInvalid, task.ID, dependency)
			}
			indegree[task.ID]++
			children[dependency] = append(children[dependency], task.ID)
		}
	}
	ready := make([]string, 0, len(tasks))
	for _, task := range tasks {
		if indegree[task.ID] == 0 {
			ready = append(ready, task.ID)
		}
	}
	visited := 0
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		visited++
		for _, child := range children[id] {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
			}
		}
	}
	if visited != len(tasks) {
		return fmt.Errorf("%w: task dependency graph contains a cycle", ErrPlanInvalid)
	}
	return nil
}

func selectInitialCandidates(contract InvestigationContract, candidates []TaskCandidate, maxTasks int) []TaskCandidate {
	if maxTasks <= 0 {
		return candidates
	}
	selected := make([]TaskCandidate, 0, minInt(maxTasks, len(candidates)))
	covered := make(map[string]struct{})
	for _, candidate := range candidates {
		needed := false
		for _, goalID := range candidate.EvidenceGoalIDs {
			if _, ok := covered[goalID]; !ok {
				needed = true
				break
			}
		}
		if !needed && len(selected) >= maxTasks {
			continue
		}
		selected = append(selected, candidate)
		for _, goalID := range candidate.EvidenceGoalIDs {
			covered[goalID] = struct{}{}
		}
		if len(selected) >= maxTasks {
			break
		}
	}
	return selected
}

// dropUnresolvedCandidates removes candidates whose dependencies were truncated
// away by MaxTasks. It preserves a valid executable DAG rather than failing the
// whole plan later with an unknown dependency.
func dropUnresolvedCandidates(candidates []TaskCandidate) []TaskCandidate {
	for {
		known := make(map[string]struct{}, len(candidates))
		for _, candidate := range candidates {
			known[candidate.ID] = struct{}{}
		}
		kept := make([]TaskCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			complete := true
			for _, dependency := range candidate.Dependencies {
				if _, ok := known[dependency]; !ok {
					complete = false
					break
				}
			}
			if complete {
				kept = append(kept, candidate)
			}
		}
		if len(kept) == len(candidates) {
			return kept
		}
		candidates = kept
	}
}

func makeSet(ids []tool.ToolID) map[tool.ToolID]struct{} {
	set := make(map[tool.ToolID]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

func allowedBySet(set map[tool.ToolID]struct{}, ids []tool.ToolID, wanted tool.ToolID) bool {
	if len(ids) == 0 {
		return true
	}
	_, ok := set[wanted]
	return ok
}

func uniqueStrings(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func isZeroBudget(value BudgetVector) bool {
	return value == (BudgetVector{})
}

func budgetWithin(value, limit BudgetVector) bool {
	return (limit.InputTokens == 0 || value.InputTokens <= limit.InputTokens) &&
		(limit.OutputTokens == 0 || value.OutputTokens <= limit.OutputTokens) &&
		(limit.TotalTokens == 0 || tokenTotal(value) <= limit.TotalTokens) &&
		(limit.ToolCalls == 0 || value.ToolCalls <= limit.ToolCalls) &&
		(limit.Duration == 0 || value.Duration <= limit.Duration) &&
		(limit.CostMicros == 0 || value.CostMicros <= limit.CostMicros)
}

func tokenTotal(value BudgetVector) int64 {
	if value.TotalTokens > 0 {
		return value.TotalTokens
	}
	return saturatingAdd(value.InputTokens, value.OutputTokens)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func cloneEvidenceGoals(goals []EvidenceGoal) []EvidenceGoal {
	if len(goals) == 0 {
		return nil
	}
	out := make([]EvidenceGoal, len(goals))
	for index, goal := range goals {
		out[index] = goal
		out[index].Facets = append([]string(nil), goal.Facets...)
	}
	return out
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func maxTaskAttempts(configured int) int {
	if configured <= 0 {
		return 1
	}
	return configured
}
