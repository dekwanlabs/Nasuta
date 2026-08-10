package domain

// RetrievalIntentKind describes the evidence shape needed by one request.
type RetrievalIntentKind string

const (
	RetrievalFocusedFact      RetrievalIntentKind = "focused_fact"
	RetrievalOverview         RetrievalIntentKind = "overview"
	RetrievalFlow             RetrievalIntentKind = "flow"
	RetrievalInventory        RetrievalIntentKind = "inventory"
	RetrievalRuntimeDiagnosis RetrievalIntentKind = "runtime_diagnosis"
)

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

// RetrievalIntent keeps retrieval goals separate from provider selection.
type RetrievalIntent struct {
	Kind           RetrievalIntentKind
	RequiredFacets []EvidenceFacet
	TargetEntities []string
}

// RetrievalIntentFor maps the existing response classification at one boundary.
func RetrievalIntentFor(mode ResponseMode) RetrievalIntent {
	switch mode {
	case ArchitectureReview:
		return RetrievalIntent{
			Kind: RetrievalOverview,
			RequiredFacets: []EvidenceFacet{
				FacetSystemBoundary,
				FacetBusinessDomain,
				FacetEntrypoint,
				FacetCoreFlow,
				FacetDataAndState,
				FacetExternalDependency,
				FacetRuntimeOperations,
			},
		}
	case BugAnalysis:
		return RetrievalIntent{
			Kind: RetrievalRuntimeDiagnosis,
			RequiredFacets: []EvidenceFacet{
				FacetEntrypoint,
				FacetCoreFlow,
				FacetExternalDependency,
				FacetRuntimeOperations,
			},
		}
	case RequirementsAnalysis:
		return RetrievalIntent{
			Kind: RetrievalInventory,
			RequiredFacets: []EvidenceFacet{
				FacetSystemBoundary,
				FacetBusinessDomain,
				FacetEntrypoint,
				FacetDataAndState,
				FacetExternalDependency,
			},
		}
	default:
		return RetrievalIntent{Kind: RetrievalFocusedFact}
	}
}
