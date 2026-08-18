package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

// investigatorProjectionStatus describes how much of a shared task contract
// was admitted to one investigator. It is intentionally internal: the
// original handoff remains the durable source of truth.
type investigatorProjectionStatus string

const projectionVersion = "v1"

const (
	projectionMatched      investigatorProjectionStatus = "matched"
	projectionEmpty        investigatorProjectionStatus = "empty"
	projectionInsufficient investigatorProjectionStatus = "insufficient"
	projectionPartial      investigatorProjectionStatus = "partial"
	projectionLegacy       investigatorProjectionStatus = "legacy"
)

type projectionResult struct {
	Input              Handoff
	Status             investigatorProjectionStatus
	InputTokens        int
	ProjectedTokens    int
	DroppedSeedCount   int
	MatchedSeedCount   int
	DuplicateSeedCount int
	MissingEntities    []string
	MissingFacets      []string
	ProjectionHash     string
}

type contractProjection struct {
	Entities      []projectedEntityRef    `json:"entities"`
	EvidenceGoals []projectedEvidenceGoal `json:"evidence_goals"`
	Context       projectedTaskContext    `json:"context"`
}

type projectedEntityRef struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Role    string   `json:"role,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

type projectedEvidenceGoal struct {
	ID              string                    `json:"id"`
	Facet           string                    `json:"facet"`
	Required        bool                      `json:"required"`
	Sources         []agentapi.EvidenceSource `json:"sources"`
	RequiredSources []agentapi.EvidenceSource `json:"required_sources,omitempty"`
	Freshness       agentapi.FreshnessPolicy  `json:"freshness"`
	MinimumCoverage int                       `json:"minimum_coverage"`
	HighRisk        bool                      `json:"high_risk,omitempty"`
}

type projectedTaskContext struct {
	ConversationRefs []json.RawMessage       `json:"conversation_refs,omitempty"`
	TimeRange        json.RawMessage         `json:"time_range,omitempty"`
	SeedMaterial     []agentapi.ContextBlock `json:"seed_material,omitempty"`
}

type projectedSeedSummary struct {
	Projection    string   `json:"projection"`
	Source        string   `json:"source"`
	Facets        []string `json:"facets,omitempty"`
	EvidenceCount int      `json:"evidence_count"`
	References    []string `json:"references,omitempty"`
	ContentHashes []string `json:"content_hashes,omitempty"`
}

// projectInvestigatorHandoff narrows a task.contract handoff to the facets,
// sources, and explicit evidence references assigned to the current node.
// The input handoff is never mutated; a projected clone is returned instead.
func projectInvestigatorHandoff(
	input Handoff,
	task *TaskDirective,
	nodeID string,
	maxTokens int64,
) (projectionResult, error) {
	result := projectionResult{
		Input:       cloneProjectedHandoff(input),
		Status:      projectionLegacy,
		InputTokens: estimateProjectionTokens(input.Payload),
	}
	if task == nil || len(task.RequiredFacets) == 0 || input.Schema.ID != "task.contract" {
		result.ProjectedTokens = result.InputTokens
		result.ProjectionHash = input.ContentHash
		return result, nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input.Payload, &raw); err != nil {
		return projectionResult{}, fmt.Errorf("decode task contract for node %q: %w", nodeID, err)
	}
	if raw == nil {
		return projectionResult{}, fmt.Errorf("task contract for node %q is not an object", nodeID)
	}
	// A task.contract without task_id is the older planner-facing shape. Do
	// not rewrite it: the node remains compatible with the pre-projection
	// execution path.
	if taskID, ok := raw["task_id"]; !ok || strings.TrimSpace(string(taskID)) == "null" || strings.TrimSpace(string(taskID)) == `""` {
		result.ProjectedTokens = result.InputTokens
		result.ProjectionHash = input.ContentHash
		return result, nil
	}
	var contract contractProjection
	if err := decodeProjectionFields(raw, &contract); err != nil {
		return projectionResult{}, fmt.Errorf("decode task contract context for node %q: %w", nodeID, err)
	}

	required := stringSet(task.RequiredFacets)
	filteredGoals := make([]projectedEvidenceGoal, 0, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		if _, ok := required[goal.Facet]; ok {
			filteredGoals = append(filteredGoals, goal)
		}
	}
	contract.EvidenceGoals = filteredGoals

	allowedSources := investigatorSourceKinds(nodeID)
	refs := append([]agentapi.EvidenceRef(nil), task.InputRefs...)
	selectedBlocks := make([]agentapi.ContextBlock, 0, len(contract.Context.SeedMaterial))
	selectedHandoffUnits := make([]tool.EvidenceUnit, 0, len(input.EvidenceUnits))
	selectedKeys := make(map[string]struct{})
	seenKeys := make(map[string]struct{})
	matchedSeeds := 0
	droppedSeeds := 0
	duplicateSeeds := 0

	for _, block := range contract.Context.SeedMaterial {
		selectedUnits := make([]tool.EvidenceUnit, 0, len(block.Evidence))
		for _, unit := range block.Evidence {
			key := projectionEvidenceKey(unit)
			if _, duplicate := seenKeys[key]; duplicate {
				duplicateSeeds++
				continue
			}
			if !projectionEvidenceMatches(unit, required, allowedSources, refs) {
				continue
			}
			seenKeys[key] = struct{}{}
			selectedKeys[key] = struct{}{}
			selectedUnits = append(selectedUnits, cloneEvidenceUnit(unit))
		}
		if len(selectedUnits) == 0 {
			droppedSeeds++
			continue
		}
		matchedSeeds++
		projected := projectContextBlock(block, selectedUnits)
		selectedBlocks = append(selectedBlocks, projected)
		selectedHandoffUnits = append(selectedHandoffUnits, selectedUnits...)
	}

	// Some workflows carry the evidence ledger on the Handoff as well as in
	// seed_material. Keep the same narrow view for the runtime policy and for
	// downstream provenance, without exposing an unrelated unit through the
	// projected payload.
	for _, unit := range input.EvidenceUnits {
		key := projectionEvidenceKey(unit)
		if _, duplicate := selectedKeys[key]; duplicate {
			continue
		}
		if _, duplicate := seenKeys[key]; duplicate {
			duplicateSeeds++
			continue
		}
		if !projectionEvidenceMatches(unit, required, allowedSources, refs) {
			continue
		}
		seenKeys[key] = struct{}{}
		selectedKeys[key] = struct{}{}
		selectedHandoffUnits = append(selectedHandoffUnits, cloneEvidenceUnit(unit))
	}

	contract.Context.SeedMaterial = selectedBlocks
	if err := encodeProjectionFields(raw, contract); err != nil {
		return projectionResult{}, fmt.Errorf("encode projected task contract for node %q: %w", nodeID, err)
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return projectionResult{}, fmt.Errorf("marshal projected task contract for node %q: %w", nodeID, err)
	}

	result.Input.Payload = payload
	result.Input.EvidenceUnits = selectedHandoffUnits
	result.Input.References = filterProjectionReferences(input.References, selectedHandoffUnits)
	result.Input.EvidenceConflicts = filterProjectionConflicts(input.EvidenceConflicts, selectedHandoffUnits)
	if result.Input.ContentHash, err = handoffHash(result.Input); err != nil {
		return projectionResult{}, fmt.Errorf("hash projected handoff for node %q: %w", nodeID, err)
	}
	result.InputTokens = estimateProjectionTokens(input.Payload)
	result.ProjectedTokens = estimateProjectionTokens(payload)
	result.DroppedSeedCount = droppedSeeds
	result.MatchedSeedCount = matchedSeeds
	result.DuplicateSeedCount = duplicateSeeds
	result.MissingFacets = missingProjectionFacets(task.RequiredFacets, selectedHandoffUnits)
	result.MissingEntities = missingProjectionEntities(
		contract.Entities, selectedHandoffUnits, projectionMinimumCoverage(contract.EvidenceGoals),
	)
	switch {
	case matchedSeeds == 0:
		result.Status = projectionEmpty
	case len(result.MissingFacets) > 0 || len(result.MissingEntities) > 0:
		result.Status = projectionInsufficient
	case droppedSeeds > 0 || duplicateSeeds > 0:
		result.Status = projectionPartial
	default:
		result.Status = projectionMatched
	}
	result.ProjectionHash = projectionHash(input, task, nodeID, payload)
	if maxTokens > 0 && int64(result.ProjectedTokens) > maxTokens {
		return projectionResult{}, fmt.Errorf(
			"projected task contract for node %q exceeds input budget: %d tokens > %d",
			nodeID, result.ProjectedTokens, maxTokens,
		)
	}
	return result, nil
}

func projectionHash(input Handoff, task *TaskDirective, nodeID string, payload []byte) string {
	type identity struct {
		Version   string         `json:"version"`
		NodeID    string         `json:"node_id"`
		InputHash string         `json:"input_hash"`
		Task      *TaskDirective `json:"task,omitempty"`
		Payload   string         `json:"payload_hash"`
	}
	payloadSum := sha256.Sum256(payload)
	value, _ := json.Marshal(identity{
		Version: projectionVersion, NodeID: nodeID, InputHash: input.ContentHash,
		Task: task, Payload: hex.EncodeToString(payloadSum[:]),
	})
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func decodeProjectionFields(raw map[string]json.RawMessage, contract *contractProjection) error {
	if value, ok := raw["entities"]; ok {
		if err := json.Unmarshal(value, &contract.Entities); err != nil {
			return fmt.Errorf("entities: %w", err)
		}
	}
	if value, ok := raw["evidence_goals"]; ok {
		if err := json.Unmarshal(value, &contract.EvidenceGoals); err != nil {
			return fmt.Errorf("evidence_goals: %w", err)
		}
	}
	if value, ok := raw["context"]; ok {
		if err := json.Unmarshal(value, &contract.Context); err != nil {
			return fmt.Errorf("context: %w", err)
		}
	}
	return nil
}

func missingProjectionFacets(required []string, units []tool.EvidenceUnit) []string {
	covered := make(map[string]struct{}, len(required))
	for _, unit := range units {
		for _, facet := range unit.Facets {
			covered[facet] = struct{}{}
		}
	}
	missing := make([]string, 0, len(required))
	for _, facet := range required {
		if _, ok := covered[facet]; !ok {
			missing = append(missing, facet)
		}
	}
	return missing
}

func projectionMinimumCoverage(goals []projectedEvidenceGoal) int {
	minimum := 1
	for _, goal := range goals {
		if goal.Required && goal.MinimumCoverage > minimum {
			minimum = goal.MinimumCoverage
		}
	}
	return minimum
}

func missingProjectionEntities(
	entities []projectedEntityRef,
	units []tool.EvidenceUnit,
	minimumCoverage int,
) []string {
	if minimumCoverage <= 1 || len(entities) == 0 {
		return nil
	}
	matched := 0
	missing := make([]string, 0, len(entities))
	for _, entity := range entities {
		if projectionEntityMatches(entity, units) {
			matched++
			continue
		}
		missing = append(missing, entity.ID)
	}
	if matched >= minimumCoverage {
		return nil
	}
	return missing
}

func projectionEntityMatches(entity projectedEntityRef, units []tool.EvidenceUnit) bool {
	terms := make([]string, 0, len(entity.Aliases)+2)
	terms = appendProjectionEntityTerm(terms, entity.ID)
	terms = appendProjectionEntityTerm(terms, entity.Label)
	for _, alias := range entity.Aliases {
		terms = appendProjectionEntityTerm(terms, alias)
	}
	for _, unit := range units {
		haystack := canonicalProjectionEntityText(strings.Join(append([]string{unit.Target}, unit.Sections...), " "))
		for _, term := range terms {
			if strings.Contains(haystack, term) {
				return true
			}
		}
	}
	return false
}

func appendProjectionEntityTerm(values []string, value string) []string {
	value = canonicalProjectionEntityText(value)
	if len([]rune(value)) < 3 || contains(values, value) {
		return values
	}
	return append(values, value)
}

func canonicalProjectionEntityText(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func encodeProjectionFields(raw map[string]json.RawMessage, contract contractProjection) error {
	goals, err := json.Marshal(contract.EvidenceGoals)
	if err != nil {
		return fmt.Errorf("evidence_goals: %w", err)
	}
	contextValue, err := json.Marshal(contract.Context)
	if err != nil {
		return fmt.Errorf("context: %w", err)
	}
	raw["evidence_goals"] = goals
	raw["context"] = contextValue
	return nil
}

func projectContextBlock(block agentapi.ContextBlock, units []tool.EvidenceUnit) agentapi.ContextBlock {
	facets := make(map[string]struct{})
	hashes := make([]string, 0, len(units))
	for _, unit := range units {
		for _, facet := range unit.Facets {
			facets[facet] = struct{}{}
		}
		if unit.ContentHash != "" {
			hashes = appendUniqueString(hashes, unit.ContentHash)
		}
	}
	facetList := make([]string, 0, len(facets))
	for facet := range facets {
		facetList = append(facetList, facet)
	}
	sort.Strings(facetList)
	sort.Strings(hashes)
	references := make([]agentapi.Reference, 0, len(block.References))
	for _, reference := range block.References {
		for _, unit := range units {
			if reference.Target == unit.Target {
				references = append(references, reference)
				break
			}
		}
	}
	referenceTargets := make([]string, 0, len(references))
	for _, reference := range references {
		referenceTargets = appendUniqueString(referenceTargets, reference.Target)
	}
	summary := projectedSeedSummary{
		Projection:    "scoped_metadata",
		Source:        block.Source,
		Facets:        facetList,
		EvidenceCount: len(units),
		References:    referenceTargets,
		ContentHashes: hashes,
	}
	contentBytes, _ := json.Marshal(summary)
	sum := sha256.Sum256(contentBytes)
	return agentapi.ContextBlock{
		Source:            block.Source,
		Title:             "Scoped evidence seed",
		Content:           string(contentBytes),
		References:        references,
		Evidence:          units,
		EvidenceConflicts: filterProjectionConflicts(block.EvidenceConflicts, units),
		Complete:          false,
		ContentHash:       hex.EncodeToString(sum[:]),
	}
}

func projectionEvidenceMatches(
	unit tool.EvidenceUnit,
	required map[string]struct{},
	allowedSources map[string]struct{},
	refs []agentapi.EvidenceRef,
) bool {
	if _, ok := allowedSources[strings.ToLower(strings.TrimSpace(unit.SourceKind))]; !ok {
		return false
	}
	facetMatch := false
	for _, facet := range unit.Facets {
		if _, ok := required[facet]; ok {
			facetMatch = true
			break
		}
	}
	if !facetMatch {
		return false
	}
	if len(refs) == 0 {
		return true
	}
	for _, ref := range refs {
		if projectionReferenceMatches(unit, ref) {
			return true
		}
	}
	return false
}

func projectionReferenceMatches(unit tool.EvidenceUnit, ref agentapi.EvidenceRef) bool {
	if ref.SourceKind != "" && ref.SourceKind != unit.SourceKind {
		return false
	}
	if ref.Target != "" && ref.Target != unit.Target {
		return false
	}
	if ref.Version != "" && ref.Version != unit.Version {
		return false
	}
	if ref.TimeRange != "" && ref.TimeRange != unit.TimeRange {
		return false
	}
	if ref.ContentHash != "" && ref.ContentHash != unit.ContentHash {
		return false
	}
	if ref.Section != "" {
		for _, section := range unit.Sections {
			if section == ref.Section {
				return true
			}
		}
		return false
	}
	return true
}

func investigatorSourceKinds(nodeID string) map[string]struct{} {
	var values []string
	switch {
	case strings.Contains(nodeID, ".code"):
		values = []string{"code", "codegraph"}
	case strings.Contains(nodeID, ".runtime"), strings.Contains(nodeID, ".service"):
		values = []string{"service", "dependency", "runtime", "codegraph"}
	case strings.Contains(nodeID, ".docs"):
		values = []string{"runbook", "generated_doc", "doc", "docs"}
	case strings.Contains(nodeID, ".web"):
		values = []string{"web", "external"}
	case strings.Contains(nodeID, ".memory"):
		values = []string{"memory"}
	default:
		return map[string]struct{}{}
	}
	return stringSet(values)
}

func filterProjectionReferences(
	references []agentapi.Reference,
	units []tool.EvidenceUnit,
) []agentapi.Reference {
	if len(references) == 0 || len(units) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(units))
	for _, unit := range units {
		allowed[unit.Target] = struct{}{}
	}
	out := make([]agentapi.Reference, 0, len(references))
	for _, reference := range references {
		if _, ok := allowed[reference.Target]; ok {
			out = append(out, reference)
		}
	}
	return out
}

func filterProjectionConflicts(
	conflicts []agentapi.EvidenceConflict,
	units []tool.EvidenceUnit,
) []agentapi.EvidenceConflict {
	if len(conflicts) == 0 || len(units) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(units))
	for _, unit := range units {
		allowed[projectionEvidenceIdentity(unit)] = struct{}{}
	}
	out := make([]agentapi.EvidenceConflict, 0, len(conflicts))
	for _, conflict := range conflicts {
		if _, ok := allowed[projectionConflictIdentity(conflict)]; ok {
			out = append(out, conflict)
		}
	}
	return out
}

func projectionConflictIdentity(conflict agentapi.EvidenceConflict) string {
	identity := conflict.Identity
	return strings.Join([]string{
		identity.SourceKind, identity.Target, identity.Section,
		identity.Version, identity.TimeRange,
	}, "\x00")
}

func projectionEvidenceIdentity(unit tool.EvidenceUnit) string {
	section := ""
	if len(unit.Sections) > 0 {
		section = unit.Sections[0]
	}
	return strings.Join([]string{
		unit.SourceKind, unit.Target, section, unit.Version, unit.TimeRange,
	}, "\x00")
}

func projectionEvidenceKey(unit tool.EvidenceUnit) string {
	return projectionEvidenceIdentity(unit) + "\x00" + unit.ContentHash
}

func cloneProjectedHandoff(input Handoff) Handoff {
	input.Payload = append(json.RawMessage(nil), input.Payload...)
	input.References = append([]agentapi.Reference(nil), input.References...)
	input.EvidenceUnits = cloneEvidenceUnits(input.EvidenceUnits)
	input.EvidenceConflicts = cloneEvidenceConflicts(input.EvidenceConflicts)
	return input
}

func cloneEvidenceUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	out := make([]tool.EvidenceUnit, 0, len(units))
	for _, unit := range units {
		out = append(out, cloneEvidenceUnit(unit))
	}
	return out
}

func cloneEvidenceUnit(unit tool.EvidenceUnit) tool.EvidenceUnit {
	unit.Sections = append([]string(nil), unit.Sections...)
	unit.Facets = append([]string(nil), unit.Facets...)
	return unit
}

func cloneEvidenceConflicts(conflicts []agentapi.EvidenceConflict) []agentapi.EvidenceConflict {
	out := make([]agentapi.EvidenceConflict, len(conflicts))
	copy(out, conflicts)
	for index := range out {
		out[index].Current = cloneEvidenceUnit(out[index].Current)
		out[index].Incoming = cloneEvidenceUnit(out[index].Incoming)
	}
	return out
}

func estimateProjectionTokens(payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	return (len(payload) + 3) / 4
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			set[value] = struct{}{}
		}
	}
	return set
}
