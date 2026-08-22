package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

type evidencePayload struct {
	ProducerNodeID string       `json:"producer_node_id"`
	Completeness   Completeness `json:"completeness"`
	ReferenceCount int          `json:"reference_count"`
	EvidenceCount  int          `json:"evidence_count"`
	ConflictCount  int          `json:"conflict_count"`
}

type synthesisObjectiveView struct {
	Objective          string              `json:"objective"`
	InvestigationGoals []synthesisGoalView `json:"investigation_goals"`
}

type synthesisGoalView struct {
	ID                  string   `json:"id"`
	Objective           string   `json:"objective"`
	IndependentlyUseful bool     `json:"independently_useful"`
	DependsOn           []string `json:"depends_on"`
}

type handoffView struct {
	ProducerNodeID string             `json:"producer_node_id"`
	Schema         agentapi.SchemaRef `json:"schema"`
	Payload        json.RawMessage    `json:"payload"`
	Completeness   Completeness       `json:"completeness"`
}

type unavailableTaskView struct {
	ProducerNodeID string     `json:"producer_node_id"`
	StopReason     StopReason `json:"stop_reason,omitempty"`
}

type convergenceView struct {
	CandidateCount    int     `json:"candidate_count"`
	NewIdentityCount  int     `json:"new_identity_count"`
	DuplicateCount    int     `json:"duplicate_count"`
	DuplicateRatio    float64 `json:"duplicate_ratio"`
	MaxDuplicateRatio float64 `json:"max_duplicate_ratio,omitempty"`
}

type ledgerView struct {
	Handoffs                   []handoffView               `json:"handoffs"`
	UnavailableTasks           []unavailableTaskView       `json:"unavailable_tasks"`
	EvidenceUnits              []tool.EvidenceUnit         `json:"evidence_units"`
	BaselineEvidenceIdentities []agentapi.EvidenceIdentity `json:"baseline_evidence_identities,omitempty"`
	EvidenceUnitsTotal         int                         `json:"evidence_units_total,omitempty"`
	EvidenceUnitsOmitted       int                         `json:"evidence_units_omitted,omitempty"`
	EvidenceConflicts          []agentapi.EvidenceConflict `json:"evidence_conflicts"`
	Convergence                *convergenceView            `json:"convergence,omitempty"`
	Completeness               Completeness                `json:"completeness"`
}

func joinedPayload(
	mode JoinMode,
	inputs []Handoff,
	unavailableTasks []unavailableTaskView,
	evidenceUnits []tool.EvidenceUnit,
	baselineEvidenceIdentities []agentapi.EvidenceIdentity,
	evidenceConflicts []agentapi.EvidenceConflict,
	evidenceUnitsTotal int,
	evidenceUnitsOmitted int,
	convergence *convergenceView,
	completeness Completeness,
) (json.RawMessage, error) {
	var value any
	switch mode {
	case JoinPayloadList:
		payloads := make([]json.RawMessage, 0, len(inputs))
		for _, input := range inputs {
			payloads = append(payloads, append(json.RawMessage(nil), input.Payload...))
		}
		value = payloads
	case JoinEvidenceView:
		handoffs := make([]handoffView, 0, len(inputs))
		for _, input := range inputs {
			handoffs = append(handoffs, handoffView{
				ProducerNodeID: input.ProducerNodeID,
				Schema:         input.Schema,
				Payload:        append(json.RawMessage(nil), input.Payload...),
				Completeness:   input.Completeness,
			})
		}
		if evidenceUnits == nil {
			evidenceUnits = []tool.EvidenceUnit{}
		}
		if evidenceConflicts == nil {
			evidenceConflicts = []agentapi.EvidenceConflict{}
		}
		if unavailableTasks == nil {
			unavailableTasks = []unavailableTaskView{}
		}
		value = ledgerView{
			Handoffs: handoffs, UnavailableTasks: unavailableTasks,
			EvidenceUnits:              evidenceUnits,
			BaselineEvidenceIdentities: baselineEvidenceIdentities,
			EvidenceUnitsTotal:         evidenceUnitsTotal,
			EvidenceUnitsOmitted:       evidenceUnitsOmitted,
			EvidenceConflicts:          evidenceConflicts, Convergence: convergence,
			Completeness: completeness,
		}
	default:
		return nil, fmt.Errorf("join mode %q is invalid", mode)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal joined handoffs: %w", err)
	}
	return payload, nil
}

