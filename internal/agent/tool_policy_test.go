package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestToolPolicyFiltersDefinitionsByKind(t *testing.T) {
	registry := testRegistry(t,
		testAgentTool("read", ToolKindRead, noopTool),
		testAgentTool("write", ToolKindWrite, noopTool),
	)
	executor := NewToolExecutor(registry)

	if defs := executor.DefinitionsFor(ToolPolicyForPlan(domain.DirectPlan(), false)); strings.Join(toolDefNames(defs), ",") != "read" {
		t.Fatalf("direct definitions = %v, want read", toolDefNames(defs))
	}
	plan := domain.EvidencePlan{Sources: domain.Internal}
	defs := executor.DefinitionsFor(ToolPolicyForPlan(plan, false))
	if got := strings.Join(toolDefNames(defs), ","); got != "read" {
		t.Fatalf("definitions = %q, want read", got)
	}
	defs = executor.DefinitionsFor(ToolPolicyForPlan(plan, true))
	if got := strings.Join(toolDefNames(defs), ","); got != "read,write" {
		t.Fatalf("definitions with write = %q, want read,write", got)
	}
}

func TestToolSnapshotBlocksToolRegisteredMidRun(t *testing.T) {
	registry := testRegistry(t, testAgentTool("read", ToolKindRead, noopTool))
	executor := NewToolExecutor(registry)
	snapshot := executor.Snapshot(ToolPolicyForPlan(domain.EvidencePlan{Sources: domain.Internal}, false))
	if err := registry.Register(testAgentTool("late", ToolKindRead, noopTool)); err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "1", Function: llm.ToolFunction{Name: "late", Arguments: `{}`}}
	result := executor.Execute(context.Background(), snapshot, call, nil, nil)
	if !strings.Contains(result.FullContent, "unknown tool") {
		t.Fatalf("result = %q, want pinned snapshot rejection", result.FullContent)
	}
}

func TestSelectRoutedToolsHidesUnmatchedReadIntent(t *testing.T) {
	always := testAgentTool("always", ToolKindRead, noopTool)
	gated := testAgentTool("runtime", ToolKindRead, noopTool)
	gated.Routing = &tool.RoutingSpec{Intent: "current runtime evidence"}
	write := testAgentTool("write", ToolKindWrite, noopTool)
	registry := testRegistry(t, always, gated, write)
	snapshot := registry.Snapshot(tool.AllPolicy())

	filtered, _ := selectRoutedTools(snapshot, nil)
	if _, ok := filtered.Get("runtime"); ok {
		t.Fatal("unmatched routed read tool remained visible")
	}
	for _, id := range []tool.ToolID{"always", "write"} {
		if _, ok := filtered.Get(id); !ok {
			t.Fatalf("tool %q was unexpectedly filtered", id)
		}
	}

	filtered, _ = selectRoutedTools(snapshot, []string{"runtime"})
	if _, ok := filtered.Get("runtime"); !ok {
		t.Fatal("matched routed read tool was not visible")
	}
}

func TestContextualRoutedToolIDsRetainsCandidatesForInternalFollowUp(t *testing.T) {
	candidates := []retrieval.ToolRouteCandidate{{ID: "runtime", Intent: "current runtime evidence"}}
	internalPlan := domain.EvidencePlan{Sources: domain.Internal}
	selected, retained := contextualRoutedToolIDs(nil, candidates, "prior runtime investigation", internalPlan)
	if !retained || len(selected) != 1 || selected[0] != "runtime" {
		t.Fatalf("selected=%v retained=%v", selected, retained)
	}
	selected, retained = contextualRoutedToolIDs(nil, candidates, "prior runtime investigation", domain.DirectPlan())
	if retained || len(selected) != 0 {
		t.Fatalf("direct selected=%v retained=%v", selected, retained)
	}
	selected, retained = contextualRoutedToolIDs([]string{"runtime"}, candidates, "context", internalPlan)
	if retained || len(selected) != 1 {
		t.Fatalf("existing selection=%v retained=%v", selected, retained)
	}
}

func noopTool(context.Context, tool.Arguments) (string, error) { return "ok", nil }

func testRegistry(t *testing.T, tools ...Tool) *Registry {
	t.Helper()
	registry := tool.NewRegistry()
	if err := registry.RegisterAll(tools); err != nil {
		t.Fatal(err)
	}
	return registry
}

func testAgentTool(id tool.ToolID, kind tool.Kind, run func(context.Context, tool.Arguments) (string, error)) Tool {
	return Tool{
		ID:          id,
		Description: "test tool",
		Kind:        kind,
		InputSchema: objectSchema(map[string]any{}, nil),
		Handler:     stringHandler(run),
	}
}

func toolDefNames(defs []llm.ToolDef) []string {
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		names = append(names, def.Function.Name)
	}
	return names
}
