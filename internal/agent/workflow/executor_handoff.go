package workflow

import (
	"encoding/json"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

func (orchestrator *Orchestrator) validateNodeInputs(
	node NodeDefinition,
	inputs []Handoff,
	maxBytes int64,
) error {
	for _, input := range inputs {
		if _, err := PrepareHandoff(input, maxBytes, orchestrator.schemas); err != nil {
			return fmt.Errorf("input from %q: %w", input.ProducerNodeID, err)
		}
		if err := orchestrator.schemas.ValidateCompatibility(input.Schema, node.InputSchema); err != nil {
			return fmt.Errorf("input from %q schema: %w", input.ProducerNodeID, err)
		}
		if err := orchestrator.schemas.Validate(node.InputSchema, input.Payload); err != nil {
			return fmt.Errorf("input from %q payload: %w", input.ProducerNodeID, err)
		}
	}
	return nil
}

func readyNodes(
	metadata graphMetadata,
	outputs map[string]Handoff,
	failedOptional map[string]struct{},
	waitingHuman map[string]struct{},
) []string {
	ready := make([]string, 0)
	for _, nodeID := range metadata.order {
		if _, done := outputs[nodeID]; done {
			continue
		}
		if _, failed := failedOptional[nodeID]; failed {
			continue
		}
		if _, waiting := waitingHuman[nodeID]; waiting {
			continue
		}
		runnable := true
		for _, predecessor := range metadata.predecessors[nodeID] {
			if _, succeeded := outputs[predecessor]; succeeded {
				continue
			}
			if _, failed := failedOptional[predecessor]; failed {
				if metadata.required[predecessor+"\x00"+nodeID] {
					runnable = false
				}
				continue
			}
			runnable = false
			break
		}
		if runnable {
			ready = append(ready, nodeID)
		}
	}
	return ready
}

func (orchestrator *Orchestrator) approvalHandoff(
	definition Definition,
	runID string,
	node NodeDefinition,
	inputs []Handoff,
	decidedAt time.Time,
) (Handoff, error) {
	if node.Kind != NodeHumanApproval {
		return Handoff{}, fmt.Errorf("workflow node %q does not require human approval", node.ID)
	}
	if err := orchestrator.validateNodeInputs(node, inputs, definition.Budget.MaxHandoffBytes); err != nil {
		return Handoff{}, err
	}
	if len(inputs) == 0 {
		return Handoff{}, fmt.Errorf("workflow human approval node %q has no input", node.ID)
	}
	if len(inputs) > 1 {
		handoff, err := joinHandoffs(
			runID,
			node.ID,
			node.OutputSchema,
			JoinPayloadList,
			inputs,
			nil,
			nil,
			0,
			false,
			definition.Budget.MaxHandoffBytes,
			orchestrator.schemas,
		)
		if err != nil {
			return Handoff{}, err
		}
		handoff.CreatedAt = decidedAt
		return handoff, nil
	}
	input := inputs[0]
	return PrepareHandoff(Handoff{
		WorkflowRunID: runID, ProducerNodeID: node.ID, Schema: node.OutputSchema,
		Payload: input.Payload, References: input.References,
		EvidenceUnits: input.EvidenceUnits, EvidenceConflicts: input.EvidenceConflicts,
		Completeness: input.Completeness, CreatedAt: decidedAt,
	}, definition.Budget.MaxHandoffBytes, orchestrator.schemas)
}

func predecessorHandoffs(nodeID string, predecessors map[string][]string, outputs map[string]Handoff, input Handoff) []Handoff {
	ids := predecessors[nodeID]
	if len(ids) == 0 {
		return []Handoff{input}
	}
	handoffs := make([]Handoff, 0, len(ids))
	for _, id := range ids {
		if handoff, ok := outputs[id]; ok {
			handoffs = append(handoffs, handoff)
		}
	}
	return handoffs
}

func unavailablePredecessors(
	nodeID string,
	predecessors map[string][]string,
	failedOptional map[string]struct{},
) []string {
	ids := predecessors[nodeID]
	unavailable := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, failed := failedOptional[id]; failed {
			unavailable = append(unavailable, id)
		}
	}
	return unavailable
}

