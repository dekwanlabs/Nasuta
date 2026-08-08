package qa

import (
	"errors"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestBaseToolIDSetKeepsCoreAndExcludesRoutingCandidates(t *testing.T) {
	registry := testRegistry(t,
		testAgentTool("get_service", ToolKindRead, noopTool),
		testAgentTool("search_code", ToolKindRead, noopTool),
		scenarioTool("observe_logs"),
		scenarioTool("search_config"),
	)
	snapshot := registry.Snapshot(tool.ReadPolicy())
	tools := snapshot.Tools()
	base := baseToolIDSet(tools, routingCandidates(tools))

	for _, id := range []tool.ToolID{"get_service", "search_code"} {
		if _, ok := base[id]; !ok {
			t.Fatalf("base set missing core tool %q", id)
		}
	}
	for _, id := range []tool.ToolID{"observe_logs", "search_config"} {
		if _, ok := base[id]; ok {
			t.Fatalf("base set unexpectedly includes routing candidate %q", id)
		}
	}
}

func TestPruneAllowanceDropsUnknownAndKeepsRouted(t *testing.T) {
	candidates := []retrieval.ToolRouteCandidate{
		{ID: "observe_logs"},
		{ID: "search_config"},
	}
	allowed := pruneAllowance([]string{"observe_logs", "not_a_tool", "search_config"}, candidates)
	if len(allowed) != 2 {
		t.Fatalf("allowed = %v, want exactly observe_logs+search_config", allowed)
	}
	for _, id := range []tool.ToolID{"observe_logs", "search_config"} {
		if _, ok := allowed[id]; !ok {
			t.Fatalf("routed tool %q was dropped", id)
		}
	}
}

func TestDecidePruneGates(t *testing.T) {
	confident := domain.PlanDecision{Plan: domain.EvidencePlan{Sources: domain.Internal}, Confidence: 0.95, Origin: domain.Model}
	fallback := domain.InternalFallbackDecision()

	if !decidePrune(nil, confident) {
		t.Fatal("confident model routing should allow pruning")
	}
	if decidePrune(errors.New("planning degraded"), confident) {
		t.Fatal("planning degradation must keep the full set")
	}
	if decidePrune(nil, fallback) {
		t.Fatal("internal fallback decision must keep the full set")
	}
}

func TestPrunedToolIDSetIsBaseUnionRouted(t *testing.T) {
	registry := testRegistry(t,
		testAgentTool("get_service", ToolKindRead, noopTool),
		scenarioTool("observe_logs"),
		scenarioTool("search_config"),
	)
	svc := &QA{}
	snapshot := registry.Snapshot(tool.ReadPolicy())
	allowed := svc.prunedToolIDSet(snapshot.Tools(), []string{"observe_logs"})

	for _, id := range []tool.ToolID{"get_service", "observe_logs"} {
		if _, ok := allowed[id]; !ok {
			t.Fatalf("allowed set missing %q", id)
		}
	}
	if _, ok := allowed["search_config"]; ok {
		t.Fatal("unrouted scenario tool must be pruned")
	}
}
