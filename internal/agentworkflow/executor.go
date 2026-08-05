package agentworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

var ErrHumanApprovalRequired = errors.New("workflow requires human approval")

type RunRequest struct {
	RunID               string
	Input               json.RawMessage
	ActorPermissions    agentapi.PermissionPolicy
	ScenarioPermissions agentapi.PermissionPolicy
}

type NodeRequest struct {
	WorkflowRunID        string
	Node                 NodeDefinition
	Inputs               []Handoff
	EffectivePermissions agentapi.PermissionPolicy
}

type NodeExecutor interface {
	Execute(context.Context, NodeRequest) (Handoff, error)
}

type GateEvaluator interface {
	Evaluate(context.Context, NodeRequest) (GateDecision, error)
}

type Result struct {
	RunID       string
	Output      Handoff
	NodeOutputs map[string]Handoff
	Gates       map[string]GateDecision
}

// Orchestrator executes stable DAG waves while each node retains an independent context.
type Orchestrator struct {
	nodes NodeExecutor
	gates map[string]GateEvaluator
}

func NewOrchestrator(nodes NodeExecutor, gates map[string]GateEvaluator) *Orchestrator {
	cloned := make(map[string]GateEvaluator, len(gates))
	for id, gate := range gates {
		cloned[id] = gate
	}
	return &Orchestrator{nodes: nodes, gates: cloned}
}

func (orchestrator *Orchestrator) Run(ctx context.Context, definition WorkflowDefinition, request RunRequest) (Result, error) {
	prepared, err := Prepare(definition)
	if err != nil {
		return Result{}, err
	}
	if request.RunID == "" || !json.Valid(request.Input) {
		return Result{}, fmt.Errorf("workflow run id and valid JSON input are required")
	}
	metadata, err := graph(prepared)
	if err != nil {
		return Result{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, prepared.Budget.Timeout)
	defer cancel()
	input, err := PrepareHandoff(Handoff{
		WorkflowRunID: request.RunID, ProducerNodeID: "workflow.input",
		Schema: prepared.InputSchema, Payload: request.Input, Completeness: Complete,
	}, prepared.Budget.MaxHandoffBytes)
	if err != nil {
		return Result{}, err
	}
	outputs := make(map[string]Handoff, len(prepared.Nodes))
	gates := make(map[string]GateDecision)
	failedOptional := make(map[string]struct{})

	for len(outputs)+len(failedOptional) < len(prepared.Nodes) {
		ready := readyNodes(metadata, outputs, failedOptional)
		if len(ready) == 0 {
			return Result{}, fmt.Errorf("workflow %q cannot make progress", prepared.ID)
		}
		wave := make([]nodeOutcome, len(ready))
		var wg sync.WaitGroup
		sem := make(chan struct{}, prepared.Budget.MaxParallelism)
		for index, nodeID := range ready {
			index, node := index, metadata.nodes[nodeID]
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-runCtx.Done():
					wave[index].err = runCtx.Err()
					return
				}
				inputs := predecessorHandoffs(node.ID, metadata.predecessors, outputs, input)
				wave[index] = orchestrator.executeNode(runCtx, prepared, request, node, inputs)
			}()
		}
		wg.Wait()
		for index, outcome := range wave {
			node := metadata.nodes[ready[index]]
			if outcome.err != nil {
				if node.Optional && prepared.FailurePolicy.Mode == CollectAvailable {
					failedOptional[node.ID] = struct{}{}
					continue
				}
				return Result{}, fmt.Errorf("workflow %q node %q: %w", prepared.ID, node.ID, outcome.err)
			}
			outputs[node.ID] = outcome.handoff
			if outcome.gate != nil {
				gates[node.ID] = *outcome.gate
			}
		}
	}
	terminals := make([]Handoff, 0)
	for _, nodeID := range metadata.order {
		if len(metadata.successors[nodeID]) == 0 {
			if output, ok := outputs[nodeID]; ok {
				terminals = append(terminals, output)
			}
		}
	}
	if len(terminals) == 0 {
		return Result{}, fmt.Errorf("workflow %q produced no terminal output", prepared.ID)
	}
	output := terminals[0]
	if len(terminals) > 1 {
		output, err = joinHandoffs(request.RunID, "workflow.output", prepared.OutputSchema, terminals, prepared.Budget.MaxHandoffBytes)
		if err != nil {
			return Result{}, err
		}
	} else if output.Schema != prepared.OutputSchema {
		return Result{}, fmt.Errorf("workflow %q terminal output schema does not match its contract", prepared.ID)
	}
	return Result{RunID: request.RunID, Output: output, NodeOutputs: outputs, Gates: gates}, nil
}

