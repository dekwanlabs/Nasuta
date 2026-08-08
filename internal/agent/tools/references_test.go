package tools

import (
	"testing"

	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/tool"
)

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
