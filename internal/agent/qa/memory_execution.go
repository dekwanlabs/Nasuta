package qa

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
)

type memoryProbeInput struct {
	Client   *llm.LLMClient
	Question string
}

type memoryProbeOutput struct {
	Probes []memory.MemoryProbe
}

var memoryProbeSpec = runtrace.Spec[memoryProbeInput, memoryProbeOutput]{
	Operation: "memory.probe",
	Node:      "memory_probe",
	Output: func(_ memoryProbeInput, output memoryProbeOutput, _ error) map[string]any {
		return map[string]any{"probes": len(output.Probes)}
	},
}

func buildMemoryProbe(ctx context.Context, input memoryProbeInput) (memoryProbeOutput, error) {
	return runtrace.Invoke(ctx, memoryProbeSpec, input, func(ctx context.Context, input memoryProbeInput) (memoryProbeOutput, error) {
		probes, err := memory.PlanMemoryProbes(ctx, input.Client, input.Question)
		return memoryProbeOutput{Probes: probes}, err
	})
}

type writeRecallInput struct {
	Store  *memory.MemoryStore
	UserID int64
	Probes []memory.MemoryProbe
}

type writeRecallOutput struct {
	Result memory.ConsolidationRecallResult
}

var writeRecallSpec = runtrace.Spec[writeRecallInput, writeRecallOutput]{
	Operation: "memory.recall_for_write",
	Node:      "memory_recall_for_write",
	Output: func(_ writeRecallInput, output writeRecallOutput, err error) map[string]any {
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		stats := output.Result.Stats
		return map[string]any{
			"probes": stats.Probes, "exact_fact_keys": stats.ExactFactKeys,
			"candidates": stats.Candidates, "below_score": stats.BelowScore,
			"invalid_payload": stats.InvalidPayload, "missing_records": stats.MissingRecords,
			"unauthorized": stats.Unauthorized, "invalid_status": stats.InvalidStatus,
			"expired": stats.Expired, "per_fact_key_dropped": stats.PerFactKeyDropped,
			"admitted": stats.Admitted,
		}
	},
}

func recallMemoriesForWrite(ctx context.Context, input writeRecallInput) (writeRecallOutput, error) {
	return runtrace.Invoke(ctx, writeRecallSpec, input, func(
		ctx context.Context,
		input writeRecallInput,
	) (writeRecallOutput, error) {
		result, err := input.Store.RecallForConsolidation(ctx, input.UserID, input.Probes)
		return writeRecallOutput{Result: result}, err
	})
}

type memoryExtractInput struct {
	Client         *llm.LLMClient
	Question       string
	Answer         string
	Existing       []memory.ConsolidationMatch
	EvidenceStatus EvidenceStatus
}

type memoryExtractOutput struct {
	Decisions []memory.MemoryDecision
	Extracted int
	Rejected  map[string]int
}

var memoryExtractSpec = runtrace.Spec[memoryExtractInput, memoryExtractOutput]{
	Operation: "memory.extract",
	Node:      "memory_extract",
	Output: func(_ memoryExtractInput, output memoryExtractOutput, err error) map[string]any {
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		return map[string]any{
			"extracted": output.Extracted, "admitted": len(output.Decisions),
			"rejected_assistant_inference": output.Rejected["assistant_inference"],
			"rejected_incomplete_evidence": output.Rejected["incomplete_evidence"],
		}
	},
}

func extractMemories(ctx context.Context, input memoryExtractInput) (memoryExtractOutput, error) {
	return runtrace.Invoke(ctx, memoryExtractSpec, input, func(ctx context.Context, input memoryExtractInput) (memoryExtractOutput, error) {
		extracted, err := memory.ConsolidateMemories(ctx, input.Client, input.Question, input.Answer, input.Existing)
		if err != nil {
			return memoryExtractOutput{}, err
		}
		decisions, rejected := admitMemoryDecisions(extracted, input.EvidenceStatus)
		return memoryExtractOutput{Decisions: decisions, Extracted: len(extracted), Rejected: rejected}, nil
	})
}

type memoryWriteInput struct {
	Store     *memory.MemoryStore
	Decisions []memory.MemoryDecision
	UserID    int64
	SessionID string
}

type memoryWriteOutput struct {
	Outcomes     map[memory.WriteOutcome]int
	Actions      map[memory.ConsolidationAction]int
	VectorSynced int
}

var memoryWriteSpec = runtrace.Spec[memoryWriteInput, memoryWriteOutput]{
	Operation: "memory.write",
	Node:      "memory_write",
	Output: func(input memoryWriteInput, output memoryWriteOutput, _ error) map[string]any {
		return map[string]any{
			"decisions": len(input.Decisions), "add": output.Actions[memory.ConsolidationAdd],
			"refresh": output.Actions[memory.ConsolidationRefresh], "replace": output.Actions[memory.ConsolidationReplace],
			"reject": output.Actions[memory.ConsolidationReject], "discard": output.Actions[memory.ConsolidationDiscard],
			"inserted":  output.Outcomes[memory.WriteInserted],
			"refreshed": output.Outcomes[memory.WriteRefreshed], "superseded": output.Outcomes[memory.WriteSuperseded],
			"rejected": output.Outcomes[memory.WriteRejected], "vector_synced": output.VectorSynced,
		}
	},
}

func writeMemories(ctx context.Context, input memoryWriteInput) (memoryWriteOutput, error) {
	return runtrace.Invoke(ctx, memoryWriteSpec, input, func(ctx context.Context, input memoryWriteInput) (memoryWriteOutput, error) {
		output := memoryWriteOutput{
			Outcomes: make(map[memory.WriteOutcome]int, 4),
			Actions:  make(map[memory.ConsolidationAction]int, 5),
		}
		for index := range input.Decisions {
			decision := input.Decisions[index]
			output.Actions[decision.Action]++
			if decision.Action == memory.ConsolidationReject || decision.Action == memory.ConsolidationDiscard {
				continue
			}
			decision.Record.UserID = input.UserID
			decision.Record.SourceSession = input.SessionID
			result, err := input.Store.ApplyDecision(ctx, decision)
			if err != nil {
				log.ErrorfCtx(ctx, "[qa] memory apply action=%s error: %v", decision.Action, err)
				continue
			}
			output.Outcomes[result.Outcome]++
			if result.VectorSynced {
				output.VectorSynced++
			}
		}
		return output, nil
	})
}
