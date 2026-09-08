package execution

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	fallbackMaxFindings = 50
	fallbackMaxGoals    = 50
	fallbackMaxNodes    = 32
	fallbackMaxEdges    = 32
)

// BuildEvidencePreservingReport reconstructs a schema-shaped
// investigation.report from evidence already collected by a child
// investigator, so a truncated or failed structured conclusion never discards
// the findings/flow/gaps the run actually gathered.
//
// It is deliberately conservative:
//   - every finding/flow edge is derived from an evidence unit or observation;
//   - flow edge evidence_state is only ever "inferred" (never "verified");
//   - covered/unresolved goals are partitioned from unit facets or the stable
//     source-kind facet mapping, not from model output;
//   - output is only returned when it is self-consistent JSON; callers still
//     run the schema validator before admitting it.
//
// The helper lives in the execution package (not catalog) so both the
// execution-loop fallback and definition recovery can share it without
// importing the schema registry.
func BuildEvidencePreservingReport(
	units []tool.EvidenceUnit,
	observations []agentapi.EvidenceObservation,
	required []string,
	focus string,
) (json.RawMessage, bool) {
	units = normalizeFallbackUnits(units)
	observations = normalizeFallbackObservations(observations)
	if len(units) == 0 && len(observations) == 0 {
		return nil, false
	}
	focus = strings.TrimSpace(focus)
	if focus == "" {
		focus = "docs"
	}

	findings := fallbackFindings(units, observations)
	flow := fallbackFlow(units)
	covered, unresolved := partitionFallbackGoals(units, observations, required)

	report := map[string]any{
		"focus":    focus,
		"summary":  "Evidence collection completed, but the final report could not be generated; the collected evidence is preserved below.",
		"findings": findings,
		"gaps": []string{
			"Evidence collection completed, but report generation ended before a schema-valid investigation.report was produced.",
		},
		"covered_evidence_goals":    covered,
		"unresolved_evidence_goals": unresolved,
	}
	if flow != nil {
		report["flow"] = flow
	}

	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, false
	}
	if !json.Valid(encoded) {
		return nil, false
	}
	return encoded, true
}

// normalizeFallbackUnits expands multi-section units and drops units without
// the identity required to derive either a finding or a flow element.
func normalizeFallbackUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	expanded := evidence.Expand(units)
	out := make([]tool.EvidenceUnit, 0, len(expanded))
	for _, unit := range expanded {
		unit.SourceKind = strings.TrimSpace(unit.SourceKind)
		unit.Target = strings.TrimSpace(unit.Target)
		if unit.SourceKind == "" || unit.Target == "" {
			continue
		}
		out = append(out, unit)
	}
	return out
}

func normalizeFallbackObservations(
	observations []agentapi.EvidenceObservation,
) []agentapi.EvidenceObservation {
	out := make([]agentapi.EvidenceObservation, 0, len(observations))
	for _, observation := range observations {
		observation.SourceKind = strings.TrimSpace(observation.SourceKind)
		observation.Target = strings.TrimSpace(observation.Target)
		if observation.SourceKind == "" || observation.Target == "" {
			continue
		}
		out = append(out, observation)
	}
	return out
}

func fallbackFindings(
	units []tool.EvidenceUnit,
	observations []agentapi.EvidenceObservation,
) []any {
	findings := make([]any, 0, minInt(len(units)+len(observations), fallbackMaxFindings))
	seenUnits := make(map[string]struct{}, len(units))
	for _, unit := range units {
		if len(findings) >= fallbackMaxFindings {
			break
		}
		key, ok := evidence.UnitKey(unit)
		if !ok {
			continue
		}
		identity := key.String()
		if _, duplicate := seenUnits[identity]; duplicate {
			continue
		}
		seenUnits[identity] = struct{}{}
		findings = append(findings, fallbackFindingFromUnit(unit))
	}
	seenObservations := make(map[string]struct{}, len(observations))
	for _, observation := range observations {
		if len(findings) >= fallbackMaxFindings {
			break
		}
		identity := fallbackObservationKey(observation)
		if identity == "" {
			continue
		}
		if _, duplicate := seenObservations[identity]; duplicate {
			continue
		}
		seenObservations[identity] = struct{}{}
		findings = append(findings, fallbackFindingFromObservation(observation))
	}
	return findings
}