// retainReportedEvidence removes tool results that no report finding cites.
func retainReportedEvidence(
	inputs []Handoff,
	units []tool.EvidenceUnit,
) ([]tool.EvidenceUnit, int, error) {
	_, keys, err := reportedEvidenceKeys(inputs)
	if err != nil {
		return nil, 0, err
	}
	if len(keys) == 0 || len(keys) >= len(units) {
		return units, 0, nil
	}
	retained := filterEvidenceByKeys(units, keys)
	return retained, len(units) - len(retained), nil
}

// compactInvestigationEvidenceToBudget keeps protected baseline evidence and
// fills the remaining handoff byte budget with fairly ordered discoveries.
func compactInvestigationEvidenceToBudget(
	inputs []Handoff,
	units []tool.EvidenceUnit,
	protected map[evidence.Key]struct{},
	maxBytes int64,
	payloadFor func([]tool.EvidenceUnit, int, int) (json.RawMessage, error),
) ([]tool.EvidenceUnit, int, int, error) {
	if maxBytes <= 0 || len(units) == 0 {
		return units, len(units), 0, nil
	}
	fullPayload, err := payloadFor(units, 0, 0)
	if err != nil {
		return nil, 0, 0, err
	}
	if int64(len(fullPayload)) <= maxBytes {
		return units, len(units), 0, nil
	}

	protectedUnits := filterEvidenceByKeys(units, protected)
	protectedPayload, err := payloadFor(
		protectedUnits,
		len(units),
		len(units)-len(protectedUnits),
	)
	if err != nil {
		return nil, 0, 0, err
	}
	if int64(len(protectedPayload)) > maxBytes {
		return nil, 0, 0, fmt.Errorf(
			"protected baseline evidence requires %d bytes, budget is %d",
			len(protectedPayload),
			maxBytes,
		)
	}

	ordered, err := orderedInvestigationEvidenceKeys(inputs, units, protected)
	if err != nil {
		return nil, 0, 0, err
	}
	low, high := 0, len(ordered)
	best := protectedUnits
	for low <= high {
		middle := low + (high-low)/2
		selected := cloneEvidenceKeySet(protected)
		for _, key := range ordered[:middle] {
			selected[key] = struct{}{}
		}
		candidate := filterEvidenceByKeys(units, selected)
		payload, err := payloadFor(
			candidate,
			len(units),
			len(units)-len(candidate),
		)
		if err != nil {
			return nil, 0, 0, err
		}
		if int64(len(payload)) <= maxBytes {
			best = candidate
			low = middle + 1
			continue
		}
		high = middle - 1
	}
	return best, len(units), len(units) - len(best), nil
}

func orderedInvestigationEvidenceKeys(
	inputs []Handoff,
	units []tool.EvidenceUnit,
	protected map[evidence.Key]struct{},
) ([]evidence.Key, error) {
	byProducer, cited, err := reportedEvidenceKeys(inputs)
	if err != nil {
		return nil, err
	}
	ordered := make([]evidence.Key, 0, len(units))
	seen := cloneEvidenceKeySet(protected)
	appendKeys := func(groups [][]evidence.Key) {
		for offset := 0; ; offset++ {
			progressed := false
			for _, keys := range groups {
				if offset >= len(keys) {
					continue
				}
				key := keys[offset]
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				ordered = append(ordered, key)
				progressed = true
			}
			if !progressed {
				return
			}
		}
	}
	if len(cited) > 0 {
		appendKeys(byProducer)
	}
	appendKeys(handoffEvidenceKeys(inputs))
	return ordered, nil
}

func reportedEvidenceKeys(
	inputs []Handoff,
) ([][]evidence.Key, map[evidence.Key]struct{}, error) {
	byProducer := make([][]evidence.Key, len(inputs))
	all := make(map[evidence.Key]struct{})
	for producer, input := range inputs {
		if input.Schema.ID != "investigation.report" {
			continue
		}
		var report reportView
		if err := json.Unmarshal(input.Payload, &report); err != nil {
			return nil, nil, fmt.Errorf("decode investigation report from %q: %w", input.ProducerNodeID, err)
		}
		index := newEvidenceIndex(input.EvidenceUnits)
		seen := make(map[evidence.Key]struct{})
		for _, finding := range report.Findings {
			for _, item := range finding.Evidence {
				for _, match := range index.match(item) {
					key := keyFromIdentity(match.identity)
					if _, duplicate := seen[key]; duplicate {
						continue
					}
					seen[key] = struct{}{}
					all[key] = struct{}{}
					byProducer[producer] = append(byProducer[producer], key)
				}
			}
		}
	}
	return byProducer, all, nil
}

