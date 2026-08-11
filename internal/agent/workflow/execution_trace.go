package workflow

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

type aggregateInput struct {
	workflowRunID string
	node          NodeDefinition
	inputs        []Handoff
	maxBytes      int64
}

var multiAgentAggregateTraceSpec = runtrace.Spec[aggregateInput, Handoff]{
	Operation: "multi_agent.aggregate",
	Node:      "multi_agent_aggregate",
	Input: func(input aggregateInput) map[string]any {
		producers := make([]string, 0, len(input.inputs))
		for _, handoff := range input.inputs {
			producers = append(producers, handoff.ProducerNodeID)
		}
		return map[string]any{
			"node_id":           input.node.ID,
			"input_count":       len(input.inputs),
			"producer_node_ids": producers,
		}
	},
	Output: func(_ aggregateInput, output Handoff, err error) map[string]any {
		fields := map[string]any{"completeness": output.Completeness}
		if err != nil {
			fields["error"] = err.Error()
		}
		return fields
	},
}

func (orchestrator *Orchestrator) aggregateHandoffs(
	ctx context.Context,
	workflowRunID string,
	node NodeDefinition,
	inputs []Handoff,
	maxBytes int64,
) (Handoff, error) {
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{WorkflowNodeID: node.ID})
	return runtrace.Invoke(
		ctx,
		multiAgentAggregateTraceSpec,
		aggregateInput{
			workflowRunID: workflowRunID,
			node:          node,
			inputs:        inputs,
			maxBytes:      maxBytes,
		},
		func(_ context.Context, input aggregateInput) (Handoff, error) {
			return joinHandoffs(
				input.workflowRunID,
				input.node.ID,
				input.node.OutputSchema,
				input.inputs,
				input.maxBytes,
				orchestrator.schemas,
			)
		},
	)
}
