package domain

import (
	"fmt"
	"regexp"
)

// QueryKind is the canonical shape of one QA request.
type QueryKind string

const (
	QueryFocusedFact      QueryKind = "focused_fact"
	QueryOverview         QueryKind = "overview"
	QueryFlow             QueryKind = "flow"
	QueryComparison       QueryKind = "comparison"
	QueryInventory        QueryKind = "inventory"
	QueryRuntimeDiagnosis QueryKind = "runtime_diagnosis"
	QueryCodeReview       QueryKind = "code_review"
)

// QueryPlan retains only request semantics that cannot be derived from the kind.
type QueryPlan struct {
	Kind        QueryKind
	Entities    []string
	EntitySpecs []EntitySpec
}

// QuerySemantics is the planner-owned answer shape for one request.
type QuerySemantics struct {
	Kind        QueryKind
	EntitySpecs []EntitySpec
}

type QueryResolutionOrigin string

const (
	QueryResolutionRule     QueryResolutionOrigin = "rule"
	QueryResolutionPlanner  QueryResolutionOrigin = "planner"
	QueryResolutionFallback QueryResolutionOrigin = "fallback"
)

// QueryResolution keeps diagnostics at the classification boundary.
type QueryResolution struct {
	Plan            QueryPlan
	Origin          QueryResolutionOrigin
	MatchedRuleKind QueryKind
}

// EvidenceFacet is one stable dimension used for coverage-driven selection.
type EvidenceFacet string

const (
	FacetSystemBoundary     EvidenceFacet = "system_boundary"
	FacetBusinessDomain     EvidenceFacet = "business_domain"
	FacetEntrypoint         EvidenceFacet = "entrypoint"
	FacetCoreFlow           EvidenceFacet = "core_flow"
	FacetDataAndState       EvidenceFacet = "data_and_state"
	FacetExternalDependency EvidenceFacet = "external_dependency"
	FacetRuntimeOperations  EvidenceFacet = "runtime_and_operations"
)

// FacetSpec defines one canonical answer-evidence dimension.
type FacetSpec struct {
	ID          EvidenceFacet
	Description string
}

var facetCatalog = []FacetSpec{
	{ID: FacetSystemBoundary, Description: "system, service, or module responsibility boundary"},
	{ID: FacetBusinessDomain, Description: "business goal, domain concepts, and responsibilities"},
	{ID: FacetEntrypoint, Description: "API, event, job, command, or code entrypoint"},
	{ID: FacetCoreFlow, Description: "main processing steps, call chain, and control flow"},
	{ID: FacetDataAndState, Description: "data model, storage, state changes, and consistency"},
	{ID: FacetExternalDependency, Description: "external services, middleware, protocols, and dependencies"},
	{ID: FacetRuntimeOperations, Description: "logs, metrics, traces, deployment, alerts, and operations"},
}

var knownFacets = func() map[EvidenceFacet]struct{} {
	known := make(map[EvidenceFacet]struct{}, len(facetCatalog))
	for _, spec := range facetCatalog {
		known[spec.ID] = struct{}{}
	}
	return known
}()

var requiredFacetsByQueryKind = map[QueryKind][]EvidenceFacet{
	QueryFocusedFact: nil,
	QueryOverview: {
		FacetSystemBoundary,
		FacetBusinessDomain,
		FacetEntrypoint,
		FacetCoreFlow,
		FacetDataAndState,
		FacetExternalDependency,
		FacetRuntimeOperations,
	},
	QueryFlow: {
		FacetEntrypoint,
		FacetCoreFlow,
		FacetDataAndState,
		FacetExternalDependency,
	},
	QueryComparison: {
		FacetBusinessDomain,
		FacetCoreFlow,
		FacetDataAndState,
		FacetExternalDependency,
	},
	QueryInventory: {
		FacetSystemBoundary,
		FacetBusinessDomain,
		FacetEntrypoint,
		FacetDataAndState,
		FacetExternalDependency,
	},
	QueryRuntimeDiagnosis: {
		FacetEntrypoint,
		FacetCoreFlow,
		FacetExternalDependency,
		FacetRuntimeOperations,
	},
	QueryCodeReview: {
		FacetEntrypoint,
		FacetCoreFlow,
		FacetDataAndState,
		FacetExternalDependency,
	},
}

var traceIDFieldRe = regexp.MustCompile(`(?i)\btrace(?:[_-]?id)?\s*[:=：]\s*[0-9a-f-]{12,64}\b`)
var traceparentRe = regexp.MustCompile(`(?i)\b[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}\b`)
var kibanaTraceURLRe = regexp.MustCompile(`(?i)https?://[^\s]*kibana[^\s]*(?:trace|discover)[^\s]*`)

