package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
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
	definition WorkflowDefinition,
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
			inputs,
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

func joinHandoffs(
	runID,
	producer string,
	schema agentapi.SchemaRef,
	inputs []Handoff,
	maxBytes int64,
	schemas *agentapi.SchemaRegistry,
) (Handoff, error) {
	ordered := append([]Handoff(nil), inputs...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].ProducerNodeID < ordered[j].ProducerNodeID
	})
	payloads := make([]json.RawMessage, 0, len(ordered))
	references := make([]agentapi.Reference, 0)
	completeness := Complete
	for _, input := range ordered {
		payloads = append(payloads, append(json.RawMessage(nil), input.Payload...))
		references = append(references, input.References...)
		if input.Completeness == Unavailable {
			completeness = Unavailable
		} else if input.Completeness == Partial && completeness == Complete {
			completeness = Partial
		}
	}
	payload, err := json.Marshal(payloads)
	if err != nil {
		return Handoff{}, fmt.Errorf("marshal joined handoffs: %w", err)
	}
	return PrepareHandoff(Handoff{
		WorkflowRunID: runID, ProducerNodeID: producer, Schema: schema,
		Payload: payload, References: references, Completeness: completeness,
	}, maxBytes, schemas)
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
