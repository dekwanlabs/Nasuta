package qa

import (
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

type taskEvidenceGroup struct {
	units  []tool.EvidenceUnit
	facets map[string]struct{}
}

func prepareInvestigationProposal(
	proposal *agentapi.TaskGraphProposal,
	contract TaskContract,
	seedEvidence []tool.EvidenceUnit,
) (*agentapi.TaskGraphProposal, error) {
	if proposal == nil {
		if len(contract.EvidenceGoals) == 0 {
			return nil, nil
		}
		fallback, err := buildTaskGraphFallback(contract)
		if err != nil {
			return nil, fmt.Errorf(
				"build scoped investigation task graph: %w",
				err,
			)
		}
		proposal = &fallback
	}
	assignTaskEvidenceOwners(proposal, seedEvidence)
	return proposal, nil
}

// assignTaskEvidenceOwners admits each seed unit to one investigator only.
func assignTaskEvidenceOwners(
	proposal *agentapi.TaskGraphProposal,
	seedEvidence []tool.EvidenceUnit,
) {
	if proposal == nil {
		return
	}
	investigators := make([]int, 0, len(proposal.Tasks))
	refSets := make(map[int]map[agentapi.EvidenceRef]struct{})
	for index := range proposal.Tasks {
		task := &proposal.Tasks[index]
		if task.OutputSchema.ID != "investigation.report" {
			continue
		}
		task.InputRefs = []agentapi.EvidenceRef{}
		investigators = append(investigators, index)
		refSets[index] = make(map[agentapi.EvidenceRef]struct{})
	}
	if len(investigators) == 0 || len(seedEvidence) == 0 {
		return
	}

	loads := make(map[int]int, len(investigators))
	for _, group := range groupTaskEvidence(seedEvidence) {
		owner := selectTaskEvidenceOwner(
			proposal.Tasks,
			investigators,
			group,
			loads,
		)
		if owner < 0 {
			continue
		}
		for _, unit := range group.units {
			for _, ref := range taskEvidenceRefs(unit) {
				if _, duplicate := refSets[owner][ref]; duplicate {
					continue
				}
				refSets[owner][ref] = struct{}{}
				proposal.Tasks[owner].InputRefs = append(
					proposal.Tasks[owner].InputRefs,
					ref,
				)
			}
		}
		loads[owner]++
	}
}

func groupTaskEvidence(
	units []tool.EvidenceUnit,
) []taskEvidenceGroup {
	groups := make([]taskEvidenceGroup, 0, len(units))
	indexByIdentity := make(map[string]int, len(units))
	for _, unit := range units {
		identity := taskEvidenceGroupIdentity(unit)
		index, exists := indexByIdentity[identity]
		if !exists {
			index = len(groups)
			indexByIdentity[identity] = index
			groups = append(groups, taskEvidenceGroup{
				facets: make(map[string]struct{}, len(unit.Facets)),
			})
		}
		group := &groups[index]
		group.units = append(group.units, unit)
		for _, facet := range unit.Facets {
			group.facets[facet] = struct{}{}
		}
	}
	return groups
}

func taskEvidenceGroupIdentity(unit tool.EvidenceUnit) string {
	return unit.SourceKind + "\x00" +
		unit.Target + "\x00" +
		unit.Version + "\x00" +
		unit.TimeRange
}

func selectTaskEvidenceOwner(
	tasks []agentapi.TaskSpec,
	investigators []int,
	group taskEvidenceGroup,
	loads map[int]int,
) int {
	best := -1
	bestFacetCount := 0
	bestLoad := 0
	for _, index := range investigators {
		task := tasks[index]
		if len(group.units) == 0 ||
			!capabilityOwnsEvidenceSource(
				task.Capability,
				group.units[0].SourceKind,
			) {
			continue
		}
		facetCount := matchingTaskFacetCount(task.RequiredFacets, group.facets)
		if facetCount == 0 {
			continue
		}
		load := loads[index]
		if best < 0 ||
			facetCount > bestFacetCount ||
			facetCount == bestFacetCount && load < bestLoad {
			best = index
			bestFacetCount = facetCount
			bestLoad = load
		}
	}
	return best
}

func matchingTaskFacetCount(
	required []string,
	available map[string]struct{},
) int {
	count := 0
	for _, facet := range required {
		if _, ok := available[facet]; ok {
			count++
		}
	}
	return count
}

func capabilityOwnsEvidenceSource(
	capability string,
	sourceKind string,
) bool {
	switch capability {
	case "knowledge.code.inspect":
		return sourceKind == "code" || sourceKind == "codegraph"
	case "knowledge.service.trace", "knowledge.runtime.observe":
		return sourceKind == "service" ||
			sourceKind == "dependency" ||
			sourceKind == "runtime" ||
			sourceKind == "codegraph"
	case "knowledge.docs.verify":
		return sourceKind == "runbook" ||
			sourceKind == "generated_doc" ||
			sourceKind == "doc" ||
			sourceKind == "docs"
	case "knowledge.web.research":
		return sourceKind == "web" || sourceKind == "external"
	case "knowledge.memory.recall":
		return sourceKind == "memory"
	default:
		return false
	}
}

func taskEvidenceRefs(unit tool.EvidenceUnit) []agentapi.EvidenceRef {
	ref := agentapi.EvidenceRef{
		SourceKind:  unit.SourceKind,
		Target:      unit.Target,
		Version:     unit.Version,
		TimeRange:   unit.TimeRange,
		ContentHash: unit.ContentHash,
	}
	if len(unit.Sections) == 0 {
		return []agentapi.EvidenceRef{ref}
	}
	refs := make([]agentapi.EvidenceRef, 0, len(unit.Sections))
	for _, section := range unit.Sections {
		ref.Section = section
		refs = append(refs, ref)
	}
	return refs
}
