package qa

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestMergeOutcomeReferencesUnionsRetrievedAndDynamic(t *testing.T) {
	retrieved := []agentapi.Reference{{Type: "code", Target: "pre.go"}, {Type: "runbook", Target: "doc-abc"}}
	dynamic := []tool.Reference{
		{Type: tool.ReferenceRunbook, Label: "design", Target: "doc-abc"},
		{Type: tool.ReferenceCode, Target: "rules.yaml"},
	}
	merged := mergeOutcomeReferences(retrieved, dynamic)
	if len(merged) != 3 {
		t.Fatalf("merged = %#v, want 3", merged)
	}
	if merged[0].Target != "pre.go" || merged[1].Target != "doc-abc" || merged[2].Target != "rules.yaml" {
		t.Fatalf("merged order = %#v, want pre.go, doc-abc, rules.yaml", merged)
	}
}
