package workflow

import (
	"context"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

const maxEvidenceTraceItems = 50

type aggregateInput struct {
	workflowRunID     string
	node              NodeDefinition
	inputs            []Handoff
	unavailableTasks  []unavailableTaskView
	baselineEvidence  []tool.EvidenceUnit
	maxDuplicateRatio float64
	maxBytes          int64
}

var aggregateTraceSpec = runtrace.Spec[aggregateInput, Handoff]{
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
			"unavailable_predecessor_ids": append(
				[]string(nil),
				unavailableTaskIDs(input.unavailableTasks)...,
			),
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

type evidenceTraceItem struct {
	SourceKind  string `json:"source_kind"`
	Target      string `json:"target"`
	Section     string `json:"section,omitempty"`
	Version     string `json:"version,omitempty"`
	TimeRange   string `json:"time_range,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	Origin      string `json:"origin,omitempty"`
}

type candidateTraceInput struct {
	nodeID string
	inputs []Handoff
}

type candidateTraceResult struct {
	items          []evidenceTraceItem
	candidateCount int
	omittedCount   int
}

var candidateTraceSpec = runtrace.Spec[
	candidateTraceInput,
	candidateTraceResult,
]{
	Operation: "evidence.candidate",
	Node:      "evidence.candidate",
	Input: func(input candidateTraceInput) map[string]any {
		producers := make([]string, 0, len(input.inputs))
		for _, handoff := range input.inputs {
			producers = append(producers, handoff.ProducerNodeID)
		}
		return map[string]any{
			"node_id":           input.nodeID,
			"producer_node_ids": producers,
		}
	},
	Output: func(
		_ candidateTraceInput,
		output candidateTraceResult,
		_ error,
	) map[string]any {
		return map[string]any{
			"candidate_count": output.candidateCount,
			"candidates":      output.items,
			"omitted_count":   output.omittedCount,
		}
	},
	Record: func(output candidateTraceResult, _ error) bool {
		return output.candidateCount > 0
	},
}

type mergedTraceInput struct {
	nodeID         string
	candidateCount int
	handoff        Handoff
}

type evidenceTraceOutput struct {
	items         []evidenceTraceItem
	evidenceCount int
	conflictCount int
}

var mergedTraceSpec = runtrace.Spec[mergedTraceInput, evidenceTraceOutput]{
	Operation: "evidence.merged",
	Node:      "evidence.merged",
	Input: func(input mergedTraceInput) map[string]any {
		return map[string]any{
			"node_id":         input.nodeID,
			"candidate_count": input.candidateCount,
		}
	},
	Output: func(_ mergedTraceInput, output evidenceTraceOutput, _ error) map[string]any {
		return map[string]any{
			"merged_count":  output.evidenceCount,
			"evidence":      output.items,
			"omitted_count": max(0, output.evidenceCount-len(output.items)),
		}
	},
	Record: func(output evidenceTraceOutput, _ error) bool {
		return output.evidenceCount > 0
	},
}

type rejectedTraceInput struct {
	nodeID    string
	conflicts []agentapi.EvidenceConflict
}

var rejectedTraceSpec = runtrace.Spec[rejectedTraceInput, []map[string]any]{
	Operation: "evidence.rejected",
	Node:      "evidence.rejected",
	Input: func(input rejectedTraceInput) map[string]any {
		return map[string]any{"node_id": input.nodeID}
	},
	Output: func(input rejectedTraceInput, conflicts []map[string]any, _ error) map[string]any {
		return map[string]any{
			"conflict_count": len(input.conflicts),
			"conflicts":      conflicts,
			"omitted_count":  max(0, len(input.conflicts)-len(conflicts)),
		}
	},
	Record: func(conflicts []map[string]any, _ error) bool {
		return len(conflicts) > 0
	},
}

type deliveredTraceInput struct {
	phase   string
	handoff Handoff
}

var deliveredTraceSpec = runtrace.Spec[deliveredTraceInput, evidenceTraceOutput]{
	Operation: "evidence.delivered",
	Node:      "evidence.delivered",
	Input: func(input deliveredTraceInput) map[string]any {
		return map[string]any{
			"phase":            input.phase,
			"producer_node_id": input.handoff.ProducerNodeID,
		}
	},
	Output: func(input deliveredTraceInput, output evidenceTraceOutput, _ error) map[string]any {
		return map[string]any{
			"evidence_count": output.evidenceCount,
			"evidence":       output.items,
			"conflict_count": output.conflictCount,
			"completeness":   input.handoff.Completeness,
			"omitted_count":  max(0, output.evidenceCount-len(output.items)),
		}
	},
	Record: func(output evidenceTraceOutput, _ error) bool {
		return output.evidenceCount > 0 || output.conflictCount > 0
	},
}

func (orchestrator *Orchestrator) aggregateHandoffs(
	ctx context.Context,
	workflowRunID string,
	node NodeDefinition,
	inputs []Handoff,
	unavailableTasks []unavailableTaskView,
	baselineEvidence []tool.EvidenceUnit,
	maxDuplicateRatio float64,
	maxBytes int64,
) (Handoff, error) {
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{WorkflowNodeID: node.ID})
	candidates, _ := runtrace.Invoke(
		ctx,
		candidateTraceSpec,
		candidateTraceInput{nodeID: node.ID, inputs: inputs},
		func(_ context.Context, input candidateTraceInput) (candidateTraceResult, error) {
			return candidateTraceOutput(input.inputs), nil
		},
	)
	handoff, err := runtrace.Invoke(
		ctx,
		aggregateTraceSpec,
		aggregateInput{
			workflowRunID:     workflowRunID,
			node:              node,
			inputs:            inputs,
			unavailableTasks:  append([]unavailableTaskView(nil), unavailableTasks...),
			baselineEvidence:  evidence.CloneUnits(baselineEvidence),
			maxDuplicateRatio: maxDuplicateRatio,
			maxBytes:          maxBytes,
		},
		func(_ context.Context, input aggregateInput) (Handoff, error) {
			return joinHandoffs(
				input.workflowRunID,
				input.node.ID,
				input.node.OutputSchema,
				input.node.JoinMode,
				input.inputs,
				input.unavailableTasks,
				input.baselineEvidence,
				input.maxDuplicateRatio,
				input.node.RejectEvidenceConflicts,
				input.maxBytes,
				orchestrator.schemas,
			)
		},
	)
	if err != nil {
		if len(handoff.EvidenceConflicts) > 0 {
			_, _ = runtrace.Invoke(
				ctx,
				rejectedTraceSpec,
				rejectedTraceInput{
					nodeID: node.ID, conflicts: handoff.EvidenceConflicts,
				},
				func(
					_ context.Context,
					input rejectedTraceInput,
				) ([]map[string]any, error) {
					return traceConflicts(input.conflicts), nil
				},
			)
		}
		return Handoff{}, err
	}
	_, _ = runtrace.Invoke(
		ctx,
		mergedTraceSpec,
		mergedTraceInput{
			nodeID: node.ID, candidateCount: candidates.candidateCount,
			handoff: handoff,
		},
		func(_ context.Context, input mergedTraceInput) (evidenceTraceOutput, error) {
			items, count := traceEvidenceUnits(input.handoff.EvidenceUnits, "")
			return evidenceTraceOutput{
				items: items, evidenceCount: count,
			}, nil
		},
	)
	_, _ = runtrace.Invoke(
		ctx,
		rejectedTraceSpec,
		rejectedTraceInput{
			nodeID: node.ID, conflicts: handoff.EvidenceConflicts,
		},
		func(_ context.Context, input rejectedTraceInput) ([]map[string]any, error) {
			return traceConflicts(input.conflicts), nil
		},
	)
	traceDelivered(ctx, "aggregation_output", handoff)
	return handoff, nil
}

func unavailableTaskIDs(tasks []unavailableTaskView) []string {
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.ProducerNodeID)
	}
	return ids
}

func traceDelivered(ctx context.Context, phase string, handoff Handoff) {
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{
		WorkflowNodeID: handoff.ProducerNodeID,
	})
	_, _ = runtrace.Invoke(
		ctx,
		deliveredTraceSpec,
		deliveredTraceInput{phase: phase, handoff: handoff},
		func(_ context.Context, input deliveredTraceInput) (evidenceTraceOutput, error) {
			items, count := traceEvidenceUnits(
				input.handoff.EvidenceUnits,
				input.handoff.ProducerNodeID,
			)
			return evidenceTraceOutput{
				items: items, evidenceCount: count,
				conflictCount: len(input.handoff.EvidenceConflicts),
			}, nil
		},
	)
}

func candidateTraceOutput(inputs []Handoff) candidateTraceResult {
	output := candidateTraceResult{}
	for _, input := range inputs {
		units := evidence.Expand(input.EvidenceUnits)
		for _, unit := range units {
			if _, ok := evidence.UnitKey(unit); !ok {
				continue
			}
			output.candidateCount++
			if len(output.items) < maxEvidenceTraceItems {
				output.items = append(
					output.items,
					traceEvidenceUnit(unit, input.ProducerNodeID),
				)
			}
		}
	}
	output.omittedCount = max(0, output.candidateCount-len(output.items))
	return output
}

func traceEvidenceUnits(
	units []tool.EvidenceUnit,
	origin string,
) ([]evidenceTraceItem, int) {
	expanded := evidence.Expand(units)
	items := make([]evidenceTraceItem, 0, min(len(expanded), maxEvidenceTraceItems))
	count := 0
	for _, unit := range expanded {
		if _, ok := evidence.UnitKey(unit); !ok {
			continue
		}
		count++
		if len(items) < maxEvidenceTraceItems {
			items = append(items, traceEvidenceUnit(unit, origin))
		}
	}
	return items, count
}

func traceEvidenceUnit(
	unit tool.EvidenceUnit,
	origin string,
) evidenceTraceItem {
	key, _ := evidence.UnitKey(unit)
	return evidenceTraceItem{
		SourceKind: key.SourceKind, Target: key.Target, Section: key.Section,
		Version: key.Version, TimeRange: key.TimeRange,
		ContentHash: unit.ContentHash, Origin: origin,
	}
}

func traceConflicts(
	conflicts []agentapi.EvidenceConflict,
) []map[string]any {
	items := make([]map[string]any, 0, min(len(conflicts), maxEvidenceTraceItems))
	for _, conflict := range conflicts {
		items = append(items, map[string]any{
			"source_kind":     conflict.Identity.SourceKind,
			"target":          conflict.Identity.Target,
			"section":         conflict.Identity.Section,
			"version":         conflict.Identity.Version,
			"time_range":      conflict.Identity.TimeRange,
			"current_hash":    conflict.Current.ContentHash,
			"incoming_hash":   conflict.Incoming.ContentHash,
			"current_origin":  conflict.CurrentOrigin,
			"incoming_origin": conflict.IncomingOrigin,
		})
		if len(items) == maxEvidenceTraceItems {
			break
		}
	}
	return items
}
