package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dekwanlabs/astris/llm"
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
	const system = `Extract at most 5 durable memories from the user message and assistant answer.

Return only a JSON array with this shape:
[{"fact_key":"...","kind":"...","content":"one line","source_type":"...","confidence":0.0}]

Allowed fact_key forms:
- user:response-language
- user:response-style
- user:role:<domain>
- user:current-focus
- workspace:<entity>:<attribute>

Use lowercase kebab-case for variable segments. Map paraphrases of the same fact to the same key.

Allowed kinds:
- preference
- profile
- work_context
- episode
- assistant_inference

Source rules:
- explicit_user only when the user explicitly asks to remember or correct a fact.
- user_stated only for a clear statement from the user message.
- assistant_inference only for a conclusion originating from the assistant answer.
- assistant_inference source must use assistant_inference kind.
- user-sourced records must not use assistant_inference kind.

Memory is for user preferences, roles, reusable work context, and historical experience.
Do not save current workspace/service/config/schema/runtime claims as user facts.
Do not save secrets, tokens, passwords, trace payloads, full logs, temporary debugging steps, or one-off requests.
Use episode for explicitly historical user statements. Output [] when nothing qualifies.`

	inputBytes, err := json.Marshal(map[string]string{
		"user_message":     userMessage,
		"assistant_answer": assistantAnswer,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: encode extraction input: %w", err)
	}
	var entries []extractedEntry
	if err := client.ChatJSON(ctx, system, string(inputBytes), &entries, llm.CallOptions{}); err != nil {
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
		}, 0, time.Time{})
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
