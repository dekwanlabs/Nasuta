package qa

import (
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

// baseToolIDSet keeps every tool the router is not allowed to prune.
// Routing candidates are the only tools pruning may drop; the base set is the
// complement so newly registered core tools stay available automatically.
func baseToolIDSet(tools []tool.Tool, candidates []retrieval.ToolRouteCandidate) map[tool.ToolID]struct{} {
	prunable := make(map[tool.ToolID]struct{}, len(candidates))
	for _, candidate := range candidates {
		prunable[tool.ToolID(candidate.ID)] = struct{}{}
	}
	base := make(map[tool.ToolID]struct{})
	for _, candidate := range tools {
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
