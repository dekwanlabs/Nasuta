package investigation

import (
	"sort"
	"strings"
)

// replanCandidateScore contains only deterministic signals already present in
// the contract and template catalog. It is intentionally not persisted: runtime
// outcome probabilities are not available at planning time and must not be
// invented from one observed question.
type replanCandidateScore struct {
	CandidateID           string
	GoalCoverageValue     int
	SourceMatchValue      int
	DependencyUnlockValue int
	IndependentUsefulness int
	EstimatedCost         int
	DuplicateRisk         int
	FailureRisk           int
	Total                 int
}

type replanSelection struct {
	candidate TaskCandidate
	bundle    []TaskCandidate
	score     replanCandidateScore
	newGoals  int
}

// selectReplanCandidates chooses a bounded, executable set-cover for the
// unresolved required goals. Dependencies already executed are satisfied;
// new dependencies are selected as a closed bundle so truncation cannot create
// an invalid later-round DAG.
func selectReplanCandidates(
	catalog *TaskTemplateCatalog,
	contract InvestigationContract,
	candidates []TaskCandidate,
	unresolved map[string]struct{},
	executed map[string]struct{},
	maxTasks int,
	available BudgetVector,
	coverage []GoalCoverage,
) []TaskCandidate {
	if len(candidates) == 0 || len(unresolved) == 0 {
		return nil
	}
	byID := make(map[string]TaskCandidate, len(candidates))
	ordered := append([]TaskCandidate(nil), candidates...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, candidate := range ordered {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		if _, done := executed[candidate.ID]; done {
			continue
		}
		// Candidate generation should already be unique. Keeping the first
		// stable entry makes selection deterministic if a custom catalog emits
		// the same ID twice.
		if _, exists := byID[candidate.ID]; !exists {
			byID[candidate.ID] = candidate
		}
	}
	if len(byID) == 0 {
		return nil
	}

	goals := make(map[string]EvidenceGoal, len(contract.Goals))
	for _, goal := range contract.Goals {
		goals[goal.ID] = goal
	}
	coverageByGoal := make(map[string]GoalCoverage, len(coverage))
	for _, item := range coverage {
		coverageByGoal[item.GoalID] = item
	}
	templates := make(map[string]TaskTemplate, len(byID))
	for id, candidate := range byID {
		if catalog == nil {
			continue
		}
		template, err := catalog.Resolve(candidate.Template)
		if err == nil {
			templates[id] = template
		}
	}
	unlockValues := dependencyUnlockValues(templates)
	selected := make(map[string]struct{}, len(byID))
	covered := make(map[string]struct{}, len(unresolved))
	used := BudgetVector{}
	selectedCount := 0

	for len(covered) < len(unresolved) {
		var best *replanSelection
		for _, candidate := range ordered {
			if _, exists := byID[candidate.ID]; !exists {
				continue
			}
			if _, exists := selected[candidate.ID]; exists {
				continue
			}
			bundle, ok := candidateBundle(candidate, byID, executed, selected)
			if !ok {
				continue
			}
			additionalTasks := len(bundle)
			if maxTasks > 0 && selectedCount+additionalTasks > maxTasks {
				continue
			}
			bundleBudget := BudgetVector{}
			newGoals := make(map[string]struct{})
			for _, item := range bundle {
				bundleBudget = addVector(bundleBudget, item.Budget)
				for _, goalID := range item.GoalIDs {
					if _, needed := unresolved[goalID]; !needed {
						continue
					}
					if _, already := covered[goalID]; !already {
						newGoals[goalID] = struct{}{}
					}
				}
			}
			if len(newGoals) == 0 {
				continue
			}
			if !fits(addVector(used, bundleBudget), available) {
				continue
			}
			score := scoreReplanCandidate(
				templates[candidate.ID], candidate, bundle, newGoals,
				goals, coverageByGoal, unlockValues,
			)
			selection := &replanSelection{
				candidate: candidate,
				bundle:    bundle,
				score:     score,
				newGoals:  len(newGoals),
			}
			if betterReplanSelection(selection, best) {
				best = selection
			}
		}
		if best == nil {
			// A later compiler pass will reject a plan that cannot cover every
			// required goal. Returning no partial plan keeps the round atomic.
			return nil
		}
		for _, item := range best.bundle {
			if _, exists := selected[item.ID]; exists {
				continue
			}
			selected[item.ID] = struct{}{}
			selectedCount++
			used = addVector(used, item.Budget)
			for _, goalID := range item.GoalIDs {
				if _, needed := unresolved[goalID]; needed {
					covered[goalID] = struct{}{}
				}
			}
		}
	}

	out := make([]TaskCandidate, 0, len(selected))
	for _, candidate := range ordered {
		if _, exists := selected[candidate.ID]; exists {
			out = append(out, candidate)
		}
	}
	return out
}

func candidateBundle(
	root TaskCandidate,
	byID map[string]TaskCandidate,
	executed map[string]struct{},
	selected map[string]struct{},
) ([]TaskCandidate, bool) {
	state := make(map[string]uint8)
	bundle := make([]TaskCandidate, 0, 1)
	var visit func(string) bool
	visit = func(id string) bool {
		if _, done := executed[id]; done {
			return true
		}
		if _, exists := selected[id]; exists {
			return true
		}
		switch state[id] {
		case 1:
			return false
		case 2:
			return true
		}
		candidate, exists := byID[id]
		if !exists {
			return false
		}
		state[id] = 1
		dependencies := append([]string(nil), candidate.Dependencies...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if !visit(dependency) {
				return false
			}
		}
		state[id] = 2
		bundle = append(bundle, candidate)
		return true
	}
	if !visit(root.ID) {
		return nil, false
	}
	return bundle, true
}

func scoreReplanCandidate(
	template TaskTemplate,
	candidate TaskCandidate,
	bundle []TaskCandidate,
	newGoals map[string]struct{},
	goals map[string]EvidenceGoal,
	coverage map[string]GoalCoverage,
	unlockValues map[string]int,
) replanCandidateScore {
	score := replanCandidateScore{CandidateID: candidate.ID}
	for goalID := range newGoals {
		goal := goals[goalID]
		score.GoalCoverageValue += 100
		if goal.HighRisk {
			score.GoalCoverageValue += 20
		}
		if goal.MinimumCoverage > 1 {
			score.GoalCoverageValue += 10
		}
		if coverage[goalID].Status == GoalPartial {
			// A partial goal has demonstrated some value already; prefer a
			// candidate that covers an unresolved goal first.
			score.GoalCoverageValue -= 20
		}
	}

	for goalID := range newGoals {
		goal := goals[goalID]
		required := make(map[string]struct{}, len(goal.RequiredSources))
		for _, source := range goal.RequiredSources {
			required[string(source)] = struct{}{}
		}
		preferred := make(map[string]struct{}, len(goal.Sources))
		for _, source := range goal.Sources {
			preferred[string(source)] = struct{}{}
		}
		for _, source := range template.SourceKinds {
			if _, ok := required[source]; ok {
				score.SourceMatchValue += 24
			} else if _, ok := preferred[source]; ok {
				score.SourceMatchValue += 12
			}
		}
	}

	for _, capability := range template.Provides {
		score.DependencyUnlockValue += unlockValues[capability]
	}
	if len(candidate.Dependencies) == 0 {
		score.IndependentUsefulness = 10
	}
	if len(bundle) > 1 {
		score.IndependentUsefulness -= len(bundle) - 1
	}
	for _, item := range bundle {
		score.EstimatedCost += estimatedBudgetCost(item.Budget)
	}
	// Candidate identity and discovery deduplication happen before scoring.
	// Runtime evidence duplication is measured by the admission policy, not
	// guessed from template metadata, so this remains conservatively zero.
	score.Total = score.GoalCoverageValue + score.SourceMatchValue +
		score.DependencyUnlockValue + score.IndependentUsefulness -
		score.EstimatedCost - score.DuplicateRisk - score.FailureRisk
	return score
}

func dependencyUnlockValues(templates map[string]TaskTemplate) map[string]int {
	requiredByCapability := make(map[string]int)
	requiresByTemplate := make(map[string]map[string]struct{}, len(templates))
	for candidateID, template := range templates {
		seen := make(map[string]struct{}, len(template.RequiredInputs))
		for _, required := range template.RequiredInputs {
			if _, duplicate := seen[required]; duplicate {
				continue
			}
			seen[required] = struct{}{}
			requiredByCapability[required]++
		}
		requiresByTemplate[candidateID] = seen
	}
	values := make(map[string]int, len(requiredByCapability))
	for candidateID, template := range templates {
		for _, provided := range template.Provides {
			count := requiredByCapability[provided]
			if _, selfRequires := requiresByTemplate[candidateID][provided]; selfRequires {
				count--
			}
			if count > 0 {
				values[provided] += count * 15
			}
		}
	}
	return values
}

func estimatedBudgetCost(budget BudgetVector) int {
	cost := budget.ToolCalls * 8
	if budget.TotalTokens > 0 {
		cost += int(budget.TotalTokens / 1000)
	} else {
		cost += int(budget.InputTokens / 1000)
		cost += int(budget.OutputTokens / 1000)
	}
	if budget.Duration > 0 {
		cost += int(budget.Duration / 10_000_000_000)
	}
	cost += int(budget.CostMicros / 1000)
	if cost < 0 {
		return int(^uint(0) >> 1)
	}
	return cost
}

func betterReplanSelection(candidate, current *replanSelection) bool {
	if current == nil {
		return true
	}
	left, right := candidate.score, current.score
	if left.Total != right.Total {
		return left.Total > right.Total
	}
	if candidate.newGoals != current.newGoals {
		return candidate.newGoals > current.newGoals
	}
	if left.GoalCoverageValue != right.GoalCoverageValue {
		return left.GoalCoverageValue > right.GoalCoverageValue
	}
	return left.CandidateID < right.CandidateID
}
