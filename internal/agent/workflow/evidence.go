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

type workflowEvidenceView struct {
	ProducerNodeID    string                      `json:"producer_node_id"`
	Completeness      Completeness                `json:"completeness"`
	EvidenceUnits     []tool.EvidenceUnit         `json:"evidence_units"`
	EvidenceConflicts []agentapi.EvidenceConflict `json:"evidence_conflicts"`
}

type workflowEvidenceHandoffView struct {
	ProducerNodeID string             `json:"producer_node_id"`
	Schema         agentapi.SchemaRef `json:"schema"`
	Payload        json.RawMessage    `json:"payload"`
	Completeness   Completeness       `json:"completeness"`
}

type workflowUnavailableTaskView struct {
	ProducerNodeID string `json:"producer_node_id"`
}

type workflowEvidenceLedgerView struct {
	Handoffs          []workflowEvidenceHandoffView `json:"handoffs"`
	UnavailableTasks  []workflowUnavailableTaskView `json:"unavailable_tasks"`
	EvidenceUnits     []tool.EvidenceUnit           `json:"evidence_units"`
	EvidenceConflicts []agentapi.EvidenceConflict   `json:"evidence_conflicts"`
	Completeness      Completeness                  `json:"completeness"`
}

func joinedPayload(
	mode JoinMode,
	inputs []Handoff,
	unavailablePredecessors []string,
	evidenceUnits []tool.EvidenceUnit,
	evidenceConflicts []agentapi.EvidenceConflict,
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
		handoffs := make([]workflowEvidenceHandoffView, 0, len(inputs))
		for _, input := range inputs {
			handoffs = append(handoffs, workflowEvidenceHandoffView{
				ProducerNodeID: input.ProducerNodeID,
				Schema:         input.Schema,
				Payload:        append(json.RawMessage(nil), input.Payload...),
				Completeness:   input.Completeness,
			})
		}
		unavailableTasks := make(
			[]workflowUnavailableTaskView,
			0,
			len(unavailablePredecessors),
		)
		for _, producerNodeID := range unavailablePredecessors {
			unavailableTasks = append(unavailableTasks, workflowUnavailableTaskView{
				ProducerNodeID: producerNodeID,
			})
		}
		if evidenceUnits == nil {
			evidenceUnits = []tool.EvidenceUnit{}
		}
		if evidenceConflicts == nil {
			evidenceConflicts = []agentapi.EvidenceConflict{}
		}
		value = workflowEvidenceLedgerView{
			Handoffs: handoffs, UnavailableTasks: unavailableTasks,
			EvidenceUnits:     evidenceUnits,
			EvidenceConflicts: evidenceConflicts, Completeness: completeness,
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

func contextBlockFromHandoff(handoff Handoff) (agentapi.ContextBlock, error) {
	view := workflowEvidenceView{
		ProducerNodeID:    handoff.ProducerNodeID,
		Completeness:      handoff.Completeness,
		EvidenceUnits:     evidence.CloneUnits(handoff.EvidenceUnits),
		EvidenceConflicts: cloneEvidenceConflicts(handoff.EvidenceConflicts),
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
		EvidenceConflicts: cloneEvidenceConflicts(handoff.EvidenceConflicts),
		Complete:          handoff.Completeness == Complete,
		ContentHash:       hex.EncodeToString(sum[:]),
	}, nil
}

func contextBlockFromTaskDirective(task TaskDirective) (agentapi.ContextBlock, error) {
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
		conflicts = appendUniqueEvidenceConflicts(
			conflicts,
			input.EvidenceConflicts,
			seenConflicts,
		)
		observed := ledger.Add(input.EvidenceUnits, input.ProducerNodeID)
		conflicts = appendUniqueEvidenceConflicts(
			conflicts,
			publicEvidenceConflicts(observed),
			seenConflicts,
		)
	}
	return ledger.Units(), conflicts
}

func appendUniqueEvidenceConflicts(
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

func publicEvidenceConflicts(
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

func cloneEvidenceConflicts(
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
