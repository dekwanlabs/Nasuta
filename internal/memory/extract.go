package memory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

type extractedEntry struct {
	FactKey            string  `json:"fact_key"`
	Kind               string  `json:"kind"`
	Content            string  `json:"content"`
	SourceType         string  `json:"source_type"`
	Confidence         float32 `json:"confidence"`
	Action             string  `json:"action"`
	TargetID           string  `json:"target_id"`
	Relation           string  `json:"relation"`
	DecisionConfidence float32 `json:"decision_confidence"`
}

// ExtractMemories distills one completed turn into controlled memory records.
func ExtractMemories(ctx context.Context, client *llm.LLMClient, userMessage, assistantAnswer string) ([]MemoryRecord, error) {
	decisions, err := ConsolidateMemories(ctx, client, userMessage, assistantAnswer, nil)
	if err != nil {
		return nil, err
	}
	records := make([]MemoryRecord, 0, len(decisions))
	for _, decision := range decisions {
		if decision.Action != ConsolidationDiscard && decision.Action != ConsolidationReject {
			records = append(records, decision.Record)
		}
	}
	return records, nil
}

// ConsolidateMemories compares one completed turn with recalled durable state.
func ConsolidateMemories(
	ctx context.Context,
	client *llm.LLMClient,
	userMessage, assistantAnswer string,
	existing []ConsolidationMatch,
) ([]MemoryDecision, error) {
	if client == nil || userMessage == "" || assistantAnswer == "" {
		return nil, nil
	}
	inputBytes, err := json.Marshal(map[string]any{
		"user_message":      userMessage,
		"assistant_answer":  assistantAnswer,
		"existing_memories": consolidationPromptMemories(existing),
	})
	if err != nil {
		return nil, fmt.Errorf("memory: encode extraction input: %w", err)
	}
	var entries []extractedEntry
	if err := client.ChatJSON(ctx, prompts.Text(prompts.MemoryExtract), string(inputBytes), &entries, llm.CallOptions{}); err != nil {
		return nil, fmt.Errorf("memory: extract: %w", err)
	}
	return normalizeConsolidated(entries, existing), nil
}

func normalizeExtracted(entries []extractedEntry) []MemoryRecord {
	decisions := normalizeConsolidated(entries, nil)
	records := make([]MemoryRecord, 0, len(decisions))
	for _, decision := range decisions {
		if decision.Action == ConsolidationAdd {
			records = append(records, decision.Record)
		}
	}
	return records
}

func normalizeConsolidated(entries []extractedEntry, existing []ConsolidationMatch) []MemoryDecision {
	targets := make(map[string]MemoryRecord, len(existing))
	for _, match := range existing {
		targets[match.Record.ID] = match.Record
	}
	decisions := make([]MemoryDecision, 0, min(5, len(entries)))
	positions := make(map[string]int, min(5, len(entries)))
	for _, entry := range entries {
		action := ConsolidationAction(entry.Action)
		if action == "" && len(existing) == 0 {
			action = ConsolidationAdd
		}
		if action == ConsolidationDiscard {
			continue
		}
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
		decision := MemoryDecision{
			Record: rec, Action: action, TargetID: entry.TargetID,
			Relation: entry.Relation, DecisionConfidence: entry.DecisionConfidence,
		}
		switch action {
		case ConsolidationAdd:
			decision.TargetID = ""
		case ConsolidationRefresh, ConsolidationReplace, ConsolidationReject:
			target, ok := targets[entry.TargetID]
			if !ok || target.FactKey != rec.FactKey || target.Status != StatusActive ||
				entry.DecisionConfidence < consolidationDecisionMinScore {
				continue
			}
			if action == ConsolidationReplace && rec.Authority < target.Authority {
				decision.Action = ConsolidationReject
			}
		default:
			continue
		}
		if position, exists := positions[rec.FactKey]; exists {
			current := decisions[position]
			if rec.Authority > current.Record.Authority ||
				rec.Authority == current.Record.Authority && decision.DecisionConfidence > current.DecisionConfidence {
				decisions[position] = decision
			}
			continue
		}
		if len(decisions) >= 5 {
			continue
		}
		positions[rec.FactKey] = len(decisions)
		decisions = append(decisions, decision)
	}
	for i := range decisions {
		decisions[i].Record.Status = ""
		decisions[i].Record.Authority = 0
	}
	return decisions
}

func consolidationPromptMemories(matches []ConsolidationMatch) []map[string]any {
	out := make([]map[string]any, 0, len(matches))
	for _, match := range matches {
		rec := match.Record
		out = append(out, map[string]any{
			"id": rec.ID, "fact_key": rec.FactKey, "kind": rec.Kind,
			"content": rec.Content, "source_type": rec.SourceType,
			"status": rec.Status, "confidence": rec.Confidence,
			"dense_score": match.DenseScore, "exact_fact_key": match.ExactFactKey,
		})
	}
	return out
}
