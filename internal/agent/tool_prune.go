package agent

import (
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

// baseToolIDSet keeps every tool the router is not allowed to prune.
// Routing candidates are the only tools pruning may drop; the base set is the
// complement so newly registered core tools stay available automatically.
func baseToolIDSet(snapshot tool.Snapshot, candidates []retrieval.ToolRouteCandidate) map[tool.ToolID]struct{} {
	prunable := make(map[tool.ToolID]struct{}, len(candidates))
	for _, candidate := range candidates {
		prunable[tool.ToolID(candidate.ID)] = struct{}{}
	}
	base := make(map[tool.ToolID]struct{})
	for _, candidate := range snapshot.Tools() {
		if _, prunable := prunable[candidate.ID]; prunable {
			continue
		}
		base[candidate.ID] = struct{}{}
	}
	return base
}

// pruneAllowance narrows routed tool IDs to the candidate set, mirroring the
// router's bindToolIDs contract: unknown or non-candidate IDs are dropped.
func pruneAllowance(routed []string, candidates []retrieval.ToolRouteCandidate) map[tool.ToolID]struct{} {
	candidateIDs := make(map[tool.ToolID]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidateIDs[tool.ToolID(candidate.ID)] = struct{}{}
	}
	allowed := make(map[tool.ToolID]struct{}, len(routed))
	for _, id := range routed {
		key := tool.ToolID(id)
		if _, ok := candidateIDs[key]; !ok {
			continue
		}
		allowed[key] = struct{}{}
	}
	return allowed
}

// decidePrune reports whether tool pruning may run this turn. It shares the
// planner's own fallback gates: healthy planning and a decision that did not
// degrade to the internal fallback. Any gate failing keeps the full set. The
// config toggle is applied separately to live pruning vs dry-run measurement.
func decidePrune(planningErr error, decision domain.PlanDecision) bool {
	return planningErr == nil && decision.Origin != domain.Fallback
}

// removedToolDefIDs reports the definition names dropped from full by kept.
// Both inputs are assumed ordered subsets, so a membership check per full entry
// is O(n) without a map.
func removedToolDefIDs(full, kept []llm.ToolDef) []string {
	keptSet := make(map[string]struct{}, len(kept))
	for _, def := range kept {
		keptSet[def.Function.Name] = struct{}{}
	}
	removed := make([]string, 0, len(full)-len(kept))
	for _, def := range full {
		if _, ok := keptSet[def.Function.Name]; !ok {
			removed = append(removed, def.Function.Name)
		}
	}
	return removed
}

// prunedDefinitions filters model-facing tool definitions to the allowed set,
// preserving the snapshot's declared order.
func prunedDefinitions(full []llm.ToolDef, allowed map[tool.ToolID]struct{}) []llm.ToolDef {
	kept := make([]llm.ToolDef, 0, len(full))
	for _, def := range full {
		if _, ok := allowed[tool.ToolID(def.Function.Name)]; !ok {
			continue
		}
		kept = append(kept, def)
	}
	return kept
}