func joinHandoffs(
	runID,
	producer string,
	schema agentapi.SchemaRef,
	mode JoinMode,
	inputs []Handoff,
	unavailableTasks []unavailableTaskView,
	baselineEvidence []tool.EvidenceUnit,
	maxDuplicateRatio float64,
	rejectEvidenceConflicts bool,
	maxBytes int64,
	schemas *agentapi.SchemaRegistry,
) (Handoff, error) {
	references := make([]agentapi.Reference, 0)
	baselineIdentities := canonicalEvidenceIdentities(baselineEvidence)
	evidenceUnits, evidenceConflicts := mergeEvidenceHandoffs(
		baselineEvidence,
		inputs,
	)
	completeness := Complete
	for _, input := range inputs {
		references = append(references, input.References...)
		if input.Completeness == Unavailable {
			completeness = Unavailable
		} else if input.Completeness == Partial && completeness == Complete {
			completeness = Partial
		}
	}
	if len(unavailableTasks) > 0 {
		if len(inputs) == 0 {
			completeness = Unavailable
		} else {
			completeness = Partial
		}
	}
	var convergence *convergenceView
	if mode == JoinEvidenceView {
		measured := measureConvergence(
			inputs,
			baselineEvidence,
			maxDuplicateRatio,
		)
		convergence = &measured
	}
	evidenceUnitsTotal := 0
	evidenceUnitsOmitted := 0
	if mode == JoinEvidenceView {
		payloadFor := func(
			units []tool.EvidenceUnit,
			total, omitted int,
		) (json.RawMessage, error) {
			return joinedPayload(
				mode,
				inputs,
				unavailableTasks,
				units,
				baselineIdentities,
				evidenceConflicts,
				total,
				omitted,
				convergence,
				completeness,
			)
		}
		compacted, total, omitted, err := compactInvestigationEvidenceToBudget(
			inputs,
			evidenceUnits,
			evidenceIdentityKeySet(baselineIdentities),
			maxBytes,
			payloadFor,
		)
		if err != nil {
			return Handoff{}, fmt.Errorf("compact join %q evidence: %w", producer, err)
		}
		evidenceUnits = compacted
		if omitted > 0 {
			evidenceUnitsTotal = total
			evidenceUnitsOmitted = omitted
			log.Warnf(
				"[workflow] join %s retained %d of %d evidence units; omitted %d uncited or over-budget unit(s)",
				producer, len(compacted), total, omitted,
			)
		}
	}
	payload, err := joinedPayload(
		mode,
		inputs,
		unavailableTasks,
		evidenceUnits,
		baselineIdentities,
		evidenceConflicts,
		evidenceUnitsTotal,
		evidenceUnitsOmitted,
		convergence,
		completeness,
	)
	if err != nil {
		return Handoff{}, err
	}
	handoff, err := PrepareHandoff(Handoff{
		WorkflowRunID: runID, ProducerNodeID: producer, Schema: schema,
		Payload: payload, References: references,
		EvidenceUnits: evidenceUnits, EvidenceConflicts: evidenceConflicts,
		Completeness: completeness,
	}, maxBytes, schemas)
	if err != nil {
		return Handoff{}, err
	}
	if rejectEvidenceConflicts && len(evidenceConflicts) > 0 {
		return handoff, conflictRejectionError{
			producer: producer,
			count:    len(evidenceConflicts),
		}
	}
	return handoff, nil
}

func unavailableTaskViews(
	nodeIDs []string,
	reasons map[string]StopReason,
) []unavailableTaskView {
	tasks := make([]unavailableTaskView, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		tasks = append(tasks, unavailableTaskView{
			ProducerNodeID: nodeID,
			StopReason:     reasons[nodeID],
		})
	}
	return tasks
}

type conflictRejectionError struct {
	producer string
	count    int
}

func (err conflictRejectionError) Error() string {
	return fmt.Sprintf(
		"join %q rejected %d evidence conflict(s)",
		err.producer,
		err.count,
	)
}

func (err conflictRejectionError) Is(target error) bool {
	return target == ErrEvidenceConflict
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneHandoffMap(source map[string]Handoff) map[string]Handoff {
	cloned := make(map[string]Handoff, len(source))
	for nodeID, handoff := range source {
		handoff.Payload = append(json.RawMessage(nil), handoff.Payload...)
		handoff.References = append([]agentapi.Reference(nil), handoff.References...)
		handoff.EvidenceUnits = evidence.CloneUnits(handoff.EvidenceUnits)
		handoff.EvidenceConflicts = cloneConflicts(handoff.EvidenceConflicts)
		cloned[nodeID] = handoff
	}
	return cloned
}

func cloneGateMap(source map[string]GateDecision) map[string]GateDecision {
	cloned := make(map[string]GateDecision, len(source))
	for nodeID, decision := range source {
		decision.ReasonCodes = append([]string(nil), decision.ReasonCodes...)
		decision.FindingIDs = append([]string(nil), decision.FindingIDs...)
		cloned[nodeID] = decision
	}
	return cloned
}

func cloneStringSet(source map[string]struct{}) map[string]struct{} {
	cloned := make(map[string]struct{}, len(source))
	for value := range source {
		cloned[value] = struct{}{}
	}
	return cloned
}

func cloneStopReasonMap(source map[string]StopReason) map[string]StopReason {
	cloned := make(map[string]StopReason, len(source))
	for nodeID, reason := range source {
		cloned[nodeID] = reason
	}
	return cloned
}
