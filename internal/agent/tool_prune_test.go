package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

// scenarioTool builds a read tool that the router is allowed to select.
func scenarioTool(id tool.ToolID) Tool {
	candidate := testAgentTool(id, ToolKindRead, noopTool)
	candidate.Routing = &tool.RoutingSpec{Intent: "current runtime evidence"}
	return candidate
}

func TestBaseToolIDSetKeepsCoreAndExcludesRoutingCandidates(t *testing.T) {
	registry := testRegistry(t,
		testAgentTool("get_service", ToolKindRead, noopTool),
		testAgentTool("search_code", ToolKindRead, noopTool),
		scenarioTool("observe_logs"),
		scenarioTool("search_config"),
	)
	snapshot := registry.Snapshot(tool.ReadPolicy())
	base := baseToolIDSet(snapshot, routingCandidates(snapshot))

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

func TestPrunedDefinitionsPreservesOrderAndMembership(t *testing.T) {
	full := []llm.ToolDef{
		{Function: llm.ToolFunctionDef{Name: "get_service"}},
		{Function: llm.ToolFunctionDef{Name: "observe_logs"}},
		{Function: llm.ToolFunctionDef{Name: "search_code"}},
		{Function: llm.ToolFunctionDef{Name: "search_config"}},
	}
	allowed := map[tool.ToolID]struct{}{
		"get_service": {},
		"search_code": {},
		"observe_logs": {},
	}
	kept := prunedDefinitions(full, allowed)
	if got := strings.Join(toolDefNames(kept), ","); got != "get_service,observe_logs,search_code" {
		t.Fatalf("kept = %q, want get_service,observe_logs,search_code in order", got)
	}
	if removed := removedToolDefIDs(full, kept); strings.Join(removed, ",") != "search_config" {
		t.Fatalf("removed = %v, want search_config", removed)
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
	allowed := svc.prunedToolIDSet(snapshot, []string{"observe_logs"})

	for _, id := range []tool.ToolID{"get_service", "observe_logs"} {
		if _, ok := allowed[id]; !ok {
			t.Fatalf("allowed set missing %q", id)
		}
	}
	if _, ok := allowed["search_config"]; ok {
		t.Fatal("unrouted scenario tool must be pruned")
	}
}
