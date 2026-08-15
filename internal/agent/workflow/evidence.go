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
	ProducerNodeID    string                      `json:"producer_node_id"`
	Completeness      Completeness                `json:"completeness"`
	EvidenceUnits     []tool.EvidenceUnit         `json:"evidence_units"`
	EvidenceConflicts []agentapi.EvidenceConflict `json:"evidence_conflicts"`
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
	Handoffs          []handoffView               `json:"handoffs"`
	UnavailableTasks  []unavailableTaskView       `json:"unavailable_tasks"`
	EvidenceUnits     []tool.EvidenceUnit         `json:"evidence_units"`
	EvidenceConflicts []agentapi.EvidenceConflict `json:"evidence_conflicts"`
	Convergence       *convergenceView            `json:"convergence,omitempty"`
	Completeness      Completeness                `json:"completeness"`
}

func joinedPayload(
	mode JoinMode,
	inputs []Handoff,
	unavailableTasks []unavailableTaskView,
	evidenceUnits []tool.EvidenceUnit,
	evidenceConflicts []agentapi.EvidenceConflict,
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
			EvidenceUnits:     evidenceUnits,
			EvidenceConflicts: evidenceConflicts, Convergence: convergence,
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
		ProducerNodeID:    handoff.ProducerNodeID,
		Completeness:      handoff.Completeness,
		EvidenceUnits:     evidence.CloneUnits(handoff.EvidenceUnits),
		EvidenceConflicts: cloneConflicts(handoff.EvidenceConflicts),
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
		Source:            "workflow.handoff",
		Title:             "Workflow evidence ledger",
		Content:           string(content),
		References:        append([]agentapi.Reference(nil), handoff.References...),
		Evidence:          evidence.CloneUnits(handoff.EvidenceUnits),
		EvidenceConflicts: cloneConflicts(handoff.EvidenceConflicts),
		Complete:          handoff.Completeness == Complete,
		ContentHash:       hex.EncodeToString(sum[:]),
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

func mergeHandoffEvidence(
	inputs []Handoff,
) ([]tool.EvidenceUnit, []agentapi.EvidenceConflict) {
	ledger := evidence.New(nil, "")
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
