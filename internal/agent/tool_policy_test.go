package agent

import (
	"context"
	"strings"
	"testing"

	types "github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/llm"
	toolruntime "github.com/dekwanlabs/astris/tool"
)

func TestToolPolicyFiltersDefinitionsByKind(t *testing.T) {
	registry := testRegistry(t,
		testAgentTool("read", ToolKindRead, noopTool),
		testAgentTool("write", ToolKindWrite, noopTool),
	)
	executor := NewToolExecutor(registry)

	if defs := executor.DefinitionsFor(ToolPolicyForPlan(types.DirectPlan(), false)); strings.Join(toolDefNames(defs), ",") != "read" {
		t.Fatalf("direct definitions = %v, want read", toolDefNames(defs))
	}
	plan := types.EvidencePlan{Sources: types.Internal}
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
	snapshot := executor.Snapshot(ToolPolicyForPlan(types.EvidencePlan{Sources: types.Internal}, false))
	if err := registry.Register(testAgentTool("late", ToolKindRead, noopTool)); err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "1", Function: llm.ToolFunction{Name: "late", Arguments: `{}`}}
	result, _ := executor.Execute(context.Background(), snapshot, call, nil, nil)
	if !strings.Contains(result, "unknown tool") {
		t.Fatalf("result = %q, want pinned snapshot rejection", result)
	}
}

func TestSelectRoutedToolsHidesUnmatchedReadIntent(t *testing.T) {
	always := testAgentTool("always", ToolKindRead, noopTool)
	gated := testAgentTool("runtime", ToolKindRead, noopTool)
	gated.Routing = &toolruntime.RoutingSpec{Intent: "current runtime evidence"}
	write := testAgentTool("write", ToolKindWrite, noopTool)
	registry := testRegistry(t, always, gated, write)
	snapshot := registry.Snapshot(toolruntime.AllPolicy())

	filtered, _ := selectRoutedTools(snapshot, nil)
	if _, ok := filtered.Get("runtime"); ok {
		t.Fatal("unmatched routed read tool remained visible")
	}
	for _, id := range []toolruntime.ToolID{"always", "write"} {
		if _, ok := filtered.Get(id); !ok {
			t.Fatalf("tool %q was unexpectedly filtered", id)
		}
	}

	filtered, _ = selectRoutedTools(snapshot, []string{"runtime"})
	if _, ok := filtered.Get("runtime"); !ok {
		t.Fatal("matched routed read tool was not visible")
	}
}

func noopTool(context.Context, toolruntime.Arguments) (string, error) { return "ok", nil }

func testRegistry(t *testing.T, tools ...Tool) *Registry {
	t.Helper()
	registry := toolruntime.NewRegistry()
	if err := registry.RegisterAll(tools); err != nil {
		t.Fatal(err)
	}
	return registry
}

func testAgentTool(id toolruntime.ToolID, kind toolruntime.Kind, run func(context.Context, toolruntime.Arguments) (string, error)) Tool {
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
