package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

type probeEntry struct {
	Query       string `json:"query"`
	FactKeyHint string `json:"fact_key_hint"`
	KindHint    string `json:"kind_hint"`
}

// MemoryProbe is a non-durable retrieval hint derived before extraction.
type MemoryProbe struct {
	Query       string
	FactKeyHint string
	KindHint    MemoryKind
}

// PlanMemoryProbes identifies bounded retrieval queries without extracting facts.
func PlanMemoryProbes(ctx context.Context, client *llm.LLMClient, userMessage string) ([]MemoryProbe, error) {
	if client == nil || strings.TrimSpace(userMessage) == "" {
		return nil, nil
	}
	input, err := json.Marshal(map[string]string{"user_message": userMessage})
	if err != nil {
		return nil, fmt.Errorf("memory: encode probe input: %w", err)
	}
	var entries []probeEntry
	if err := client.ChatJSON(ctx, prompts.Text(prompts.MemoryProbe), string(input), &entries, llm.CallOptions{}); err != nil {
		return nil, fmt.Errorf("memory: plan probes: %w", err)
	}
	return normalizeMemoryProbes(entries), nil
}

func normalizeMemoryProbes(entries []probeEntry) []MemoryProbe {
	probes := make([]MemoryProbe, 0, min(5, len(entries)))
	seen := make(map[string]struct{}, min(5, len(entries)))
	for _, entry := range entries {
		query := strings.TrimSpace(entry.Query)
		if query == "" {
			continue
		}
		if len([]rune(query)) > 500 {
			continue
		}
		factKey := strings.ToLower(strings.TrimSpace(entry.FactKeyHint))
		if factKey != "" && !validFactKey(factKey) {
			factKey = ""
		}
		kind := MemoryKind(strings.ToLower(strings.TrimSpace(entry.KindHint)))
		if kind != "" && !validKind(kind) {
			kind = ""
		}
		key := factKey + "\x00" + query
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		probes = append(probes, MemoryProbe{Query: query, FactKeyHint: factKey, KindHint: kind})
		if len(probes) == 5 {
			break
		}
	}
	return probes
}