func fallbackObservationKey(observation agentapi.EvidenceObservation) string {
	return strings.Join([]string{
		observation.SourceKind, observation.Target, observation.Section,
		observation.Version, observation.TimeRange,
	}, "\x00")
}

func fallbackFindingFromUnit(unit tool.EvidenceUnit) map[string]any {
	kind := strings.TrimSpace(unit.SourceKind)
	reference := strings.TrimSpace(unit.Target)
	summary := fallbackUnitSummary(unit)
	goalIDs := fallbackUnitGoals(unit)
	return fallbackFinding(kind, reference, summary, goalIDs)
}

func fallbackFindingFromObservation(observation agentapi.EvidenceObservation) map[string]any {
	kind := strings.TrimSpace(observation.SourceKind)
	reference := strings.TrimSpace(observation.Target)
	summary := truncateForEvidence(strings.TrimSpace(observation.Summary), 2000)
	if summary == "" {
		summary = "Collected evidence for " + reference
	}
	goalIDs := fallbackObservationGoals(observation)
	return fallbackFinding(kind, reference, summary, goalIDs)
}

func fallbackFinding(kind, reference, summary string, goalIDs []string) map[string]any {
	if goalIDs == nil {
		goalIDs = []string{}
	}
	return map[string]any{
		"claim":             truncateForEvidence(summary, 4000),
		"evidence_goal_ids": goalIDs,
		"evidence": []any{
			map[string]any{
				"kind":      truncateForEvidence(kind, 256),
				"reference": truncateForEvidence(reference, 256),
				"summary":   truncateForEvidence(summary, 4000),
			},
		},
		"confidence": 0.5,
	}
}

func fallbackUnitSummary(unit tool.EvidenceUnit) string {
	if section := fallbackSection(unit); section != "" {
		return fmt.Sprintf("Evidence %s/%s: %s", unit.SourceKind, unit.Target, section)
	}
	return fmt.Sprintf("Evidence %s/%s", unit.SourceKind, unit.Target)
}

func fallbackSection(unit tool.EvidenceUnit) string {
	if len(unit.Sections) == 0 {
		return ""
	}
	return strings.TrimSpace(unit.Sections[0])
}

func fallbackUnitGoals(unit tool.EvidenceUnit) []string {
	if len(unit.Facets) > 0 {
		return dedupeStrings(unit.Facets)
	}
	return facetStrings(domain.ProvidedFacetsFor(unit.SourceKind, fallbackUnitKind(unit)))
}

func fallbackObservationGoals(observation agentapi.EvidenceObservation) []string {
	if len(observation.Facets) > 0 {
		return dedupeStrings(observation.Facets)
	}
	return facetStrings(domain.ProvidedFacetsFor(observation.SourceKind, ""))
}

// fallbackUnitKind approximates the DocKind used by ProvidedFacetsFor for
// runbook units. A runbook unit's section carries a `direction:from->to:type`
// shape for flow documents, so we map that back to a flow kind.
func fallbackUnitKind(unit tool.EvidenceUnit) string {
	if unit.SourceKind != "runbook" {
		return ""
	}
	section := fallbackSection(unit)
	if strings.Contains(section, "->") {
		return string(domain.DocKindFlow)
	}
	return ""
}

