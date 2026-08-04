package agent

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/knowledge"
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

func TestMergeOutcomeReferencesUnionsRetrievedAndDynamic(t *testing.T) {
	retrieved := []retrieval.Reference{{Type: "code", Target: "pre.go"}, {Type: "runbook", Target: "doc-abc"}}
	dynamic := []tool.Reference{
		{Type: tool.ReferenceRunbook, Label: "design", Target: "doc-abc"}, // dedup vs retrieved
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

func TestCodeRefsNormalizesPathAndSkipsEmpty(t *testing.T) {
	refs := codeRefs([]knowledge.CodeSearchHit{
		{Path: "repos/a/x.go", StartLine: 12},
		{Path: ""},
	})
	if len(refs) != 1 {
		t.Fatalf("refs = %#v, want 1 (empty path skipped)", refs)
	}
	if refs[0].Type != tool.ReferenceCode || refs[0].Target != "repos/a/x.go" || refs[0].Label != "repos/a/x.go:L12" {
		t.Fatalf("ref = %#v", refs[0])
	}
}

func TestRunbookRefsUsesDocID(t *testing.T) {
	refs := runbookRefs([]knowledge.RunbookSearchHit{
		{DocID: "doc-abc", Title: "设计文档"},
		{DocID: ""},
	})
	if len(refs) != 1 {
		t.Fatalf("refs = %#v, want 1", refs)
	}
	if refs[0].Type != tool.ReferenceRunbook || refs[0].Target != "doc-abc" || refs[0].Label != "设计文档" {
		t.Fatalf("ref = %#v", refs[0])
	}
}
