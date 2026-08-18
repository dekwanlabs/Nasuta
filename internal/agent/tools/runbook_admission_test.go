package tools

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
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

func TestRunbookEvidenceUnitsUseStableChunkIdentity(t *testing.T) {
	result := knowledge.RunbookSearchResult{
		Matches: []knowledge.RunbookSearchHit{{
			DocID: "doc-a", DocKind: domain.DocKindFlow,
			EvidenceClass: domain.EvidenceClassCuratedRunbook,
			TrustTier:     domain.TrustCuratedRunbook,
			Chunks: []knowledge.RunbookChunk{
				{ChunkIndex: 1, SectionHeader: "Overview", ChunkText: "a", SemanticScore: 0.5},
				{ChunkIndex: 2, SectionHeader: "Overview", ChunkText: "b", SemanticScore: 0.4},
			},
		}},
	}
	units := runbookEvidenceUnits(result)
	if len(units) != 2 || units[0].Target != "doc-a" || units[1].Target != "doc-a" {
		t.Fatalf("units = %#v", units)
	}
	if units[0].Sections[0] != "chunk:1" || units[1].Sections[0] != "chunk:2" {
		t.Fatalf("sections = %#v, %#v", units[0].Sections, units[1].Sections)
	}
	if units[0].Coverage.Complete || !units[0].Coverage.Partial || units[0].Coverage.Included != 1 {
		t.Fatalf("coverage = %#v, want one partial chunk", units[0].Coverage)
	}

	variant := result
	variant.Matches = []knowledge.RunbookSearchHit{{
		DocID: "doc-a", Title: "query-specific title", DocKind: domain.DocKindFlow,
		EvidenceClass: domain.EvidenceClassCuratedRunbook,
		TrustTier:     domain.TrustCuratedRunbook,
		Chunks: []knowledge.RunbookChunk{{
			ChunkIndex: 1, SectionHeader: "renamed presentation header",
			ChunkText: "a", SemanticScore: 0.99,
		}},
	}}
	ledger := evidence.New(units, "first-query")
	if conflicts := ledger.Add(runbookEvidenceUnits(variant), "second-query"); len(conflicts) != 0 {
		t.Fatalf("retrieval variant conflicts = %#v", conflicts)
	}

	variant.Matches[0].Chunks[0].ChunkText = "changed authoritative content"
	if conflicts := ledger.Add(runbookEvidenceUnits(variant), "changed-source"); len(conflicts) != 1 {
		t.Fatalf("changed chunk conflicts = %#v, want one", conflicts)
	}
}