func handoffEvidenceKeys(inputs []Handoff) [][]evidence.Key {
	byProducer := make([][]evidence.Key, len(inputs))
	for producer, input := range inputs {
		seen := make(map[evidence.Key]struct{}, len(input.EvidenceUnits))
		for _, unit := range evidence.Expand(input.EvidenceUnits) {
			key, ok := evidence.UnitKey(unit)
			if !ok {
				continue
			}
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			byProducer[producer] = append(byProducer[producer], key)
		}
	}
	return byProducer
}

func cloneEvidenceKeySet(
	values map[evidence.Key]struct{},
) map[evidence.Key]struct{} {
	cloned := make(map[evidence.Key]struct{}, len(values))
	for key := range values {
		cloned[key] = struct{}{}
	}
	return cloned
}

func filterEvidenceByKeys(
	units []tool.EvidenceUnit,
	keys map[evidence.Key]struct{},
) []tool.EvidenceUnit {
	retained := make([]tool.EvidenceUnit, 0, min(len(units), len(keys)))
	for _, unit := range evidence.Expand(units) {
		key, ok := evidence.UnitKey(unit)
		if !ok {
			continue
		}
		if _, keep := keys[key]; keep {
			retained = append(retained, unit)
		}
	}
	return retained
}

func measureConvergence(
	inputs []Handoff,
	baseline []tool.EvidenceUnit,
	maxDuplicateRatio float64,
) convergenceView {
	baselineKeys := make(map[evidence.Key]struct{})
	for _, unit := range evidence.Expand(baseline) {
		key, ok := evidence.UnitKey(unit)
		if ok {
			baselineKeys[key] = struct{}{}
		}
	}
	seen := make(map[evidence.Key]struct{})
	view := convergenceView{
		MaxDuplicateRatio: maxDuplicateRatio,
	}
	for _, input := range inputs {
		for _, unit := range evidence.Expand(input.EvidenceUnits) {
			key, ok := evidence.UnitKey(unit)
			if !ok {
				continue
			}
			if _, existed := baselineKeys[key]; existed {
				continue
			}
			view.CandidateCount++
			if _, duplicate := seen[key]; duplicate {
				view.DuplicateCount++
				continue
			}
			seen[key] = struct{}{}
			view.NewIdentityCount++
		}
	}
	if view.CandidateCount > 0 {
		view.DuplicateRatio = float64(view.DuplicateCount) /
			float64(view.CandidateCount)
	}
	return view
}

func contextFromHandoff(handoff Handoff) (agentapi.ContextBlock, error) {
	view := evidencePayload{
		ProducerNodeID: handoff.ProducerNodeID,
		Completeness:   handoff.Completeness,
		ReferenceCount: len(handoff.References),
		EvidenceCount:  len(handoff.EvidenceUnits),
		ConflictCount:  len(handoff.EvidenceConflicts),
	}
	content, err := json.Marshal(view)
	if err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf(
			"marshal workflow evidence from %q: %w",
			handoff.ProducerNodeID,
			err,
		)
	}
	sum := sha256.Sum256(content)
	return agentapi.ContextBlock{
		Source:      "workflow.handoff",
		Title:       "Workflow handoff metadata",
		Content:     string(content),
		Complete:    handoff.Completeness == Complete,
		ContentHash: hex.EncodeToString(sum[:]),
	}, nil
}

func contextFromDirective(task TaskDirective) (agentapi.ContextBlock, error) {
	content, err := json.Marshal(task)
	if err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf("marshal workflow task directive: %w", err)
	}
	sum := sha256.Sum256(content)
	return agentapi.ContextBlock{
		Source:      "workflow.task",
		Title:       "Workflow task directive",
		Content:     string(content),
		Complete:    true,
		ContentHash: hex.EncodeToString(sum[:]),
	}, nil
}

func contextFromSynthesisObjective(input Handoff) (agentapi.ContextBlock, error) {
	objective := synthesisObjectiveView{
		InvestigationGoals: []synthesisGoalView{},
	}
	if err := json.Unmarshal(input.Payload, &objective); err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf(
			"decode task contract from %q: %w",
			input.ProducerNodeID,
			err,
		)
	}
	content, err := json.Marshal(objective)
	if err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf(
			"marshal synthesis objective: %w",
			err,
		)
	}
	sum := sha256.Sum256(content)
	return agentapi.ContextBlock{
		Source:      "workflow.synthesis_objective",
		Title:       "Original investigation objective",
		Content:     string(content),
		Complete:    input.Completeness == Complete,
		ContentHash: hex.EncodeToString(sum[:]),
	}, nil
}

