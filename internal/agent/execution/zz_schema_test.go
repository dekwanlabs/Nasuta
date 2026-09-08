package execution

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestFallbackReportSchemaValid(t *testing.T) {
	units := []tool.EvidenceUnit{
		{SourceKind: "code", Target: "svc-a/handler.go", Sections: []string{"main"}, Facets: []string{"core_flow"}},
		{SourceKind: "dependency", Target: "svc-a", Sections: []string{"outbound:svc-a->svc-b:http"}},
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
