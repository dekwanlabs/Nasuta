package execution

import (
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

func removedToolDefIDs(full, kept []llm.ToolDef) []string {
	keptSet := make(map[string]struct{}, len(kept))
	for _, definition := range kept {
		keptSet[definition.Function.Name] = struct{}{}
	}
	removed := make([]string, 0, len(full)-len(kept))
	for _, definition := range full {
		if _, ok := keptSet[definition.Function.Name]; !ok {
			removed = append(removed, definition.Function.Name)
		}
	}
	return removed
}

func prunedDefinitions(full []llm.ToolDef, allowed map[tool.ToolID]struct{}) []llm.ToolDef {
	kept := make([]llm.ToolDef, 0, len(full))
	for _, definition := range full {
		if _, ok := allowed[tool.ToolID(definition.Function.Name)]; !ok {
			continue
		}
		kept = append(kept, definition)
	}
	return kept
}
