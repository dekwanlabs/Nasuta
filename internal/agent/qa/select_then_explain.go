package qa

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	"github.com/dekwanlabs/nasuta/internal/domain"
)

const (
	discoverBusinessesGoalID = "discover_businesses"
	coreBusinessRole         = "core_business"
)

// applyDiscoverThenSelect narrows a no-entity contract that still has a
// business-domain goal to a single inventory wave. Named subjects are never
// invented here; later rounds bind whatever the investigator reported.
func applyDiscoverThenSelect(contract TaskContract) TaskContract {
	if contract.DiscoveryPhase || len(contract.Entities) > 0 {
		return contract
	}
	if !needsSubjectDiscovery(contract) {
		return contract
	}
	discovery, deferred := splitDiscoveryEvidenceGoals(contract.EvidenceGoals)
	if len(discovery) == 0 {
		return contract
	}
	next := cloneTaskContract(contract)
	next.DiscoveryPhase = true
	next.SelectCount = maxInvestigationTasks
	next.EvidenceGoals = discovery
	next.DeferredEvidenceGoals = deferred
	next.InvestigationGoals = []InvestigationGoal{{
		ID:                  discoverBusinessesGoalID,
		Objective:           discoverBusinessesObjective(next.SelectCount),
		IndependentlyUseful: true,
		DependsOn:           []string{},
	}}
	return next
}

// needsSubjectDiscovery is true when the contract asks about businesses but
// has no named subjects yet. That is a structural gap, not a phrase match.
func needsSubjectDiscovery(contract TaskContract) bool {
	if contract.DiscoveryPhase || len(contract.Entities) > 0 {
		return false
	}
	return hasBusinessDomainGoal(contract.EvidenceGoals)
}

func hasBusinessDomainGoal(goals []EvidenceGoal) bool {
	for _, goal := range goals {
		if evidenceGoalID(goal) == string(domain.FacetBusinessDomain) ||
			goal.Facet == string(domain.FacetBusinessDomain) {
			return true
		}
	}
	return false
}

func splitDiscoveryEvidenceGoals(goals []EvidenceGoal) (discovery, deferred []EvidenceGoal) {
	discovery = make([]EvidenceGoal, 0, 1)
	deferred = make([]EvidenceGoal, 0, len(goals))
	for _, goal := range goals {
		cloned := cloneEvidenceGoal(goal)
		if evidenceGoalID(goal) == string(domain.FacetBusinessDomain) ||
			goal.Facet == string(domain.FacetBusinessDomain) {
			cloned.Required = true
			discovery = append(discovery, cloned)
			continue
		}
		deferred = append(deferred, cloned)
	}
	return discovery, deferred
}

func discoverBusinessesObjective(limit int) string {
	return fmt.Sprintf(
		"Inventory user-facing business domains with cited evidence. Name the domains people operate, not repository, organization, or module prefixes. Do not invent names. Later rounds will take at most %d discovered businesses and gather evidence for each.",
		limit,
	)
}

func resultHasValidReport(result InvestigationResult) bool {
	if len(result.DiscoveredEntities) > 0 {
		return true
	}
	for _, claim := range result.SupportedClaims {
		if investigation.UserReadableClaimText(claim.Claim) {
			return true
		}
	}
	for _, claim := range result.PartialClaims {
		if investigation.UserReadableClaimText(claim.Claim) {
			return true
		}
	}
	return false
}

func pendingEntitySelection(contract TaskContract, result InvestigationResult) bool {
	if !contract.DiscoveryPhase || len(contract.Entities) > 0 {
		return false
	}
	return len(selectDiscoveredEntities(result, contract.SelectCount)) > 0
}

