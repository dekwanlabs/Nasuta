package retrieval

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestSelectOverviewEvidenceStartsWithAuthoritySpineAndStopsWithoutIncrement(t *testing.T) {
	unit := func(source, target string, trust int, facets ...domain.EvidenceFacet) tool.EvidenceUnit {
		values := make([]string, len(facets))
		for i, facet := range facets {
			values[i] = string(facet)
		}
		return tool.EvidenceUnit{
			SourceKind: source, Target: target, TrustTier: trust, Facets: values,
		}
	}
	parts := []partial{
		{text: "runtime", score: 0.95, units: []tool.EvidenceUnit{
			unit("code", "runtime.go", domain.TrustCodeRuntime, domain.FacetEntrypoint, domain.FacetRuntimeOperations),
		}},
		{text: "duplicate flow", score: 0.99, units: []tool.EvidenceUnit{
			unit("runbook", "flow-local", domain.TrustGeneratedDoc, domain.FacetSystemBoundary, domain.FacetCoreFlow),
		}},
		{text: "authority spine", score: 0.75, units: []tool.EvidenceUnit{
			unit("runbook", "flow-authority", domain.TrustCuratedRunbook, domain.FacetSystemBoundary, domain.FacetCoreFlow),
		}},
		{text: "schema", score: 0.80, units: []tool.EvidenceUnit{
			unit("runbook", "schema", domain.TrustCuratedSchema, domain.FacetDataAndState),
		}},
	}
	selected := selectOverviewEvidence(parts, []domain.EvidenceFacet{
		domain.FacetSystemBoundary,
		domain.FacetCoreFlow,
		domain.FacetEntrypoint,
		domain.FacetRuntimeOperations,
		domain.FacetDataAndState,
	})
	if len(selected) != 3 {
		t.Fatalf("selected = %d, want 3 incremental parts", len(selected))
	}
	if selected[0].text != "authority spine" {
		t.Fatalf("first = %q, want authority spine", selected[0].text)
	}
	for _, part := range selected {
		if part.text == "duplicate flow" {
			t.Fatal("lower-authority evidence with no new facet was selected")
		}
	}
}

func TestAssemblePreservesOnlyIncludedEvidenceUnits(t *testing.T) {
	retrieve := &Retriever{}
	parts := []partial{
		{
			text: "included", priority: partialPriorityEvidence,
			units: []tool.EvidenceUnit{{
				SourceKind: "runbook", Target: "doc-a",
				Coverage: tool.EvidenceCoverage{Complete: true},
			}},
		},
		{
			text: "", priority: partialPriorityService,
			units: []tool.EvidenceUnit{{SourceKind: "service", Target: "svc-b"}},
		},
	}
	result := retrieve.assemble(t.Context(), parts, nil, "query")
	if len(result.EvidenceUnits) != 1 || result.EvidenceUnits[0].Target != "doc-a" {
		t.Fatalf("evidence units = %#v", result.EvidenceUnits)
	}
}

func TestAssembleHashesDeliveredCleanEvidence(t *testing.T) {
	retrieve := &Retriever{}
	retrieve.serviceModules.Store([]domain.ServiceRecord{{
		ServiceName: "svc-a",
		Repo:        "repo-a",
	}})
	result := retrieve.assemble(t.Context(), []partial{{
		text: "repos/repo-a/file.go",
		units: []tool.EvidenceUnit{{
			SourceKind: "code", Target: "repos/repo-a/file.go",
		}},
	}}, nil, "query")
	if result.Text != "svc-a/file.go\n" {
		t.Fatalf("text = %q", result.Text)
	}
	if len(result.EvidenceUnits) != 1 ||
		result.EvidenceUnits[0].ContentHash != evidenceHash("svc-a/file.go") {
		t.Fatalf("evidence units = %#v", result.EvidenceUnits)
	}
}
