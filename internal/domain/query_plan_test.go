package domain

import "testing"

func TestResolveQueryPlanUsesPlannerSemantics(t *testing.T) {
	semantics := &QuerySemantics{Kind: QueryFlow}
	resolution := ResolveQueryPlan(
		"任意自然语言表述",
		semantics,
		[]string{"PaymentHandler.handle"},
	)
	if resolution.Plan.Kind != QueryFlow || resolution.Origin != QueryResolutionPlanner {
		t.Fatalf("resolution = %+v", resolution)
	}
	if len(resolution.Plan.Entities) != 1 || resolution.Plan.Entities[0] != "paymenthandler.handle" {
		t.Fatalf("entities = %v", resolution.Plan.Entities)
	}
	if resolution.MatchedRuleKind != "" {
		t.Fatalf("matched rule = %q, want empty", resolution.MatchedRuleKind)
	}
}

func TestResolveQueryPlanPreservesNonCanonicalIdentifierForEntityMatching(t *testing.T) {
	resolution := ResolveQueryPlan(
		"比较系统",
		&QuerySemantics{Kind: QueryComparison},
		[]string{"本系统ai集成"},
	)
	if len(resolution.Plan.Entities) != 1 ||
		resolution.Plan.Entities[0] != "entity_75cbe4e1e8cee1d5879f90a9f477396b94d02f27a61407c4230618ddd2d16869" {
		t.Fatalf("entities = %#v", resolution.Plan.Entities)
	}
	if len(resolution.Plan.EntitySpecs) != 1 ||
		resolution.Plan.EntitySpecs[0].Label != "本系统ai集成" {
		t.Fatalf("entity specs = %#v", resolution.Plan.EntitySpecs)
	}
}

func TestResolveQueryPlanTypedRuntimeLocatorOverridesPlanner(t *testing.T) {
	for _, question := range []string{
		"trace_id: 12345678-1234-1234-1234-1234567890ab",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"https://kibana.example.com/app/discover#/?trace=abc",
	} {
		resolution := ResolveQueryPlan(question, &QuerySemantics{Kind: QueryOverview}, nil)
		if resolution.Plan.Kind != QueryRuntimeDiagnosis ||
			resolution.Origin != QueryResolutionRule ||
			resolution.MatchedRuleKind != QueryRuntimeDiagnosis {
			t.Fatalf("ResolveQueryPlan(%q) = %+v", question, resolution)
		}
	}
}

func TestResolveQueryPlanDoesNotTreatBareUUIDAsRuntimeLocator(t *testing.T) {
	resolution := ResolveQueryPlan(
		"12345678-1234-1234-1234-1234567890ab",
		&QuerySemantics{Kind: QueryFocusedFact},
		nil,
	)
	if resolution.Plan.Kind != QueryFocusedFact || resolution.Origin != QueryResolutionPlanner {
		t.Fatalf("resolution = %+v", resolution)
	}
}

func TestResolveQueryPlanFallsBackWithoutPlannerSemantics(t *testing.T) {
	resolution := ResolveQueryPlan("这个服务如何一路完成状态变更", nil, nil)
	if resolution.Plan.Kind != QueryFocusedFact || resolution.Origin != QueryResolutionFallback {
		t.Fatalf("resolution = %+v", resolution)
	}
}

func TestResolveQueryPlanBoundsEntities(t *testing.T) {
	resolution := ResolveQueryPlan(
		"这些符号分别做什么",
		&QuerySemantics{Kind: QueryComparison},
		[]string{"A", "a", "B", "C", "D", "E", "F", "G", "H", "I", "J"},
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

func TestResolveQueryPlanPreservesTypedSemanticEntitiesAndMergesIdentifiers(t *testing.T) {
	resolution := ResolveQueryPlan(
		"compare the systems",
		&QuerySemantics{
			Kind: QueryComparison,
			EntitySpecs: []EntitySpec{
				{ID: "our_agent", Label: "Our Agent", Role: "first_party_agent", Aliases: []string{"自有 Agent"}},
				{ID: "google", Label: "Google", Role: "external_adapter"},
			},
		},
		[]string{"Google", "Alexa"},
	)

	if resolution.Plan.Kind != QueryComparison || resolution.Origin != QueryResolutionPlanner {
		t.Fatalf("resolution = %+v", resolution)
	}
	if len(resolution.Plan.EntitySpecs) != 3 {
		t.Fatalf("entity specs = %+v, want three distinct entities", resolution.Plan.EntitySpecs)
	}
	if got := resolution.Plan.EntitySpecs[0]; got.ID != "our_agent" || got.Role != "first_party_agent" || len(got.Aliases) != 1 {
		t.Fatalf("first entity = %+v", got)
	}
	if got := resolution.Plan.Entities; len(got) != 3 || got[0] != "our_agent" || got[1] != "google" || got[2] != "alexa" {
		t.Fatalf("entity ids = %v", got)
	}
}