func mergeHandoffEvidence(
	inputs []Handoff,
) ([]tool.EvidenceUnit, []agentapi.EvidenceConflict) {
	return mergeEvidenceHandoffs(nil, inputs)
}

func mergeEvidenceHandoffs(
	baseline []tool.EvidenceUnit,
	inputs []Handoff,
) ([]tool.EvidenceUnit, []agentapi.EvidenceConflict) {
	ledger := evidence.New(baseline, "workflow.baseline")
	conflicts := make([]agentapi.EvidenceConflict, 0)
	seenConflicts := make(map[string]struct{})
	for _, input := range inputs {
		conflicts = appendEvidenceConflicts(
			conflicts,
			input.EvidenceConflicts,
			seenConflicts,
		)
		observed := ledger.Add(input.EvidenceUnits, input.ProducerNodeID)
		conflicts = appendEvidenceConflicts(
			conflicts,
			publicConflicts(observed),
			seenConflicts,
		)
	}
	return ledger.Units(), conflicts
}

func canonicalEvidenceIdentities(
	units []tool.EvidenceUnit,
) []agentapi.EvidenceIdentity {
	expanded := evidence.Expand(units)
	identities := make([]agentapi.EvidenceIdentity, 0, len(expanded))
	seen := make(map[evidence.Key]struct{}, len(expanded))
	for _, unit := range expanded {
		key, ok := evidence.UnitKey(unit)
		if !ok {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		identities = append(identities, identityFromKey(key))
	}
	return identities
}

func evidenceIdentityKeySet(
	identities []agentapi.EvidenceIdentity,
) map[evidence.Key]struct{} {
	keys := make(map[evidence.Key]struct{}, len(identities))
	for _, identity := range identities {
		keys[keyFromIdentity(identity)] = struct{}{}
	}
	return keys
}

func appendEvidenceConflicts(
	target []agentapi.EvidenceConflict,
	incoming []agentapi.EvidenceConflict,
	seen map[string]struct{},
) []agentapi.EvidenceConflict {
	for _, conflict := range incoming {
		key := evidenceConflictKey(conflict)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		target = append(target, cloneEvidenceConflict(conflict))
	}
	return target
}

func evidenceConflictKey(conflict agentapi.EvidenceConflict) string {
	identity := conflict.Identity
	return identity.SourceKind + "\x00" +
		identity.Target + "\x00" +
		identity.Section + "\x00" +
		identity.Version + "\x00" +
		identity.TimeRange + "\x00" +
		conflict.Current.ContentHash + "\x00" +
		conflict.Incoming.ContentHash + "\x00" +
		conflict.CurrentOrigin + "\x00" +
		conflict.IncomingOrigin
}

func publicConflicts(
	conflicts []evidence.Conflict,
) []agentapi.EvidenceConflict {
	if len(conflicts) == 0 {
		return nil
	}
	out := make([]agentapi.EvidenceConflict, len(conflicts))
	for index, conflict := range conflicts {
		out[index] = agentapi.EvidenceConflict{
			Identity: agentapi.EvidenceIdentity{
				SourceKind: conflict.Key.SourceKind,
				Target:     conflict.Key.Target,
				Section:    conflict.Key.Section,
				Version:    conflict.Key.Version,
				TimeRange:  conflict.Key.TimeRange,
			},
			Current:        evidence.CloneUnit(conflict.Current),
			Incoming:       evidence.CloneUnit(conflict.Incoming),
			CurrentOrigin:  conflict.CurrentOrigin,
			IncomingOrigin: conflict.IncomingOrigin,
		}
	}
	return out
}

func cloneConflicts(
	conflicts []agentapi.EvidenceConflict,
) []agentapi.EvidenceConflict {
	if len(conflicts) == 0 {
		return nil
	}
	out := make([]agentapi.EvidenceConflict, len(conflicts))
	for index, conflict := range conflicts {
		out[index] = cloneEvidenceConflict(conflict)
	}
	return out
}

func cloneEvidenceConflict(
	conflict agentapi.EvidenceConflict,
) agentapi.EvidenceConflict {
	conflict.Current = evidence.CloneUnit(conflict.Current)
	conflict.Incoming = evidence.CloneUnit(conflict.Incoming)
	return conflict
}
