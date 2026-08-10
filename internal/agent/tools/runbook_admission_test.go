package tools

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestRunbookAdmissionScopeAndBound(t *testing.T) {
	spec := runbookAdmissionSpec()
	scope, err := spec.ResolveScope(tool.Arguments{
		"query": "architecture", "doc_id": "doc-a", "limit": float64(3),
	})
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if scope.SourceKind != "runbook" || scope.Target != "doc-a" {
		t.Fatalf("scope = %#v", scope)
	}
	if got := spec.MaxResultTokens(tool.Arguments{"limit": float64(3)}); got != 5656 {
		t.Fatalf("max tokens = %d, want 5656", got)
	}
	narrowed, changed := spec.Narrow(tool.Arguments{"limit": float64(5)}, 3856)
	if !changed || narrowed.Int("limit", 0) != 2 {
		t.Fatalf("narrowed = %#v changed=%t", narrowed, changed)
	}
}

func TestRunbookEvidenceUnitsKeepCanonicalDocumentAndSections(t *testing.T) {
	units := runbookEvidenceUnits(knowledge.RunbookSearchResult{
		Matches: []knowledge.RunbookSearchHit{{
			DocID: "doc-a", DocKind: domain.DocKindFlow,
			EvidenceClass: domain.EvidenceClassCuratedRunbook,
			TrustTier:     domain.TrustCuratedRunbook,
			Chunks: []knowledge.RunbookChunk{
				{ChunkIndex: 1, SectionHeader: "Overview", ChunkText: "a"},
				{ChunkIndex: 2, ChunkText: "b"},
			},
		}},
	})
	if len(units) != 1 || units[0].Target != "doc-a" {
		t.Fatalf("units = %#v", units)
	}
	if len(units[0].Sections) != 2 || units[0].Sections[0] != "Overview" || units[0].Sections[1] != "chunk:2" {
		t.Fatalf("sections = %#v", units[0].Sections)
	}
	if units[0].Coverage.Complete || !units[0].Coverage.Partial {
		t.Fatalf("coverage = %#v, want partial document scope", units[0].Coverage)
	}
}
