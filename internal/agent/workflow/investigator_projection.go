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
	projectionBypassed     investigatorProjectionStatus = "bypassed"
)

type projectionResult struct {
	Input              Handoff
	Task               *TaskDirective
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
	Objective               string                        `json:"objective"`
	Entities                []projectedEntityRef          `json:"entities"`
	InvestigationGoals      []projectedInvestigationGoal  `json:"investigation_goals"`
	EvidenceGoals           []projectedEvidenceGoal       `json:"evidence_goals"`
	TaskEvidenceAssignments []projectedEvidenceAssignment `json:"task_evidence_assignments,omitempty"`
	Context                 projectedTaskContext          `json:"context"`
}

type projectedEntityRef struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Role    string   `json:"role,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

type projectedInvestigationGoal struct {
	ID                  string   `json:"id"`
	Objective           string   `json:"objective"`
	IndependentlyUseful bool     `json:"independently_useful"`
	DependsOn           []string `json:"depends_on"`
}

type projectedEvidenceGoal struct {
	ID              string                    `json:"id"`
	Facet           string                    `json:"facet"`
	Facets          []string                  `json:"facets"`
	Required        bool                      `json:"required"`
	Sources         []agentapi.EvidenceSource `json:"sources"`
	RequiredSources []agentapi.EvidenceSource `json:"required_sources,omitempty"`
	Freshness       agentapi.FreshnessPolicy  `json:"freshness"`
	MinimumCoverage int                       `json:"minimum_coverage"`
	HighRisk        bool                      `json:"high_risk,omitempty"`
}

type projectedEvidenceAssignment struct {
	TaskID         string                    `json:"task_id"`
	RequiredFacets []string                  `json:"required_facets,omitempty"`
	InputRefs      []agentapi.EvidenceRef    `json:"input_refs"`
	ContextRefs    []projectedTaskContextRef `json:"context_refs"`
}

type projectedTaskContextRef struct {
	Source      string `json:"source"`
	ContentHash string `json:"content_hash"`
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
		Task:        cloneTaskDirective(task),
		Status:      projectionBypassed,
		InputTokens: estimateProjectionTokens(input.Payload),
	}
	if input.Schema.ID != agentapi.TaskContractSchemaID {
		result.ProjectedTokens = result.InputTokens
		result.ProjectionHash = input.ContentHash
		return result, nil
	}
	if input.Schema != agentapi.TaskContractSchemaRef() {
		return projectionResult{}, fmt.Errorf(
			"task contract schema version %d is unsupported; current version is %d",
			input.Schema.Version, agentapi.TaskContractSchemaVersion,
		)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input.Payload, &raw); err != nil {
		return projectionResult{}, fmt.Errorf("decode task contract for node %q: %w", nodeID, err)
	}
	if raw == nil {
		return projectionResult{}, fmt.Errorf("task contract for node %q is not an object", nodeID)
	}
	// Delegation-shaped task contracts do not contain the canonical investigation
	// context required for server-side projection, so they pass through unchanged.
	if taskID, ok := raw["task_id"]; !ok || strings.TrimSpace(string(taskID)) == "null" || strings.TrimSpace(string(taskID)) == `""` {
		result.ProjectedTokens = result.InputTokens
		result.ProjectionHash = input.ContentHash
		return result, nil
	}
	resolved, bypass, err := resolveProjection(input, task, nodeID, raw)
	if err != nil {
		return projectionResult{}, err
	}
	if bypass {
		result.ProjectedTokens = result.InputTokens
		result.ProjectionHash = input.ContentHash
		result.Task = resolved.effectiveTask
		return result, nil
	}
	result.Task = resolved.effectiveTask

	collector := newProjectionCollector(
		resolved.required,
		investigatorSourceKinds(nodeID),
		cloneEvidenceRefs(resolved.effectiveTask.InputRefs),
		resolved.assignment.ContextRefs,
		resolved.assigned,
		len(resolved.contract.Context.SeedMaterial),
		len(input.EvidenceUnits),
	)
	collector.collectSeedMaterial(resolved.contract.Context.SeedMaterial)
	// Some workflows carry the evidence ledger on the Handoff as well as in
	// seed_material. Keep the same narrow view for the runtime policy and for
	// downstream provenance, without exposing an unrelated unit through the
	// projected payload.
	collector.collectHandoffUnits(input.EvidenceUnits)

	if err := finalizeProjection(input, nodeID, maxTokens, &result, resolved, collector); err != nil {
		return projectionResult{}, err
	}
	return result, nil
}

// projectionCollector narrows seed material and the handoff evidence ledger to
// the current investigator's assigned facets, sources, and input refs. It keeps
// shared deduplication state across both the seed_material blocks and the
// handoff evidence units so neither can expose a unit the other already saw.
type projectionCollector struct {
	required      map[string]struct{}
	allowedSource map[string]struct{}
	refs          []agentapi.EvidenceRef
	contextRefs   []projectedTaskContextRef
	assigned      bool

	selectedBlocks            []agentapi.ContextBlock
	selectedHandoffUnits      []tool.EvidenceUnit
	selectedContextReferences []agentapi.Reference
	selectedKeys              map[string]struct{}
	seenKeys                  map[string]struct{}
	matchedSeeds              int
	droppedSeeds              int
	duplicateSeeds            int
}

func newProjectionCollector(
	required map[string]struct{},
	allowedSource map[string]struct{},
	refs []agentapi.EvidenceRef,
	contextRefs []projectedTaskContextRef,
	assigned bool,
	seedCount int,
	handoffCount int,
) projectionCollector {
	return projectionCollector{
		required:                  required,
		allowedSource:             allowedSource,
		refs:                      refs,
		contextRefs:               contextRefs,
		assigned:                  assigned,
		selectedBlocks:            make([]agentapi.ContextBlock, 0, seedCount),
		selectedHandoffUnits:      make([]tool.EvidenceUnit, 0, handoffCount),
		selectedContextReferences: make([]agentapi.Reference, 0),
		selectedKeys:              make(map[string]struct{}),
		seenKeys:                  make(map[string]struct{}),
	}
}

func (collector *projectionCollector) collectSeedMaterial(blocks []agentapi.ContextBlock) {
	for _, block := range blocks {
		if len(block.Evidence) == 0 {
			if collector.assigned && taskContextReferenceMatches(block, collector.contextRefs) {
				collector.selectedBlocks = append(
					collector.selectedBlocks,
					cloneContextBlock(block),
				)
				collector.selectedContextReferences = append(
					collector.selectedContextReferences,
					block.References...,
				)
				collector.matchedSeeds++
			} else {
				collector.droppedSeeds++
			}
			continue
		}
		selectedUnits := make([]tool.EvidenceUnit, 0, len(block.Evidence))
		for _, unit := range block.Evidence {
			if !collector.admit(unit) {
				continue
			}
			selectedUnits = append(selectedUnits, cloneEvidenceUnit(unit))
		}
		if len(selectedUnits) == 0 {
			collector.droppedSeeds++
			continue
		}
		collector.matchedSeeds++
		projected := projectContextBlock(block, selectedUnits)
		collector.selectedBlocks = append(collector.selectedBlocks, projected)
		collector.selectedHandoffUnits = append(
			collector.selectedHandoffUnits,
			selectedUnits...,
		)
	}
}

func (collector *projectionCollector) collectHandoffUnits(units []tool.EvidenceUnit) {
	for _, unit := range units {
		key := projectionEvidenceKey(unit)
		if _, duplicate := collector.selectedKeys[key]; duplicate {
			continue
		}
		if _, duplicate := collector.seenKeys[key]; duplicate {
			collector.duplicateSeeds++
			continue
		}
		if !collector.matches(unit) {
			continue
		}
		collector.seenKeys[key] = struct{}{}
		collector.selectedKeys[key] = struct{}{}
		collector.selectedHandoffUnits = append(
			collector.selectedHandoffUnits,
			cloneEvidenceUnit(unit),
		)
	}
}

// admit returns true when a seed unit should be kept in its projected block.
func (collector *projectionCollector) admit(unit tool.EvidenceUnit) bool {
	key := projectionEvidenceKey(unit)
	if _, duplicate := collector.seenKeys[key]; duplicate {
		collector.duplicateSeeds++
		return false
	}
	if !collector.matches(unit) {
		return false
	}
	collector.seenKeys[key] = struct{}{}
	collector.selectedKeys[key] = struct{}{}
	return true
}

func (collector *projectionCollector) matches(unit tool.EvidenceUnit) bool {
	return projectionEvidenceMatches(
		unit, collector.required, collector.allowedSource, collector.refs,
	)
}

// resolvedProjection carries the decoded and narrowed contract state before the
// evidence units are collected and re-encoded.
type resolvedProjection struct {
	raw           map[string]json.RawMessage
	contract      contractProjection
	effectiveTask *TaskDirective
	assigned      bool
	assignment    projectedEvidenceAssignment
	required      map[string]struct{}
}

// resolveProjection decodes a task contract, binds it to the current node's
// assignment, and narrows its investigation/evidence goals. A zero return with
// bypass=true means the input should pass through unchanged; the caller must
// fill the projection hash and token estimate from the original handoff.
func resolveProjection(
	input Handoff,
	task *TaskDirective,
	nodeID string,
	raw map[string]json.RawMessage,
) (resolvedProjection, bool, error) {
	var resolved resolvedProjection
	contract := contractProjection{
		InvestigationGoals: []projectedInvestigationGoal{},
	}
	if err := decodeProjectionFields(raw, &contract); err != nil {
		return resolved, false, fmt.Errorf(
			"decode task contract context for node %q: %w",
			nodeID,
			err,
		)
	}
	assignment, assigned, err := resolveProjectionAssignment(raw, contract, nodeID)
	if err != nil {
		return resolved, false, err
	}
	effectiveTask := resolveProjectionTask(task, assignment, assigned, &contract)
	if effectiveTask == nil ||
		!assigned &&
			len(effectiveTask.RequiredFacets) == 0 &&
			len(effectiveTask.InvestigationGoalIDs) == 0 {
		return resolved, true, nil
	}

	required := stringSet(effectiveTask.RequiredFacets)
	if err := narrowProjectionContract(effectiveTask, &contract, nodeID); err != nil {
		return resolved, false, err
	}
	contract.EvidenceGoals = filterProjectionEvidenceGoals(contract.EvidenceGoals, required)

	return resolvedProjection{
		raw:           raw,
		contract:      contract,
		effectiveTask: effectiveTask,
		assigned:      assigned,
		assignment:    assignment,
		required:      required,
	}, false, nil
}

func resolveProjectionAssignment(
	raw map[string]json.RawMessage,
	contract contractProjection,
	nodeID string,
) (projectedEvidenceAssignment, bool, error) {
	_, assignmentsDeclared := raw["task_evidence_assignments"]
	assignment, assigned, err := taskEvidenceAssignmentForNode(
		contract.TaskEvidenceAssignments,
		nodeID,
	)
	if err != nil {
		return assignment, assigned, fmt.Errorf(
			"task contract evidence assignments for node %q: %w",
			nodeID,
			err,
		)
	}
	if assignmentsDeclared && !assigned {
		assignment = projectedEvidenceAssignment{
			TaskID:      nodeID,
			InputRefs:   []agentapi.EvidenceRef{},
			ContextRefs: []projectedTaskContextRef{},
		}
		assigned = true
	}
	return assignment, assigned, nil
}

func resolveProjectionTask(
	task *TaskDirective,
	assignment projectedEvidenceAssignment,
	assigned bool,
	contract *contractProjection,
) *TaskDirective {
	effectiveTask := cloneTaskDirective(task)
	if !assigned {
		return effectiveTask
	}
	if effectiveTask == nil {
		effectiveTask = &TaskDirective{}
	}
	if effectiveTask.Purpose == "" {
		effectiveTask.Purpose = investigatorTaskPurpose(assignment.TaskID)
	}
	requiredFacets := assignment.RequiredFacets
	if len(requiredFacets) == 0 {
		requiredFacets = projectedEvidenceGoalFacets(contract.EvidenceGoals)
	}
	effectiveTask.RequiredFacets = append([]string(nil), requiredFacets...)
	effectiveTask.InputRefs = cloneEvidenceRefs(assignment.InputRefs)
	contract.TaskEvidenceAssignments = []projectedEvidenceAssignment{
		cloneProjectedEvidenceAssignment(assignment),
	}
	return effectiveTask
}

func narrowProjectionContract(
	effectiveTask *TaskDirective,
	contract *contractProjection,
	nodeID string,
) error {
	if len(effectiveTask.InvestigationGoalIDs) == 0 {
		return nil
	}
	bound := stringSet(effectiveTask.InvestigationGoalIDs)
	filtered := make(
		[]projectedInvestigationGoal,
		0,
		len(effectiveTask.InvestigationGoalIDs),
	)
	for _, goal := range contract.InvestigationGoals {
		if _, ok := bound[goal.ID]; ok {
			filtered = append(filtered, goal)
		}
	}
	if len(filtered) != len(bound) {
		return fmt.Errorf(
			"task contract for node %q is missing bound investigation goals",
			nodeID,
		)
	}
	contract.InvestigationGoals = filtered
	if len(filtered) == 1 {
		contract.Objective = filtered[0].Objective
	} else {
		contract.Objective = effectiveTask.Purpose
	}
	return nil
}

func filterProjectionEvidenceGoals(goals []projectedEvidenceGoal, required map[string]struct{}) []projectedEvidenceGoal {
	filteredGoals := make([]projectedEvidenceGoal, 0, len(goals))
	for _, goal := range goals {
		if projectedEvidenceGoalMatches(goal, required) {
			filteredGoals = append(filteredGoals, goal)
		}
	}
	return filteredGoals
}

// finalizeProjection encodes the narrowed contract, rehashes the handoff, and
// fills the remaining projectionResult fields.
func finalizeProjection(
	input Handoff,
	nodeID string,
	maxTokens int64,
	result *projectionResult,
	resolved resolvedProjection,
	collector projectionCollector,
) error {
	resolved.contract.Context.SeedMaterial = collector.selectedBlocks
	if err := encodeProjectionFields(resolved.raw, resolved.contract); err != nil {
		return fmt.Errorf("encode projected task contract for node %q: %w", nodeID, err)
	}
	payload, err := json.Marshal(resolved.raw)
	if err != nil {
		return fmt.Errorf("marshal projected task contract for node %q: %w", nodeID, err)
	}

	result.Input.Payload = payload
	result.Input.EvidenceUnits = collector.selectedHandoffUnits
	result.Input.References = appendUniqueReferences(
		filterProjectionReferences(input.References, collector.selectedHandoffUnits),
		collector.selectedContextReferences,
	)
	result.Input.EvidenceConflicts = filterProjectionConflicts(
		input.EvidenceConflicts, collector.selectedHandoffUnits,
	)
	if result.Input.ContentHash, err = handoffHash(result.Input); err != nil {
		return fmt.Errorf("hash projected handoff for node %q: %w", nodeID, err)
	}
	result.InputTokens = estimateProjectionTokens(input.Payload)
	result.ProjectedTokens = estimateProjectionTokens(payload)
	result.DroppedSeedCount = collector.droppedSeeds
	result.MatchedSeedCount = collector.matchedSeeds
	result.DuplicateSeedCount = collector.duplicateSeeds
	result.MissingFacets = missingProjectionFacets(
		resolved.effectiveTask.RequiredFacets,
		collector.selectedHandoffUnits,
	)
	result.MissingEntities = missingProjectionEntities(
		resolved.contract.Entities,
		collector.selectedHandoffUnits,
		projectionMinimumCoverage(resolved.contract.EvidenceGoals),
	)
	switch {
	case collector.matchedSeeds == 0:
		result.Status = projectionEmpty
	case len(result.MissingFacets) > 0 || len(result.MissingEntities) > 0:
		result.Status = projectionInsufficient
	case collector.droppedSeeds > 0 || collector.duplicateSeeds > 0:
		result.Status = projectionPartial
	default:
		result.Status = projectionMatched
	}
	result.ProjectionHash = projectionHash(
		input, resolved.effectiveTask, nodeID, payload,
	)
	if maxTokens > 0 && int64(result.ProjectedTokens) > maxTokens {
		return fmt.Errorf(
			"projected task contract for node %q exceeds input budget: %d tokens > %d",
			nodeID, result.ProjectedTokens, maxTokens,
		)
	}
	return nil
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
	if value, ok := raw["objective"]; ok {
		if err := json.Unmarshal(value, &contract.Objective); err != nil {
			return fmt.Errorf("objective: %w", err)
		}
	}
	if value, ok := raw["entities"]; ok {
		if err := json.Unmarshal(value, &contract.Entities); err != nil {
			return fmt.Errorf("entities: %w", err)
		}
	}
	if value, ok := raw["investigation_goals"]; ok {
		if err := json.Unmarshal(value, &contract.InvestigationGoals); err != nil {
			return fmt.Errorf("investigation_goals: %w", err)
		}
	}
	if value, ok := raw["evidence_goals"]; ok {
		if err := json.Unmarshal(value, &contract.EvidenceGoals); err != nil {
			return fmt.Errorf("evidence_goals: %w", err)
		}
	}
	if value, ok := raw["task_evidence_assignments"]; ok {
		if err := json.Unmarshal(
			value,
			&contract.TaskEvidenceAssignments,
		); err != nil {
			return fmt.Errorf("task_evidence_assignments: %w", err)
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
	objective, err := json.Marshal(contract.Objective)
	if err != nil {
		return fmt.Errorf("objective: %w", err)
	}
	investigationGoals, err := json.Marshal(contract.InvestigationGoals)
	if err != nil {
		return fmt.Errorf("investigation_goals: %w", err)
	}
	goals, err := json.Marshal(contract.EvidenceGoals)
	if err != nil {
		return fmt.Errorf("evidence_goals: %w", err)
	}
	assignments, err := json.Marshal(contract.TaskEvidenceAssignments)
	if err != nil {
		return fmt.Errorf("task_evidence_assignments: %w", err)
	}
	contextValue, err := json.Marshal(contract.Context)
	if err != nil {
		return fmt.Errorf("context: %w", err)
	}
	raw["objective"] = objective
	raw["investigation_goals"] = investigationGoals
	raw["evidence_goals"] = goals
	if len(contract.TaskEvidenceAssignments) == 0 {
		delete(raw, "task_evidence_assignments")
	} else {
		raw["task_evidence_assignments"] = assignments
	}
	raw["context"] = contextValue
	return nil
}

func taskEvidenceAssignmentForNode(
	assignments []projectedEvidenceAssignment,
	nodeID string,
) (projectedEvidenceAssignment, bool, error) {
	var matched projectedEvidenceAssignment
	found := false
	seen := make(map[string]struct{}, len(assignments))
	for _, assignment := range assignments {
		if _, duplicate := seen[assignment.TaskID]; duplicate {
			return projectedEvidenceAssignment{}, false, fmt.Errorf(
				"task %q is assigned more than once",
				assignment.TaskID,
			)
		}
		seen[assignment.TaskID] = struct{}{}
		if assignment.TaskID != nodeID {
			continue
		}
		matched = cloneProjectedEvidenceAssignment(assignment)
		found = true
	}
	return matched, found, nil
}

func projectedEvidenceGoalFacets(
	goals []projectedEvidenceGoal,
) []string {
	facets := make([]string, 0, len(goals))
	seen := make(map[string]struct{}, len(goals))
	for _, goal := range goals {
		for _, facet := range goal.Facets {
			if _, duplicate := seen[facet]; duplicate {
				continue
			}
			seen[facet] = struct{}{}
			facets = append(facets, facet)
		}
	}
	return facets
}

func projectedEvidenceGoalMatches(goal projectedEvidenceGoal, required map[string]struct{}) bool {
	if _, ok := required[goal.Facet]; ok {
		return true
	}
	for _, facet := range goal.Facets {
		if _, ok := required[facet]; ok {
			return true
		}
	}
	return false
}

func investigatorTaskPurpose(nodeID string) string {
	switch {
	case strings.Contains(nodeID, ".code"):
		return "Inspect the assigned implementation evidence."
	case strings.Contains(nodeID, ".runtime"),
		strings.Contains(nodeID, ".service"):
		return "Trace the assigned runtime and service evidence."
	case strings.Contains(nodeID, ".docs"):
		return "Verify the assigned documentation evidence."
	case strings.Contains(nodeID, ".web"):
		return "Research the assigned public evidence."
	case strings.Contains(nodeID, ".memory"):
		return "Evaluate the assigned recalled evidence."
	default:
		return "Investigate the assigned evidence."
	}
}

func cloneProjectedEvidenceAssignment(
	assignment projectedEvidenceAssignment,
) projectedEvidenceAssignment {
	assignment.RequiredFacets = append(
		[]string(nil),
		assignment.RequiredFacets...,
	)
	assignment.InputRefs = cloneEvidenceRefs(assignment.InputRefs)
	assignment.ContextRefs = append(
		[]projectedTaskContextRef(nil),
		assignment.ContextRefs...,
	)
	return assignment
}

func cloneTaskDirective(task *TaskDirective) *TaskDirective {
	if task == nil {
		return nil
	}
	cloned := *task
	cloned.InvestigationGoalIDs = append(
		[]string(nil),
		task.InvestigationGoalIDs...,
	)
	cloned.EvidenceGoalIDs = append([]string(nil), task.EvidenceGoalIDs...)
	cloned.RequiredFacets = append([]string(nil), task.RequiredFacets...)
	cloned.InputRefs = cloneEvidenceRefs(task.InputRefs)
	return &cloned
}

func taskContextReferenceMatches(
	block agentapi.ContextBlock,
	refs []projectedTaskContextRef,
) bool {
	for _, ref := range refs {
		if ref.Source == block.Source && ref.ContentHash == block.ContentHash {
			return true
		}
	}
	return false
}

func cloneContextBlock(block agentapi.ContextBlock) agentapi.ContextBlock {
	block.References = append([]agentapi.Reference(nil), block.References...)
	block.Evidence = cloneEvidenceUnits(block.Evidence)
	block.EvidenceConflicts = cloneEvidenceConflicts(block.EvidenceConflicts)
	return block
}

func appendUniqueReferences(
	values []agentapi.Reference,
	additions []agentapi.Reference,
) []agentapi.Reference {
	seen := make(map[agentapi.Reference]struct{}, len(values)+len(additions))
	out := make([]agentapi.Reference, 0, len(values)+len(additions))
	for _, reference := range values {
		if _, duplicate := seen[reference]; duplicate {
			continue
		}
		seen[reference] = struct{}{}
		out = append(out, reference)
	}
	for _, reference := range additions {
		if _, duplicate := seen[reference]; duplicate {
			continue
		}
		seen[reference] = struct{}{}
		out = append(out, reference)
	}
	return out
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
	if refs == nil {
		return true
	}
	if len(refs) == 0 {
		return false
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
