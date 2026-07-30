package agent

import (
	"context"
	"encoding/json"
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
	result := executor.Execute(context.Background(), snapshot, call, nil, nil, nil)
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

func TestReferenceMismatchEnforcesDeclaredToolBoundaries(t *testing.T) {
	references := map[string]tool.ReferenceType{
		"flow-system-overview": tool.ReferenceRunbook,
		"hsds-base-system":     tool.ReferenceService,
		"NormalizeCommand":     tool.ReferenceSymbol,
	}
	runbookTool := referenceTestTool("search_runbooks", "doc_id", tool.ReferenceRunbook)
	codeTool := referenceTestTool("search_code", "query", tool.ReferenceService, tool.ReferenceSymbol)
	docsTool := referenceTestTool("check_docs", "service", tool.ReferenceService)
	symbolTool := referenceTestTool("get_symbol", "query", tool.ReferenceSymbol)
	traceTool := referenceTestTool("trace_calls", "query", tool.ReferenceSymbol)
	registry := testRegistry(t, runbookTool, codeTool, docsTool, symbolTool, traceTool)
	snapshot := registry.Snapshot(tool.ReadPolicy())

	tests := []struct {
		name      string
		candidate tool.Tool
		args      tool.Arguments
		wantCode  string
	}{
		{"runbook accepted by runbook search", runbookTool, tool.Arguments{"doc_id": "flow-system-overview"}, ""},
		{"runbook rejected by code search", codeTool, tool.Arguments{"query": "flow-system-overview architecture"}, "entity_type_mismatch"},
		{"runbook rejected by docs check", docsTool, tool.Arguments{"service": "flow-system-overview"}, "entity_type_mismatch"},
		{"runbook rejected by symbol lookup", symbolTool, tool.Arguments{"query": "flow-system-overview"}, "entity_type_mismatch"},
		{"service accepted by docs check", docsTool, tool.Arguments{"service": "hsds-base-system"}, ""},
		{"symbol accepted by symbol lookup", symbolTool, tool.Arguments{"query": "NormalizeCommand"}, ""},
		{"symbol accepted by call trace", traceTool, tool.Arguments{"query": "NormalizeCommand"}, ""},
		{"unknown free query allowed", codeTool, tool.Arguments{"query": "command normalization"}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := referenceMismatch(snapshot, test.candidate, test.args, references)
			if test.wantCode == "" {
				if got != "" {
					t.Fatalf("mismatch = %s, want none", got)
				}
				return
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(got), &payload); err != nil {
				t.Fatalf("decode mismatch: %v", err)
			}
			if payload["code"] != test.wantCode {
				t.Fatalf("code = %v, want %s", payload["code"], test.wantCode)
			}
		})
	}
}

func TestReferenceMismatchUsesCompleteTokenBoundaries(t *testing.T) {
	references := map[string]tool.ReferenceType{"flow-system-overview": tool.ReferenceRunbook}
	candidate := referenceTestTool("search_code", "query", tool.ReferenceService)
	snapshot := testRegistry(t,
		candidate,
		referenceTestTool("search_runbooks", "doc_id", tool.ReferenceRunbook),
	).Snapshot(tool.ReadPolicy())

	for _, query := range []string{"prefix-flow-system-overview", "flow-system-overview-suffix", "xflow-system-overview"} {
		if got := referenceMismatch(snapshot, candidate, tool.Arguments{"query": query}, references); got != "" {
			t.Fatalf("query %q matched a partial token: %s", query, got)
		}
	}
	if got := referenceMismatch(snapshot, candidate, tool.Arguments{"query": "(flow-system-overview)"}, references); !strings.Contains(got, "entity_type_mismatch") {
		t.Fatalf("complete token was not rejected: %s", got)
	}
}

func referenceTestTool(id tool.ToolID, argument string, accepts ...tool.ReferenceType) Tool {
	return Tool{
		ID: id, Description: "test reference tool", Kind: ToolKindRead,
		InputSchema:     objectSchema(map[string]any{argument: propString("reference")}, nil),
		ReferenceInputs: []tool.ReferenceInput{{Argument: argument, Accepts: accepts}},
		Handler:         stringHandler(noopTool),
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