// ResolveQueryPlan applies only closed-format overrides around planner semantics.
func ResolveQueryPlan(
	question string,
	semantics *QuerySemantics,
	identifiers []string,
) QueryResolution {
	entities := CanonicalEntityIDs(identifiers)
	entitySpecs := canonicalQueryEntitySpecs(semantics, entities)
	entities = entitySpecIDs(entitySpecs)
	if hasTypedRuntimeLocator(question) {
		return QueryResolution{
			Plan:            QueryPlan{Kind: QueryRuntimeDiagnosis, Entities: entities, EntitySpecs: entitySpecs},
			Origin:          QueryResolutionRule,
			MatchedRuleKind: QueryRuntimeDiagnosis,
		}
	}
	if semantics != nil {
		return QueryResolution{
			Plan:   QueryPlan{Kind: semantics.Kind, Entities: entities, EntitySpecs: entitySpecs},
			Origin: QueryResolutionPlanner,
		}
	}
	return QueryResolution{
		Plan:   QueryPlan{Kind: QueryFocusedFact, Entities: entities, EntitySpecs: entitySpecs},
		Origin: QueryResolutionFallback,
	}
}

func canonicalQueryEntitySpecs(semantics *QuerySemantics, identifiers []string) []EntitySpec {
	specs := make([]EntitySpec, 0, len(identifiers)+MaxCanonicalEntities)
	if semantics != nil {
		specs = append(specs, semantics.EntitySpecs...)
	}
	ids := CanonicalEntityIDs(identifiers)
	for _, id := range ids {
		specs = append(specs, EntitySpec{ID: id})
	}
	return CanonicalEntitySpecs(specs)
}

func entitySpecIDs(specs []EntitySpec) []string {
	ids := make([]string, 0, len(specs))
	for _, spec := range specs {
		ids = append(ids, spec.ID)
	}
	return ids
}

func hasTypedRuntimeLocator(question string) bool {
	return traceIDFieldRe.MatchString(question) ||
		traceparentRe.MatchString(question) ||
		kibanaTraceURLRe.MatchString(question)
}

// RequiredFacetsFor derives stable coverage goals from the canonical query kind.
func RequiredFacetsFor(kind QueryKind) []EvidenceFacet {
	return append([]EvidenceFacet(nil), requiredFacetsByQueryKind[kind]...)
}

// FacetCatalog returns a copy so callers cannot mutate the canonical ordering.
func FacetCatalog() []FacetSpec {
	return append([]FacetSpec(nil), facetCatalog...)
}

func IsKnownFacet(facet EvidenceFacet) bool {
	_, ok := knownFacets[facet]
	return ok
}

func ValidateFacets(facets []EvidenceFacet) error {
	seen := make(map[EvidenceFacet]struct{}, len(facets))
	for _, facet := range facets {
		if !IsKnownFacet(facet) {
			return fmt.Errorf("unknown evidence facet %q", facet)
		}
		if _, duplicate := seen[facet]; duplicate {
			return fmt.Errorf("duplicate evidence facet %q", facet)
		}
		seen[facet] = struct{}{}
	}
	return nil
}

// ProvidedFacetsFor derives conservative content coverage from an evidence kind.
func ProvidedFacetsFor(source, kind string) []EvidenceFacet {
	var facets []EvidenceFacet
	switch source {
	case "service":
		facets = []EvidenceFacet{FacetSystemBoundary, FacetBusinessDomain}
	case "dependency":
		facets = []EvidenceFacet{FacetExternalDependency}
	case "code":
		facets = []EvidenceFacet{FacetEntrypoint, FacetCoreFlow, FacetDataAndState}
	case "codegraph":
		facets = []EvidenceFacet{FacetCoreFlow}
	case "runtime":
		facets = []EvidenceFacet{FacetRuntimeOperations}
	case "runbook":
		switch kind {
		case DocKindFlow:
			facets = []EvidenceFacet{FacetSystemBoundary, FacetCoreFlow}
		case DocKindSchema:
			facets = []EvidenceFacet{FacetDataAndState}
		case DocKindModule:
			facets = []EvidenceFacet{FacetBusinessDomain, FacetEntrypoint}
		default:
			facets = []EvidenceFacet{FacetSystemBoundary}
		}
	}
	return append([]EvidenceFacet(nil), facets...)
}
