package tools

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestSourceAdaptersDoNotFabricateEvidenceWithoutCanonicalProvenance(t *testing.T) {
	if units := symbolEvidenceUnits(map[string]any{
		"resolution": "ambiguous",
		"candidates": []any{map[string]any{"qualifiedName": "a::B"}},
	}); len(units) != 0 {
		t.Fatalf("ambiguous symbol evidence = %#v", units)
	}
	if units := dependencyEvidenceUnits(domain.DependencyTrace{
		Service:    "orders",
		Downstream: []domain.DependencyEdge{{From: "orders", To: "payments", Type: domain.EdgeHTTP}},
	}); len(units) != 0 {
		t.Fatalf("unproven dependency evidence = %#v", units)
	}
	if units := apiEvidenceUnits([]domain.EndpointRecord{{
		ServiceName: "orders", Method: "POST", Path: "/orders",
	}}); len(units) != 0 {
		t.Fatalf("unlocated API evidence = %#v", units)
	}
}

func TestCallChainEvidenceDeduplicatesCanonicalCodeIdentity(t *testing.T) {
	node := map[string]any{
		"qualifiedName": "orders::Place", "file": "repos/team/orders/place.go",
		"language": "go", "line": 10, "endLine": 20, "source": "func Place() {}",
	}
	result := map[string]any{
		"callers": map[string]any{
			"nodes": []map[string]any{node}, "truncated": false, "unresolved": []string{},
		},
		"callees": map[string]any{
			"nodes": []map[string]any{node}, "truncated": false, "unresolved": []string{},
		},
	}
	units := callChainEvidenceUnits(result)
	if len(units) != 1 || units[0].Target != "repos/team/orders/place.go" ||
		len(units[0].Sections) != 1 || units[0].Sections[0] != "L10-L20" ||
		!units[0].Coverage.Complete {
		t.Fatalf("call-chain evidence = %#v", units)
	}
	refs := callChainRefs(result)
	if len(refs) != 1 || refs[0].Type != tool.ReferenceSymbol || refs[0].Target != "orders::Place" {
		t.Fatalf("call-chain refs = %#v", refs)
	}
}

func TestWebSearchToolResultPublishesCanonicalFetchedEvidence(t *testing.T) {
	result, err := webSearchToolResult(WebSearchResponse{
		SourceStatus: WebSourceUsable,
		Fetched: &WebFetchedEvidence{
			URL: "https://developers.example.com/device-control", Title: "Device Control",
			Content: "bounded external documentation",
		},
	})
	if err != nil {
		t.Fatalf("webSearchToolResult() error = %v", err)
	}
	if len(result.EvidenceUnits) != 1 {
		t.Fatalf("evidence units = %#v", result.EvidenceUnits)
	}
	unit := result.EvidenceUnits[0]
	if unit.SourceKind != "web" || unit.Target != "https://developers.example.com/device-control" ||
		len(unit.Sections) != 1 || unit.Sections[0] != "Device Control" ||
		unit.ContentHash == "" || !unit.Coverage.Partial || unit.Coverage.Complete {
		t.Fatalf("web evidence unit = %#v", unit)
	}
	if !result.Coverage.Partial || len(result.Content) == 0 {
		t.Fatalf("tool result = %#v", result)
	}
}

func TestWebSearchToolResultKeepsSourceUnusableWithoutEvidence(t *testing.T) {
	result, err := webSearchToolResult(WebSearchResponse{
		SourceStatus: WebSourceUnusable,
		FetchNote:    "automatic fetch skipped: no relevant candidate",
	})
	if err != nil {
		t.Fatalf("webSearchToolResult() error = %v", err)
	}
	if len(result.EvidenceUnits) != 0 || !result.Coverage.Partial ||
		!strings.Contains(result.Content, `"source_status": "source_unusable"`) {
		t.Fatalf("tool result = %#v", result)
	}
}
