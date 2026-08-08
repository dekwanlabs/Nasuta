package execution

import (
	"testing"

	"github.com/dekwanlabs/nasuta/tool"
)

func TestMergeToolReferencesDedupsByTypeAndTarget(t *testing.T) {
	var dst []tool.Reference
	mergeToolReferences(&dst, []tool.Reference{
		{Type: tool.ReferenceCode, Label: "a:L1", Target: "a.go"},
		{Type: tool.ReferenceRunbook, Label: "doc", Target: "doc-abc"},
		{Type: tool.ReferenceCode, Label: "a:L2", Target: "a.go"}, // same target, dedup
	})
	if len(dst) != 2 {
		t.Fatalf("references = %#v, want 2 unique", dst)
	}
	mergeToolReferences(&dst, []tool.Reference{{Type: tool.ReferenceCode, Target: "b.go"}})
	if len(dst) != 3 {
		t.Fatalf("after merge references = %#v, want 3", dst)
	}
}
