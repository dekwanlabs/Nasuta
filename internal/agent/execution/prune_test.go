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
