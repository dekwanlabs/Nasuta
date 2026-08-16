package domain

import "testing"

func TestResolveQueryPlanPreservesClassificationPriority(t *testing.T) {
	tests := []struct {
		name string
		q    string
		want QueryKind
	}{
		{"en error", "why does the service error out", QueryRuntimeDiagnosis},
		{"cn 报错", "欧区服务半夜报错", QueryRuntimeDiagnosis},
		{"pasted trace id", "trace id: 12345678-1234-1234-1234-1234567890ab", QueryRuntimeDiagnosis},
		{"kibana url", "https://kibana.example.com/app/discover", QueryRuntimeDiagnosis},
		{"cn 能不能", "能不能加重试", QueryInventory},
		{"en implement", "how to implement a retry", QueryInventory},
		{"cn 架构", "为什么用这个架构", QueryOverview},
		{"en tradeoff", "tradeoff between consistency and availability", QueryOverview},
		{"cn 这段代码", "这段代码有什么问题", QueryCodeReview},
		{"en refactor", "should I refactor this method", QueryCodeReview},
		{"general", "这个服务是干什么的", QueryFocusedFact},
		{"empty", "", QueryFocusedFact},
		{"bug+req", "报错了能不能加个重试", QueryRuntimeDiagnosis},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ResolveQueryPlan(test.q, QuerySignals{}).Plan.Kind
			if got != test.want {
				t.Fatalf("ResolveQueryPlan(%q) = %q, want %q", test.q, got, test.want)
			}
		})
	}
}

func TestResolveQueryPlanPrioritizesComparisonThenFlow(t *testing.T) {
	comparison := ResolveQueryPlan(
		"分别比较两个设备控制链路的业务、实现和依赖有什么差异",
		QuerySignals{Identifiers: []string{"Domestic.Control", "Overseas.Control"}},
	)
	if comparison.Plan.Kind != QueryComparison {
		t.Fatalf("comparison kind = %q, want %q", comparison.Plan.Kind, QueryComparison)
	}
	if len(comparison.Plan.Entities) != 2 {
		t.Fatalf("comparison entities = %v", comparison.Plan.Entities)
	}
	if comparison.MatchedRuleKind != QueryComparison {
		t.Fatalf("comparison matched rule = %q, want %q", comparison.MatchedRuleKind, QueryComparison)
	}

	flow := ResolveQueryPlan(
		"这个方法的调用链是什么",
		QuerySignals{Identifiers: []string{"PaymentHandler.handle"}},
	)
	if flow.Plan.Kind != QueryFlow {
		t.Fatalf("flow kind = %q, want %q", flow.Plan.Kind, QueryFlow)
	}
	if len(flow.Plan.Entities) != 1 || flow.Plan.Entities[0] != "paymenthandler.handle" {
		t.Fatalf("flow entities = %v", flow.Plan.Entities)
	}
	if flow.MatchedRuleKind != QueryFlow {
		t.Fatalf("flow matched rule = %q, want %q", flow.MatchedRuleKind, QueryFlow)
	}
}

func TestResolveQueryPlanDoesNotTreatEveryIdentifierAsFlow(t *testing.T) {
	resolution := ResolveQueryPlan(
		"PaymentHandler 这个类负责什么",
		QuerySignals{Identifiers: []string{"PaymentHandler"}},
	)
	if resolution.Plan.Kind != QueryFocusedFact {
		t.Fatalf("kind = %q, want %q", resolution.Plan.Kind, QueryFocusedFact)
	}
	if resolution.Origin != QueryResolutionFallback {
		t.Fatalf("origin = %q, want %q", resolution.Origin, QueryResolutionFallback)
	}
	if resolution.MatchedRuleKind != "" {
		t.Fatalf("matched rule = %q, want empty fallback diagnostic", resolution.MatchedRuleKind)
	}
}

func TestResolveQueryPlanBoundsEntities(t *testing.T) {
	resolution := ResolveQueryPlan(
		"这些符号分别做什么",
		QuerySignals{Identifiers: []string{"A", "a", "B", "C", "D", "E", "F", "G", "H", "I", "J"}},
	)
	if len(resolution.Plan.Entities) != MaxCanonicalEntities {
		t.Fatalf("entities = %v, want %d unique bounded entries", resolution.Plan.Entities, MaxCanonicalEntities)
	}
}

