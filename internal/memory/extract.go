package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

type extractedEntry struct {
	FactKey    string  `json:"fact_key"`
	Kind       string  `json:"kind"`
	Content    string  `json:"content"`
	SourceType string  `json:"source_type"`
	Confidence float32 `json:"confidence"`
}

// ExtractMemories distills one completed turn into controlled memory records.
func ExtractMemories(ctx context.Context, client *llm.LLMClient, userMessage, assistantAnswer string) ([]MemoryRecord, error) {
	if client == nil || userMessage == "" || assistantAnswer == "" {
		return nil, nil
	}
	inputBytes, err := json.Marshal(map[string]string{
		"user_message":     userMessage,
		"assistant_answer": assistantAnswer,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: encode extraction input: %w", err)
	}
	var entries []extractedEntry
	if err := client.ChatJSON(ctx, prompts.Text(prompts.MemoryExtract), string(inputBytes), &entries, llm.CallOptions{}); err != nil {
		return nil, fmt.Errorf("memory: extract: %w", err)
	}
	return normalizeExtracted(entries), nil
}

func normalizeExtracted(entries []extractedEntry) []MemoryRecord {
	records := make([]MemoryRecord, 0, min(5, len(entries)))
	positions := make(map[string]int, min(5, len(entries)))
	for _, entry := range entries {
		rec, err := canonicalizeRecord(MemoryRecord{
			FactKey:    entry.FactKey,
			Kind:       MemoryKind(entry.Kind),
			Content:    entry.Content,
			SourceType: SourceType(entry.SourceType),
			Confidence: entry.Confidence,
		})
		if err != nil {
			continue
		}
		if position, exists := positions[rec.FactKey]; exists {
			existing := records[position]
			if rec.Authority > existing.Authority ||
				(rec.Authority == existing.Authority && rec.Confidence > existing.Confidence) {
				records[position] = rec
			}
			continue
		}
		if len(records) >= 5 {
			continue
		}
		positions[rec.FactKey] = len(records)
		records = append(records, rec)
	}
	for i := range records {
		records[i].Status = ""
		records[i].Authority = 0
	}
	return records
}
