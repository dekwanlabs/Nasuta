package execution

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestPrunedDefinitionsPreservesOrderAndMembership(t *testing.T) {
	full := []llm.ToolDef{
		{Function: llm.ToolFunctionDef{Name: "get_service"}},
		{Function: llm.ToolFunctionDef{Name: "observe_logs"}},
		{Function: llm.ToolFunctionDef{Name: "search_code"}},
		{Function: llm.ToolFunctionDef{Name: "search_config"}},
	}
	allowed := map[tool.ToolID]struct{}{
		"get_service":  {},
		"search_code":  {},
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

func TestPrepareToolDefinitionsHonorsAppliedEmptyToolSet(t *testing.T) {
	registry := testRegistry(t,
		testAgentTool("inspect_service", ToolKindRead, noopTool),
		testAgentTool("inspect_runbook", ToolKindRead, noopTool),
	)
	executor := NewToolExecutor(registry)
	agent := NewAgent(nil, executor, AgentConfig{}, nil, nil)
	snapshot := executor.Snapshot(ToolPolicyForRun(false))

	pruned := agent.prepareToolDefinitions(t.Context(), "run-pruned-empty", Input{
		OfferedToolIDs:     map[tool.ToolID]struct{}{},
		ToolPruningApplied: true,
	}, snapshot)
	if len(pruned) != 0 {
		t.Fatalf("applied empty tool set restored definitions: %v", toolDefNames(pruned))
	}

	unpruned := agent.prepareToolDefinitions(t.Context(), "run-unpruned-empty", Input{
		OfferedToolIDs: map[tool.ToolID]struct{}{},
	}, snapshot)
	if got := strings.Join(toolDefNames(unpruned), ","); got != "inspect_service,inspect_runbook" {
		t.Fatalf("unpruned definitions = %q", got)
	}
}
