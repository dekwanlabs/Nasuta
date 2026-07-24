package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
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

func TestRoutingMetadataDoesNotChangeSnapshotVisibility(t *testing.T) {
	always := testAgentTool("always", ToolKindRead, noopTool)
	gated := testAgentTool("runtime", ToolKindRead, noopTool)
	gated.Routing = &tool.RoutingSpec{Intent: "current runtime evidence"}
	write := testAgentTool("write", ToolKindWrite, noopTool)
	registry := testRegistry(t, always, gated, write)
	snapshot := registry.Snapshot(ToolPolicyForPlan(domain.DirectPlan(), false))

	for _, id := range []tool.ToolID{"always", "runtime"} {
		if _, ok := snapshot.Get(id); !ok {
			t.Fatalf("read tool %q was unexpectedly hidden", id)
		}
	}
	if _, ok := snapshot.Get("write"); ok {
		t.Fatal("write tool was visible without write permission")
	}
}

func TestPreferredToolsInstructionIsAdvisory(t *testing.T) {
	instruction := preferredToolsInstruction([]string{"runtime"})
	for _, want := range []string{"runtime", "advisory, not mandatory", "answer directly", "Other registered tools remain available"} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("instruction missing %q: %s", want, instruction)
		}
	}
	if strings.Contains(instruction, "must call") || strings.Contains(instruction, "required") {
		t.Fatalf("preference was expressed as a requirement: %s", instruction)
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
