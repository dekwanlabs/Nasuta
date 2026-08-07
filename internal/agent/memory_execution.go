package agent

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/log"
)

type memoryExtractInput struct {
	Client         *llm.LLMClient
	Question       string
	Answer         string
	EvidenceStatus EvidenceStatus
}

type memoryExtractOutput struct {
	Records   []memory.MemoryRecord
	Extracted int
	Rejected  map[string]int
}

var memoryExtractSpec = executiontrace.Spec[memoryExtractInput, memoryExtractOutput]{
	Operation: "memory.extract",
	Node:      "memory_extract",
	Output: func(_ memoryExtractInput, output memoryExtractOutput, err error) map[string]any {
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		return map[string]any{
			"extracted": output.Extracted, "admitted": len(output.Records),
			"rejected_assistant_inference": output.Rejected["assistant_inference"],
			"rejected_incomplete_evidence": output.Rejected["incomplete_evidence"],
		}
	},
}

func extractMemories(ctx context.Context, input memoryExtractInput) (memoryExtractOutput, error) {
	return executiontrace.Invoke(ctx, memoryExtractSpec, input, func(ctx context.Context, input memoryExtractInput) (memoryExtractOutput, error) {
		extracted, err := memory.ExtractMemories(ctx, input.Client, input.Question, input.Answer)
		if err != nil {
			return memoryExtractOutput{}, err
		}
		records, rejected := admitExtractedMemories(extracted, input.EvidenceStatus)
		return memoryExtractOutput{Records: records, Extracted: len(extracted), Rejected: rejected}, nil
	})
}

type memoryWriteInput struct {
	Store     *memory.MemoryStore
	Records   []memory.MemoryRecord
	UserID    int64
	SessionID string
}

type memoryWriteOutput struct {
	Outcomes     map[memory.WriteOutcome]int
	VectorSynced int
}

var memoryWriteSpec = executiontrace.Spec[memoryWriteInput, memoryWriteOutput]{
	Operation: "memory.write",
	Node:      "memory_write",
	Output: func(input memoryWriteInput, output memoryWriteOutput, _ error) map[string]any {
		return map[string]any{
			"records": len(input.Records), "inserted": output.Outcomes[memory.WriteInserted],
			"refreshed": output.Outcomes[memory.WriteRefreshed], "superseded": output.Outcomes[memory.WriteSuperseded],
			"rejected": output.Outcomes[memory.WriteRejected], "vector_synced": output.VectorSynced,
		}
	},
}

func writeMemories(ctx context.Context, input memoryWriteInput) (memoryWriteOutput, error) {
	return executiontrace.Invoke(ctx, memoryWriteSpec, input, func(ctx context.Context, input memoryWriteInput) (memoryWriteOutput, error) {
		output := memoryWriteOutput{Outcomes: make(map[memory.WriteOutcome]int, 4)}
		for index := range input.Records {
			input.Records[index].UserID = input.UserID
			input.Records[index].SourceSession = input.SessionID
			result, err := input.Store.Write(ctx, input.Records[index])
			if err != nil {
				log.ErrorfCtx(ctx, "[qa] memory write error: %v", err)
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