func fallbackFlow(units []tool.EvidenceUnit) map[string]any {
	edges := make([]map[string]any, 0, fallbackMaxEdges)
	nodeIDs := make(map[string]struct{}, fallbackMaxNodes)
	nodeOrder := make([]string, 0, fallbackMaxNodes)
	addNode := func(id string) {
		id = truncateForEvidence(id, 128)
		if id == "" {
			return
		}
		if _, exists := nodeIDs[id]; exists {
			return
		}
		if len(nodeOrder) >= fallbackMaxNodes {
			return
		}
		nodeIDs[id] = struct{}{}
		nodeOrder = append(nodeOrder, id)
	}

	for _, unit := range units {
		if len(edges) >= fallbackMaxEdges {
			break
		}
		from, to, protocol, ok := fallbackDependencyEdge(unit)
		if !ok {
			continue
		}
		addNode(from)
		addNode(to)
		edges = append(edges, map[string]any{
			"from":           truncateForEvidence(from, 128),
			"to":             truncateForEvidence(to, 128),
			"protocol":       truncateForEvidence(protocol, 128),
			"sync_mode":      "unknown",
			"evidence_state": "inferred",
		})
	}
	if len(nodeOrder) == 0 {
		return nil
	}
	nodes := make([]any, 0, len(nodeOrder))
	for _, id := range nodeOrder {
		nodes = append(nodes, map[string]any{
			"id":    id,
			"label": id,
			"kind":  "service",
		})
	}
	return map[string]any{
		"subject":    truncateForEvidence("Evidence-derived flow", 256),
		"status":     "partial",
		"nodes":      nodes,
		"edges":      edges,
		"open_hops":  []any{},
		"confidence": "low",
	}
}

// fallbackDependencyEdge decodes a dependency unit's canonical section
// `direction:from->to:type` (see evidence.DependencyUnit). Only dependency
// units contribute edges; other evidence contributes findings but no flow.
func fallbackDependencyEdge(unit tool.EvidenceUnit) (from, to, protocol string, ok bool) {
	if unit.SourceKind != "dependency" {
		return "", "", "", false
	}
	section := fallbackSection(unit)
	if section == "" {
		return "", "", "", false
	}
	// direction:from->to:type
	colon := strings.Index(section, ":")
	if colon < 0 {
		return "", "", "", false
	}
	body := section[colon+1:]
	arrow := strings.Index(body, "->")
	if arrow < 0 {
		return "", "", "", false
	}
	from = strings.TrimSpace(body[:arrow])
	rest := strings.TrimSpace(body[arrow+2:])
	if from == "" || rest == "" {
		return "", "", "", false
	}
	to = rest
	if typeSep := strings.Index(rest, ":"); typeSep > 0 {
		to = strings.TrimSpace(rest[:typeSep])
		protocol = strings.TrimSpace(rest[typeSep+1:])
	}
	if to == "" {
		return "", "", "", false
	}
	if protocol == "" {
		protocol = "dependency"
	}
	return from, to, protocol, true
}

func partitionFallbackGoals(
	units []tool.EvidenceUnit,
	observations []agentapi.EvidenceObservation,
	required []string,
) (covered, unresolved []string) {
	coveredSet := make(map[string]struct{}, len(required))
	for _, unit := range units {
		for _, goal := range fallbackUnitGoals(unit) {
			coveredSet[goal] = struct{}{}
		}
	}
	for _, observation := range observations {
		for _, goal := range fallbackObservationGoals(observation) {
			coveredSet[goal] = struct{}{}
		}
	}
	covered = make([]string, 0, len(coveredSet))
	for goal := range coveredSet {
		if requiredContains(required, goal) {
			covered = append(covered, goal)
		}
	}
	sort.Strings(covered)
	covered = capFallbackStrings(covered, fallbackMaxGoals)

	unresolvedSet := make(map[string]struct{}, len(required))
	for _, goal := range required {
		goal = strings.TrimSpace(goal)
		if goal == "" {
			continue
		}
		unresolvedSet[goal] = struct{}{}
	}
	for _, goal := range covered {
		delete(unresolvedSet, goal)
	}
	unresolved = make([]string, 0, len(unresolvedSet))
	for goal := range unresolvedSet {
		unresolved = append(unresolved, goal)
	}
	sort.Strings(unresolved)
	unresolved = capFallbackStrings(unresolved, fallbackMaxGoals)
	return covered, unresolved
}

func requiredContains(required []string, goal string) bool {
	for _, candidate := range required {
		if strings.TrimSpace(candidate) == goal {
			return true
		}
	}
	return false
}

func capFallbackStrings(values []string, max int) []string {
	if len(values) <= max {
		return values
	}
	return values[:max]
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func facetStrings(facets []domain.EvidenceFacet) []string {
	out := make([]string, len(facets))
	for index, facet := range facets {
		out[index] = string(facet)
	}
	return out
}

func truncateForEvidence(value string, max int) string {
	if max <= 0 {
		return value
	}
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}
