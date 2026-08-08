package qa

import (
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"strings"
)

type memoryRecallInput struct {
	Store          *memory.MemoryStore
	UserID         int64
	Query          string
	Limit          int
	TemporalIntent memory.TemporalIntent
}

type memoryRecallOutput struct {
	Result memory.RecallResult
	Status string
	Error  string
}

var memoryRecallSpec = executiontrace.Spec[*memoryRecallInput, memoryRecallOutput]{
	Operation: "memory.recall",
	Node:      "memory_recall",
	Input: func(input *memoryRecallInput) map[string]any {
		result := map[string]any{"user_id": input.UserID, "limit": input.Limit}
		if input.TemporalIntent != "" {
			result["temporal_intent"] = input.TemporalIntent
		}
		return result
	},
	Output: func(_ *memoryRecallInput, output memoryRecallOutput, _ error) map[string]any {
		if output.Status != "completed" {
			return map[string]any{"records": len(output.Result.Records), "error": output.Error}
		}
		stats := output.Result.Stats
		return map[string]any{
			"candidates": stats.Candidates, "invalid_payload": stats.InvalidPayload,
			"missing_records": stats.MissingRecords, "unauthorized": stats.Unauthorized,
			"superseded_filtered": stats.SupersededFiltered, "expired_filtered": stats.ExpiredFiltered,
			"episode_filtered": stats.EpisodeFiltered, "records": stats.Injected,
		}
	},
	Status: func(output memoryRecallOutput, _ error) string { return output.Status },
}

var memoryInjectSpec = executiontrace.Spec[[]memory.MemoryRecord, string]{
	Operation: "memory.inject",
	Node:      "memory_inject",
	Output: func(records []memory.MemoryRecord, formatted string, _ error) map[string]any {
		return map[string]any{"records": len(records), "characters": len([]rune(formatted))}
	},
}

func memoryExtractionAllowed(outcome RunOutcome, result *RunResult) bool {
	return outcome.Status == RunStatusDone && result != nil &&
		!result.ForcedConclusion && strings.TrimSpace(result.Answer) != ""
}

func admitExtractedMemories(records []memory.MemoryRecord, evidence EvidenceStatus) ([]memory.MemoryRecord, map[string]int) {
	admitted := make([]memory.MemoryRecord, 0, len(records))
	rejected := make(map[string]int, 2)
	incomplete := evidence == EvidencePartial || evidence == EvidenceUnavailable
	for _, record := range records {
		if record.SourceType == memory.SourceAssistantInference {
			rejected["assistant_inference"]++
			continue
		}
		if incomplete && record.SourceType != memory.SourceExplicitUser && record.SourceType != memory.SourceUserStated {
			rejected["incomplete_evidence"]++
			continue
		}
		admitted = append(admitted, record)
	}
	return admitted, rejected
}

func buildRagCtx(history []llm.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return history[i].Content
		}
	}
	return ""
}
