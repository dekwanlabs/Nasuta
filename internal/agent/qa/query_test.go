package qa

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

func TestQueryAnalysisTraceCarriesDerivedDiagnostics(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := runtrace.WithScope(t.Context(), runtrace.NewScope(runtrace.Evaluation, func(event domain.EvaluationTrace) {
		events = append(events, event)
	}))
	analysis, err := analyzeQuery(ctx, queryAnalysisInput{
		Question:      "分别比较 Domestic.Control 和 Overseas.Control 的差异",
		CleanQuestion: "分别比较 Domestic.Control 和 Overseas.Control 的差异",
		Terms: retrieval.QueryTerms{
			Identifiers: []string{"Domestic.Control", "Overseas.Control"},
			DomainTerms: []string{"比较"},
		},
	})
	if err != nil {
		t.Fatalf("analyzeQuery: %v", err)
	}
	if analysis.QueryPlan.Kind != domain.QueryComparison {
		t.Fatalf("query kind = %q, want %q", analysis.QueryPlan.Kind, domain.QueryComparison)
	}
	if len(events) != 1 || events[0].Node != "query_analysis" {
		t.Fatalf("events = %#v", events)
	}
	output := events[0].Output
	if output["query_kind"] != domain.QueryComparison ||
		output["resolution_origin"] != domain.QueryResolutionRule ||
		output["matched_rule_kind"] != domain.QueryComparison ||
		output["entity_count"] != 2 {
		t.Fatalf("query analysis trace = %#v", output)
	}
	facets, ok := output["required_facets"].([]string)
	if !ok || len(facets) != len(domain.RequiredFacetsFor(domain.QueryComparison)) {
		t.Fatalf("required facets = %#v", output["required_facets"])
	}
}