func selectDiscoveredEntities(result InvestigationResult, limit int) []EntityRef {
	if limit <= 0 {
		limit = maxInvestigationTasks
	}
	if limit > maxInvestigationTasks {
		limit = maxInvestigationTasks
	}
	specs := make([]domain.EntitySpec, 0, len(result.DiscoveredEntities)+len(result.SupportedClaims)+len(result.PartialClaims))
	for _, name := range result.DiscoveredEntities {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		specs = append(specs, domain.EntitySpec{ID: name, Label: name, Role: coreBusinessRole})
	}
	for _, claim := range append(append([]InvestigationClaim(nil), result.SupportedClaims...), result.PartialClaims...) {
		for _, id := range claim.EntityIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			specs = append(specs, domain.EntitySpec{ID: id, Label: id, Role: coreBusinessRole})
		}
	}
	canonical := domain.CanonicalEntitySpecs(specs)
	if len(canonical) > limit {
		canonical = canonical[:limit]
	}
	entities := make([]EntityRef, 0, len(canonical))
	for _, spec := range canonical {
		if !investigationGoalIDOK(spec.ID) {
			continue
		}
		entities = append(entities, EntityRef{
			ID: spec.ID, Label: spec.Label, Role: spec.Role,
			Aliases: append([]string(nil), spec.Aliases...),
		})
	}
	return entities
}

func bindSelectedEntitiesContract(
	previous TaskContract,
	result InvestigationResult,
) (TaskContract, bool) {
	if !previous.DiscoveryPhase {
		return TaskContract{}, false
	}
	selected := selectDiscoveredEntities(result, previous.SelectCount)
	if len(selected) == 0 {
		return TaskContract{}, false
	}
	next := cloneTaskContract(previous)
	next.DiscoveryPhase = false
	next.Entities = selected
	next.DeferredEvidenceGoals = nil
	next.EvidenceGoals = restoreDeferredEvidenceGoals(previous)
	next.InvestigationGoals = investigationGoalsForEntities(selected)
	next.TaskEvidenceAssignments = nil
	return next, true
}

// subjectExplainRound is the one wave after discovery bound named businesses.
// Facet leftovers are not a reason to open another coordinator round.
func subjectExplainRound(contract TaskContract) bool {
	if contract.DiscoveryPhase || len(contract.Entities) == 0 {
		return false
	}
	if len(contract.InvestigationGoals) == 0 ||
		len(contract.InvestigationGoals) != len(contract.Entities) {
		return false
	}
	ids := make(map[string]struct{}, len(contract.Entities))
	for _, entity := range contract.Entities {
		id := strings.TrimSpace(entity.ID)
		if id == "" {
			return false
		}
		if _, duplicate := ids[id]; duplicate {
			return false
		}
		ids[id] = struct{}{}
	}
	for _, goal := range contract.InvestigationGoals {
		if _, ok := ids[strings.TrimSpace(goal.ID)]; !ok {
			return false
		}
	}
	return true
}

func restoreDeferredEvidenceGoals(previous TaskContract) []EvidenceGoal {
	goals := make([]EvidenceGoal, 0, len(previous.DeferredEvidenceGoals)+len(previous.EvidenceGoals))
	seen := make(map[string]struct{}, len(previous.DeferredEvidenceGoals)+1)
	for _, goal := range previous.DeferredEvidenceGoals {
		cloned := cloneEvidenceGoal(goal)
		cloned.Required = false
		id := evidenceGoalID(cloned)
		seen[id] = struct{}{}
		goals = append(goals, cloned)
	}
	for _, goal := range previous.EvidenceGoals {
		id := evidenceGoalID(goal)
		if _, exists := seen[id]; exists {
			continue
		}
		cloned := cloneEvidenceGoal(goal)
		cloned.Required = id == string(domain.FacetBusinessDomain)
		goals = append(goals, cloned)
	}
	return goals
}

func investigationGoalsForEntities(entities []EntityRef) []InvestigationGoal {
	goals := make([]InvestigationGoal, 0, len(entities))
	for _, entity := range entities {
		label := strings.TrimSpace(entity.Label)
		if label == "" {
			label = entity.ID
		}
		goals = append(goals, InvestigationGoal{
			ID: entity.ID,
			Objective: fmt.Sprintf(
				"Explain core business %s with cited evidence covering its entrypoints, flow, and data.",
				label,
			),
			IndependentlyUseful: true,
			DependsOn:           []string{},
		})
	}
	return goals
}

func investigationGoalIDOK(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	for index, r := range id {
		if index == 0 {
			if r < 'a' || r > 'z' {
				return false
			}
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("._-", r) {
			continue
		}
		return false
	}
	return true
}