func TestRequiredFacetsForReturnsStableCopies(t *testing.T) {
	want := []EvidenceFacet{
		FacetBusinessDomain,
		FacetCoreFlow,
		FacetDataAndState,
		FacetExternalDependency,
	}
	got := RequiredFacetsFor(QueryComparison)
	if len(got) != len(want) {
		t.Fatalf("comparison facets = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("comparison facets = %v, want %v", got, want)
		}
	}
	got[0] = FacetEntrypoint
	if RequiredFacetsFor(QueryComparison)[0] != FacetBusinessDomain {
		t.Fatal("RequiredFacetsFor returned mutable catalog storage")
	}
}

func TestFacetCatalogAndQueryDefaultsAreValid(t *testing.T) {
	catalog := FacetCatalog()
	if len(catalog) != 7 {
		t.Fatalf("facet catalog size = %d, want 7", len(catalog))
	}
	for _, kind := range []QueryKind{
		QueryFocusedFact,
		QueryOverview,
		QueryFlow,
		QueryComparison,
		QueryInventory,
		QueryRuntimeDiagnosis,
		QueryCodeReview,
	} {
		if err := ValidateFacets(RequiredFacetsFor(kind)); err != nil {
			t.Fatalf("facets for %q: %v", kind, err)
		}
	}
}

func TestProvidedFacetsForUsesCanonicalConservativeCoverage(t *testing.T) {
	for _, test := range []struct {
		source string
		kind   string
		want   []EvidenceFacet
	}{
		{source: "code", want: []EvidenceFacet{FacetEntrypoint, FacetCoreFlow, FacetDataAndState}},
		{source: "codegraph", want: []EvidenceFacet{FacetCoreFlow}},
		{source: "dependency", want: []EvidenceFacet{FacetExternalDependency}},
		{source: "runtime", want: []EvidenceFacet{FacetRuntimeOperations}},
		{source: "runbook", kind: DocKindSchema, want: []EvidenceFacet{FacetDataAndState}},
	} {
		got := ProvidedFacetsFor(test.source, test.kind)
		if len(got) != len(test.want) {
			t.Fatalf("ProvidedFacetsFor(%q, %q) = %v, want %v", test.source, test.kind, got, test.want)
		}
		for i := range test.want {
			if got[i] != test.want[i] {
				t.Fatalf("ProvidedFacetsFor(%q, %q) = %v, want %v", test.source, test.kind, got, test.want)
			}
		}
		if err := ValidateFacets(got); err != nil {
			t.Fatalf("ProvidedFacetsFor(%q, %q): %v", test.source, test.kind, err)
		}
	}
	for _, facet := range ProvidedFacetsFor("code", "") {
		if facet == FacetRuntimeOperations {
			t.Fatal("code evidence must not imply runtime coverage")
		}
	}
}

func TestFacetCatalogReturnsCopyAndRejectsInvalidValues(t *testing.T) {
	catalog := FacetCatalog()
	catalog[0].ID = "mutated"
	if FacetCatalog()[0].ID != FacetSystemBoundary {
		t.Fatal("FacetCatalog returned mutable catalog storage")
	}
	if err := ValidateFacets([]EvidenceFacet{"unknown"}); err == nil {
		t.Fatal("ValidateFacets accepted an unknown facet")
	}
	if err := ValidateFacets([]EvidenceFacet{FacetCoreFlow, FacetCoreFlow}); err == nil {
		t.Fatal("ValidateFacets accepted a duplicate facet")
	}
}

func TestEveryRequiredFacetHasEvidenceProvider(t *testing.T) {
	provided := make(map[EvidenceFacet]struct{})
	for _, evidenceKind := range []struct {
		source string
		kind   string
	}{
		{source: "service"},
		{source: "dependency"},
		{source: "code"},
		{source: "codegraph"},
		{source: "runtime"},
		{source: "runbook", kind: DocKindFlow},
		{source: "runbook", kind: DocKindSchema},
		{source: "runbook", kind: DocKindModule},
	} {
		for _, facet := range ProvidedFacetsFor(evidenceKind.source, evidenceKind.kind) {
			provided[facet] = struct{}{}
		}
	}
	for _, kind := range []QueryKind{
		QueryFocusedFact,
		QueryOverview,
		QueryFlow,
		QueryComparison,
		QueryInventory,
		QueryRuntimeDiagnosis,
		QueryCodeReview,
	} {
		for _, facet := range RequiredFacetsFor(kind) {
			if _, ok := provided[facet]; !ok {
				t.Fatalf("query kind %q requires facet %q without an evidence provider", kind, facet)
			}
		}
	}
}