type nodeOutcome struct {
	handoff Handoff
	gate    *GateDecision
	err     error
}

func (orchestrator *Orchestrator) executeNode(ctx context.Context, definition WorkflowDefinition, request RunRequest, node NodeDefinition, inputs []Handoff) nodeOutcome {
	nodeCtx, cancel := context.WithTimeout(ctx, node.Timeout)
	defer cancel()
	nodeRequest := NodeRequest{
		WorkflowRunID: request.RunID, Node: node, Inputs: inputs,
		EffectivePermissions: IntersectPermissions(
			request.ActorPermissions, request.ScenarioPermissions,
			definition.Permissions, node.Permissions,
		),
	}
	var handoff Handoff
	switch node.Kind {
	case NodeJoin:
		joined, err := joinHandoffs(request.RunID, node.ID, node.OutputSchema, inputs, definition.Budget.MaxHandoffBytes)
		return nodeOutcome{handoff: joined, err: err}
	case NodeGate:
		evaluator := orchestrator.gates[node.Gate.ID]
		if evaluator == nil {
			return nodeOutcome{err: fmt.Errorf("gate evaluator %q is not registered", node.Gate.ID)}
		}
		decision, err := evaluator.Evaluate(nodeCtx, nodeRequest)
		if err != nil {
			return nodeOutcome{err: err}
		}
		if !contains(node.Gate.AllowedDecisions, decision.Decision) {
			return nodeOutcome{err: fmt.Errorf("gate %q returned unsupported decision %q", node.Gate.ID, decision.Decision)}
		}
		decision.GateID = node.Gate.ID
		if decision.EvaluatedAt.IsZero() {
			decision.EvaluatedAt = time.Now().UTC()
		}
		payload, err := json.Marshal(decision)
		if err != nil {
			return nodeOutcome{err: fmt.Errorf("marshal gate decision: %w", err)}
		}
		handoff, err = PrepareHandoff(Handoff{
			WorkflowRunID: request.RunID, ProducerNodeID: node.ID,
			Schema: node.OutputSchema, Payload: payload, Completeness: Complete,
		}, definition.Budget.MaxHandoffBytes)
		return nodeOutcome{handoff: handoff, gate: &decision, err: err}
	case NodeHumanApproval:
		return nodeOutcome{err: ErrHumanApprovalRequired}
	case NodeAgent, NodeTransform:
		if orchestrator.nodes == nil {
			return nodeOutcome{err: fmt.Errorf("node executor is not configured")}
		}
		var err error
		handoff, err = orchestrator.nodes.Execute(nodeCtx, nodeRequest)
		if err != nil {
			return nodeOutcome{err: err}
		}
	default:
		return nodeOutcome{err: fmt.Errorf("node kind %q is unsupported", node.Kind)}
	}
	handoff.WorkflowRunID = request.RunID
	handoff.ProducerNodeID = node.ID
	handoff.Schema = node.OutputSchema
	prepared, err := PrepareHandoff(handoff, definition.Budget.MaxHandoffBytes)
	return nodeOutcome{handoff: prepared, err: err}
}

func readyNodes(metadata graphMetadata, outputs map[string]Handoff, failedOptional map[string]struct{}) []string {
	ready := make([]string, 0)
	for _, nodeID := range metadata.order {
		if _, done := outputs[nodeID]; done {
			continue
		}
		if _, failed := failedOptional[nodeID]; failed {
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

func joinHandoffs(runID, producer string, schema agentapi.SchemaRef, inputs []Handoff, maxBytes int64) (Handoff, error) {
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
	}, maxBytes)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
