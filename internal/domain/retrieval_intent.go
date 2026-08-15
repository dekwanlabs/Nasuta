package domain

import "strings"

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

type RetrievalIntentSignals struct {
	Identifiers []string
	DomainTerms []string
}

type IntentOrigin string

const (
	IntentOriginRule     IntentOrigin = "rule"
	IntentOriginFallback IntentOrigin = "fallback"
)

type IntentResolution struct {
	ResponseMode ResponseMode
	Intent       RetrievalIntent
	Origin       IntentOrigin
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

// ResolveRetrievalIntent is the single local resolver for retrieval shape.
// Planner output may enrich signals, but intent resolution never performs I/O.
func ResolveRetrievalIntent(question string, signals RetrievalIntentSignals) IntentResolution {
	mode := ClassifyResponseMode(question)
	if hasFlowSignal(question, signals) {
		return IntentResolution{
			ResponseMode: mode,
			Intent: RetrievalIntent{
				Kind: RetrievalFlow,
				RequiredFacets: []EvidenceFacet{
					FacetEntrypoint,
					FacetCoreFlow,
					FacetDataAndState,
					FacetExternalDependency,
				},
				TargetEntities: CanonicalEntityIDs(signals.Identifiers),
			},
			Origin: IntentOriginRule,
		}
	}
	intent := RetrievalIntentFor(mode)
	intent.TargetEntities = CanonicalEntityIDs(signals.Identifiers)
	origin := IntentOriginRule
	if mode == CodebaseQA && intent.Kind == RetrievalFocusedFact {
		origin = IntentOriginFallback
	}
	return IntentResolution{ResponseMode: mode, Intent: intent, Origin: origin}
}

func hasFlowSignal(question string, signals RetrievalIntentSignals) bool {
	q := strings.ToLower(question)
	for _, signal := range []string{
		"调用链", "调用关系", "谁调用", "被谁调用", "调用方", "被调用方",
		"写入路径", "落库路径", "方法实现", "函数实现", "类定义", "符号定义",
		"call chain", "caller", "callee", "callers", "callees", "implementation",
		"method body", "function body", "write path",
	} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	for _, term := range signals.DomainTerms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "调用链" || term == "调用关系" || term == "call chain" ||
			term == "caller" || term == "callee" || term == "implementation" {
			return true
		}
	}
	return false
}
