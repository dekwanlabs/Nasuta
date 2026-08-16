package domain

import (
	"fmt"
	"regexp"
	"strings"
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
	Kind     QueryKind
	Entities []string
}

// QuerySignals are planner-derived hints consumed by the local resolver.
type QuerySignals struct {
	Identifiers []string
	DomainTerms []string
}

type QueryResolutionOrigin string

const (
	QueryResolutionRule     QueryResolutionOrigin = "rule"
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

var queryKindOrder = []QueryKind{
	QueryRuntimeDiagnosis,
	QueryInventory,
	QueryOverview,
	QueryCodeReview,
}

var queryKindSignals = map[QueryKind][]string{
	QueryRuntimeDiagnosis: {
		"error", "exception", "bug", "failed", "failure", "crash", "incident",
		"timeout", "nullpointer", "npe", "stacktrace", "trace id", "traceid",
		"kibana", "5xx", "unavailable",
		"500", "502", "503", "504",
		"panic", "oom", "deadlock",
		"什么原因", "报错", "出错", "异常", "失败", "超时", "挂了", "不可用",
		"崩了", "重启", "打不开", "不响应", "内存溢出",
		"エラー", "バグ", "落ちた", "タイムアウト", "障害",
		"오류", "버그", "장애", "타임아웃",
		"erreur", "panne", "plantage", "indisponible",
		"fehler", "absturz", "ausgefallen", "nicht verfügbar",
	},
	QueryInventory: {
		"implement", "add a", "add an", "new feature", "new endpoint", "new api",
		"what's needed", "what is needed", "how to build", "how to implement",
		"how would you", "how to add", "how to create",
		"能不能", "可以加", "可以做个", "能否", "需求", "实现", "开发", "新增",
		"增加一个", "做一个", "想加", "能加吗", "新建",
		"追加", "作って", "実装", "機能",
		"추가", "구현", "만들어", "기능",
		"ajouter", "implémenter", "créer", "fonctionnalité",
		"implementieren", "hinzufügen", "funktionalität",
	},
	QueryOverview: {
		"architecture", "design pattern", "system design",
		"data source", "datasource", "dual datasource",
		"trade-off", "tradeoff", "topology", "why is",
		"scalability", "coupling", "bottleneck", "data flow",
		"为什么", "架构", "设计", "数据源", "双数据源", "双写",
		"解耦", "瓶颈", "数据流",
		"構造", "なぜ", "アーキテクチャ",
		"아키텍처", "구조", "설계",
		"conception", "pourquoi",
		"architektur", "entwurf", "warum",
	},
	QueryCodeReview: {
		"review", "code quality", "best practice", "refactor",
		"code smell", "anti-pattern",
		"有问题", "这段代码", "这个写法", "代码审查",
		"コードレビュー", "リファクタリング",
		"코드 리뷰", "리팩토링",
		"revue de code",
		"refaktorisierung",
	},
}

var traceIDRe = regexp.MustCompile(`(?i)(?:trace[_\-\s]?id|traceid|trace)\s*[=:：]?\s*([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
var uuidRe = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
var kibanaURLRe = regexp.MustCompile(`(?i)https?://[^\s]*kibana[^\s]*`)

// ResolveQueryPlan classifies request semantics once without performing I/O.
func ResolveQueryPlan(question string, signals QuerySignals) QueryResolution {
	entities := CanonicalEntityIDs(signals.Identifiers)
	if hasComparisonSignal(question, signals) {
		return QueryResolution{
			Plan:            QueryPlan{Kind: QueryComparison, Entities: entities},
			Origin:          QueryResolutionRule,
			MatchedRuleKind: QueryComparison,
		}
	}
	if hasFlowSignal(question, signals) {
		return QueryResolution{
			Plan:            QueryPlan{Kind: QueryFlow, Entities: entities},
			Origin:          QueryResolutionRule,
			MatchedRuleKind: QueryFlow,
		}
	}
	kind := classifyQueryKind(question)
	if kind == QueryFocusedFact {
		return QueryResolution{
			Plan:   QueryPlan{Kind: kind, Entities: entities},
			Origin: QueryResolutionFallback,
		}
	}
	return QueryResolution{
		Plan:            QueryPlan{Kind: kind, Entities: entities},
		Origin:          QueryResolutionRule,
		MatchedRuleKind: kind,
	}
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

func classifyQueryKind(question string) QueryKind {
	q := strings.ToLower(question)
	if traceIDRe.MatchString(q) || uuidRe.MatchString(q) || kibanaURLRe.MatchString(q) {
		return QueryRuntimeDiagnosis
	}
	for _, kind := range queryKindOrder {
		for _, signal := range queryKindSignals[kind] {
			if strings.Contains(q, signal) {
				return kind
			}
		}
	}
	return QueryFocusedFact
}

func hasComparisonSignal(question string, signals QuerySignals) bool {
	q := strings.ToLower(question)
	for _, signal := range []string{
		"对比", "比较", "区别", "差异", "共性", "异同", "各自", "分别",
		"compare", "comparison", "difference", "differences", "versus", " vs ",
	} {
		if strings.Contains(q, signal) {
			return true
		}
	}
	for _, term := range signals.DomainTerms {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "对比" || term == "比较" || term == "区别" || term == "差异" ||
			term == "共性" || term == "异同" || term == "compare" || term == "comparison" {
			return true
		}
	}
	return false
}

func hasFlowSignal(question string, signals QuerySignals) bool {
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
