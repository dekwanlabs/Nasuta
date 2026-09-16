package execution

import (
	"encoding/json"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestFallbackReportSchemaValid(t *testing.T) {
	units := []tool.EvidenceUnit{
		{SourceKind: "code", Target: "svc-a/handler.go", Sections: []string{"main"}, Facets: []string{"core_flow"}},
		{SourceKind: "dependency", Target: "svc-a", Sections: []string{"outbound:svc-a->svc-b:http|Client.call"}},
	}
	report, ok := BuildEvidencePreservingReport(units, nil, []string{"core_flow", "external_dependency"}, "code")
	if !ok {
		t.Fatalf("BuildEvidencePreservingReport returned ok=false")
	}
	reg := agentapi.NewSchemaRegistry()
	if err := reg.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Validate(agentapi.InvestigationReportSchemaRef(), report); err != nil {
		t.Fatalf("fallback report invalid: %v\n%s", err, string(report))
	}
}

// A fallback report preserves findings and gaps but never manufactures a flow
// from raw dependency edges — even when those edges carry call sites. A flat
// dependency graph is not the subject's flow; emitting one reads as a
// misleading trace_deps rendering.
func TestFallbackReportDoesNotManufactureFlow(t *testing.T) {
	units := []tool.EvidenceUnit{
		{SourceKind: "code", Target: "svc-a/handler.go", Sections: []string{"main"}, Facets: []string{"core_flow"}},
		{SourceKind: "dependency", Target: "svc-a", Sections: []string{"outbound:svc-a->svc-b:http|Client.call"}},
		{SourceKind: "dependency", Target: "svc-b", Sections: []string{"outbound:svc-b->svc-c:feign|Worker.run"}},
	}
	report, ok := BuildEvidencePreservingReport(units, nil, []string{"core_flow"}, "code")
	if !ok {
		t.Fatalf("BuildEvidencePreservingReport returned ok=false")
	}
	var decoded map[string]any
	if err := json.Unmarshal(report, &decoded); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if _, hasFlow := decoded["flow"]; hasFlow {
		t.Fatalf("fallback report manufactured a flow from dependency edges: %v", decoded["flow"])
	}
	if len(decoded["findings"].([]any)) == 0 {
		t.Fatal("fallback report dropped its findings along with the flow")
	}
}
