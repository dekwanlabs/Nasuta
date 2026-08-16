package retrieval

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/tokenestimate"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

// Reference is one source surfaced with retrieved context.
type Reference struct {
	Type   string `json:"type"`
	Label  string `json:"label"`
	Target string `json:"target"`
}

// RetrievedContext is the assembled retrieval payload passed to the agent.
type RetrievedContext struct {
	Text              string              `json:"text"`
	References        []Reference         `json:"references"`
	EvidenceUnits     []tool.EvidenceUnit `json:"evidenceUnits,omitempty"`
	EvidenceConflicts []evidence.Conflict `json:"evidenceConflicts,omitempty"`
	HitCount          int                 `json:"hitCount"`
	OriginalQuestion  string
	Query             domain.QueryPlan
	selection         selectionStats
}

type selectionStats struct {
	Selected  int
	Included  int
	Dropped   int
	Truncated int
	Chars     int
}

type partial struct {
	text     string
	refs     []Reference
	units    []tool.EvidenceUnit
	priority int
	score    float64
}

const (
	partialPriorityEvidence   = 0
	partialPriorityGeneral    = 3
	partialPriorityDependency = 4
	partialPriorityService    = 5
)

type codeHit struct {
	path          string
	lang          string
	repo          string
	layer         string
	snippet       string
	recallScore   float64
	denseScore    float64
	scoreKind     string
	evidenceClass string
	trustTier     int
	startLine     int
	endLine       int
}

func codeHitPassesFloor(score, floor float64) bool {
	if floor <= 0 {
		return true
	}
	return score >= floor
}

type serviceMatch struct {
	name    string
	layer   string
	lang    string
	summary string
}

type anchor struct {
	services   []string
	svcMatches map[string]serviceMatch
	codeHits   []codeHit
	runbooks   []domain.RunbookSearchHit
}

type retrievalSourceOutcome struct {
	status string
	count  int
	err    error
}

type retrievalSourcesResult struct {
	anchor  anchor
	code    retrievalSourceOutcome
	runbook retrievalSourceOutcome
	service retrievalSourceOutcome
}

var retrievalSourcesSpec = runtrace.Spec[retrievalDiscoverInput, retrievalSourcesResult]{
	Operation: "retrieval.sources",
	Node:      "retrieval_sources",
	Output: func(_ retrievalDiscoverInput, result retrievalSourcesResult, _ error) map[string]any {
		statuses := map[string]retrievalSourceOutcome{
			"code": result.code, "runbook": result.runbook, "service": result.service,
		}
		output := make(map[string]any, len(statuses))
		for source, status := range statuses {
			item := map[string]any{"status": status.status, "candidate_count": status.count}
			if status.err != nil {
				item["error"] = status.err.Error()
			}
			output[source] = item
		}
		return output
	},
}

// Retriever orchestrates workspace code, docs, and codegraph retrieval.
type Retriever struct {
	tools          toolset
	workspaceRoot  string
	cfg            config.Config
	platform       *config.PlatformSettings
	serviceModules atomic.Value // []ServiceRecord, built lazily
	reranker       Reranker
	codegraphDB    *codegraph.DB
}

type toolset interface {
	AllServices(ctx context.Context) ([]domain.ServiceRecord, error)
	FindServices(ctx context.Context, query string, limit int) (domain.SearchResult[domain.ServiceRecord], error)
	FindCode(ctx context.Context, query, lang string, limit int) (domain.SearchResult[domain.CodeSearchHit], error)
	FindAPIs(ctx context.Context, service, keyword string, limit int) ([]domain.EndpointRecord, error)
	FindRunbooks(ctx context.Context, query knowledge.RunbookQuery) (domain.RunbookSearchResult, error)
	TraceDeps(context.Context, string, string, int) (domain.DependencyTrace, error)
	ServiceModules(ctx context.Context, repos []string) ([]domain.ServiceRecord, error)
}

type vectorToolset interface {
	EmbedQuery(context.Context, string) ([]float32, error)
	FindCodeWithVector(context.Context, string, string, int, []float32) (domain.SearchResult[domain.CodeSearchHit], error)
	FindServicesWithVector(context.Context, string, int, []float32) (domain.SearchResult[domain.ServiceRecord], error)
	FindRunbooksWithVector(context.Context, knowledge.RunbookQuery, []float32) (domain.RunbookSearchResult, error)
}

type retrievalBudget struct {
	code    int
	runbook int
	service int
	rerank  int
}

type queryRetrievalPolicy struct {
	budget              retrievalBudget
	maxExpandedServices int
	expandCodeGraph     bool
	coverageSelection   bool
}

func retrievalPolicyFor(kind domain.QueryKind) queryRetrievalPolicy {
	base := queryRetrievalPolicy{
		budget: retrievalBudget{code: 12, runbook: 8, service: 6, rerank: 20},
	}
	switch kind {
	case domain.QueryFocusedFact, domain.QueryCodeReview:
		return base
	case domain.QueryRuntimeDiagnosis, domain.QueryInventory, domain.QueryComparison:
		base.budget = retrievalBudget{code: 16, runbook: 12, service: 8, rerank: 24}
	case domain.QueryFlow:
		base.budget = retrievalBudget{code: 16, runbook: 8, service: 6, rerank: 24}
		base.expandCodeGraph = true
	case domain.QueryOverview:
		base.budget = retrievalBudget{code: 16, runbook: 16, service: 8, rerank: 24}
		base.maxExpandedServices = 4
		base.coverageSelection = true
	}
	return base
}

// New builds a Retriever with dense fallback reranking enabled.
func New(t toolset, cfg config.Config) *Retriever {
	WarmTokenizer()
	return &Retriever{tools: t, workspaceRoot: cfg.WorkspaceRoot, cfg: cfg, platform: &config.PlatformSettings{}, reranker: denseReranker{}}
}

func (retrieve *Retriever) WithPlatform(p *config.PlatformSettings) *Retriever {
	if p != nil {
		retrieve.platform = p
	}
	return retrieve
}

func (retrieve *Retriever) WithReranker(r Reranker) *Retriever {
	if r != nil && r.Enabled() {
		retrieve.reranker = r
	}
	return retrieve
}

func NewDashScopeReranker(p *config.PlatformSettings) Reranker { return newDashScopeReranker(p) }

const tokenBudget = 48000

// ContextBudget returns the configured whole-run evidence budget.
func (retrieve *Retriever) ContextBudget() int {
	if retrieve != nil && retrieve.platform != nil && retrieve.platform.ContextBudget > 0 {
		return retrieve.platform.ContextBudget
	}
	return tokenBudget
}

func sortPartsByPriority(parts []partial) {
	sort.SliceStable(parts, func(i, j int) bool {
		return parts[i].priority < parts[j].priority
	})
}

// RetrievePlan dispatches only the pre-retrieval backends selected for this run.
func (retrieve *Retriever) RetrievePlan(
	ctx context.Context,
	searchQuery string,
	terms QueryTerms,
	evidencePlan domain.EvidencePlan,
	query domain.QueryPlan,
) (*RetrievedContext, error) {
	if !evidencePlan.Valid() {
		return nil, fmt.Errorf("retrieval: invalid source bits %08b", evidencePlan.Sources)
	}
	var a anchor
	if evidencePlan.Has(domain.Internal) {
		embeddingStarted := time.Now()
		reportProgress(ctx, "retrieval.embedding", "正在准备查询向量", embeddingStarted)
		var queryVector []float32
		queryVectorAttempted := false
		if vectorTools, ok := retrieve.tools.(vectorToolset); ok {
			queryVectorAttempted = true
			queryVector, _ = vectorTools.EmbedQuery(ctx, searchQuery)
		}
		reportProgress(ctx, "retrieval.embedding", "查询向量准备完成", embeddingStarted)
		discoverStarted := time.Now()
		reportProgress(ctx, "retrieval.discover", "正在查找相关代码、文档和服务", discoverStarted)
		a, _ = runtrace.Invoke(ctx, retrievalDiscoverSpec, retrievalDiscoverInput{
			SearchQuery: searchQuery, QueryVector: queryVector, QueryVectorAttempted: queryVectorAttempted,
			Plan: query, ServiceScoped: false,
		}, func(ctx context.Context, input retrievalDiscoverInput) (anchor, error) {
			return retrieve.discover(
				ctx, input.SearchQuery, input.ServicePatterns, input.ServiceScoped,
				input.QueryVector, input.QueryVectorAttempted, input.Plan,
			), nil
		})
		reportProgress(ctx, "retrieval.discover", "代码、文档和服务召回完成", discoverStarted)
	}
	expandStarted := time.Now()
	reportProgress(ctx, "retrieval.expand", "正在展开证据和依赖关系", expandStarted)
	expanded, _ := runtrace.Invoke(ctx, retrievalExpandSpec, retrievalExpandInput{
		Anchor: a, Terms: terms, EvidencePlan: evidencePlan, Plan: query,
	}, func(ctx context.Context, input retrievalExpandInput) (retrievalExpandOutput, error) {
		parts, codePool := retrieve.expand(ctx, input.Anchor, input.Terms, input.EvidencePlan, input.Plan)
		return retrievalExpandOutput{Parts: parts, CodePool: codePool}, nil
	})
	reportProgress(ctx, "retrieval.expand", "证据展开完成", expandStarted)
	rerankStarted := time.Now()
	reportProgress(ctx, "retrieval.rerank", "正在整理候选证据", rerankStarted)
	result, _ := runtrace.Invoke(ctx, retrievalAssembleSpec, retrievalAssembleInput{
		Parts: expanded.Parts, CodePool: expanded.CodePool, SearchQuery: searchQuery, Plan: query,
	}, func(ctx context.Context, input retrievalAssembleInput) (*RetrievedContext, error) {
		return retrieve.assemble(ctx, input.Parts, input.CodePool, input.SearchQuery, input.Plan), nil
	})
	reportProgress(ctx, "retrieval.rerank", "候选证据整理完成", rerankStarted)
	return result, nil
}

type retrievalDiscoverInput struct {
	SearchQuery          string
	QueryVector          []float32
	QueryVectorAttempted bool
	ServicePatterns      []string
	ServiceScoped        bool
	Plan                 domain.QueryPlan
}

var retrievalDiscoverSpec = runtrace.Spec[retrievalDiscoverInput, anchor]{
	Operation: "retrieval.discover",
	Node:      "retrieval_discover",
	Input: func(input retrievalDiscoverInput) map[string]any {
		return map[string]any{"query": input.SearchQuery, "service_scoped": input.ServiceScoped}
	},
	Output: func(_ retrievalDiscoverInput, result anchor, _ error) map[string]any {
		return map[string]any{"services": len(result.services), "code_hits": len(result.codeHits), "runbooks": len(result.runbooks)}
	},
}

type retrievalExpandInput struct {
	Anchor       anchor
	Terms        QueryTerms
	EvidencePlan domain.EvidencePlan
	Plan         domain.QueryPlan
}

type retrievalExpandOutput struct {
	Parts    []partial
	CodePool []codeDoc
}

var retrievalExpandSpec = runtrace.Spec[retrievalExpandInput, retrievalExpandOutput]{
	Operation: "retrieval.expand",
	Node:      "retrieval_expand",
	Output: func(input retrievalExpandInput, result retrievalExpandOutput, _ error) map[string]any {
		return map[string]any{
			"parts": len(result.Parts), "code_pool": len(result.CodePool),
			"sources": input.EvidencePlan.SourceNames(), "query_kind": input.Plan.Kind,
		}
	},
}

type retrievalAssembleInput struct {
	Parts       []partial
	CodePool    []codeDoc
	SearchQuery string
	Plan        domain.QueryPlan
}

var retrievalAssembleSpec = runtrace.Spec[retrievalAssembleInput, *RetrievedContext]{
	Operation: "retrieval.assemble",
	Node:      "retrieval_assemble",
	Output: func(_ retrievalAssembleInput, result *RetrievedContext, _ error) map[string]any {
		if result == nil {
			return nil
		}
		return map[string]any{
			"hit_count": result.HitCount, "references": len(result.References), "context_chars": result.selection.Chars,
			"selected": result.selection.Selected, "included": result.selection.Included,
			"dropped": result.selection.Dropped, "truncated": result.selection.Truncated,
			"query_kind": result.Query.Kind, "evidence_units": len(result.EvidenceUnits),
		}
	},
}

// discover gathers the narrow set of candidate services, code hits, and runbooks.
func (retrieve *Retriever) discover(
	ctx context.Context,
	searchQuery string,
	servicePatterns []string,
	serviceScoped bool,
	queryVector []float32,
	queryVectorAttempted bool,
	query domain.QueryPlan,
) anchor {
	result, _ := runtrace.Invoke(ctx, retrievalSourcesSpec, retrievalDiscoverInput{
		SearchQuery: searchQuery, QueryVector: queryVector, ServicePatterns: servicePatterns,
		QueryVectorAttempted: queryVectorAttempted, ServiceScoped: serviceScoped, Plan: query,
	}, retrieve.discoverSources)
	a := result.anchor
	log.InfofCtx(ctx, "[qa] retrieval sources: code=%s runbook=%s service=%s",
		result.code.status, result.runbook.status, result.service.status)

	preview := a.services
	if len(preview) > 8 {
		preview = preview[:8]
	}
	log.InfofCtx(ctx, "[qa] anchor: services=%d codeHits=%d runbooks=%d first=%v",
		len(a.services), len(a.codeHits), len(a.runbooks), preview)
	return a
}

func (retrieve *Retriever) discoverSources(ctx context.Context, input retrievalDiscoverInput) (retrievalSourcesResult, error) {
	searchQuery := input.SearchQuery
	servicePatterns := input.ServicePatterns
	serviceScoped := input.ServiceScoped
	budget := retrievalPolicyFor(input.Plan.Kind).budget
	vectorTools, vectorCapable := retrieve.tools.(vectorToolset)
	useSharedVectorPath := vectorCapable && input.QueryVectorAttempted
	var a anchor
	a.svcMatches = map[string]serviceMatch{}
	seen := map[string]bool{}
	var mu sync.Mutex
	addSvc := func(name string) {
		if name == "" {
			return
		}
		if serviceScoped && !matchesConfiguredService(name, servicePatterns) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if seen[name] {
			return
		}
		seen[name] = true
		a.services = append(a.services, name)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	codeStatus := retrievalSourceOutcome{}
	runbookStatus := retrievalSourceOutcome{}
	serviceStatus := retrievalSourceOutcome{}

	go func() {
		defer wg.Done()
		var result domain.SearchResult[domain.CodeSearchHit]
		var err error
		if useSharedVectorPath {
			result, err = vectorTools.FindCodeWithVector(ctx, searchQuery, "", budget.code, input.QueryVector)
		} else {
			result, err = retrieve.tools.FindCode(ctx, searchQuery, "", budget.code)
		}
		if err != nil {
			codeStatus.status, codeStatus.err = "failed", err
			log.InfofCtx(ctx, "[qa] semantic code search error: %v", err)
			return
		}
		codeMatches := result.Matches
		log.InfofCtx(ctx, "[qa] semantic code search selected:\n%s", codeMatchSummary(codeMatches, "code-search"))
		// Resolve repo → service in one batch so discover stays cheap on large indexes.
		uniqueRepos := uniqueRepoStrings(codeMatches)
		modules := retrieve.resolveServiceModules(ctx, uniqueRepos)
		var localHits []codeHit
		for _, m := range codeMatches {
			if m.Path == "" {
				continue
			}
			snippet := m.Text
			serviceName := retrieve.serviceForRepoMapped(ctx, modules, m.Repo, m.Path)
			if serviceScoped && (serviceName == "" || !matchesConfiguredService(serviceName, servicePatterns)) {
				continue
			}
			recallScore := m.SemanticScore
			if m.FusionScore != 0 {
				recallScore = m.FusionScore
			} else if recallScore == 0 {
				recallScore = m.Score
			}
			localHits = append(localHits, codeHit{
				path:          m.Path,
				lang:          m.Lang,
				repo:          m.Repo,
				layer:         m.Layer,
				snippet:       snippet,
				recallScore:   recallScore,
				denseScore:    m.SemanticScore,
				scoreKind:     m.ScoreKind,
				evidenceClass: m.EvidenceClass,
				trustTier:     m.TrustTier,
				startLine:     m.StartLine,
				endLine:       m.EndLine,
			})
			addSvc(serviceName)
		}
		mu.Lock()
		a.codeHits = localHits
		mu.Unlock()
		codeStatus.count = len(localHits)
		codeStatus.status = retrievalSourceStatus(len(localHits))
		log.InfofCtx(ctx, "[qa] semantic code search raw hits: %d", len(localHits))
	}()

	go func() {
		defer wg.Done()
		query := knowledge.RunbookQuery{Query: searchQuery, Limit: budget.runbook}
		var result domain.RunbookSearchResult
		var err error
		if useSharedVectorPath {
			result, err = vectorTools.FindRunbooksWithVector(ctx, query, input.QueryVector)
		} else {
			result, err = retrieve.tools.FindRunbooks(ctx, query)
		}
		if err != nil {
			runbookStatus.status, runbookStatus.err = "failed", err
			log.InfofCtx(ctx, "[qa] runbook search error: %v", err)
			return
		}
		matches := result.Matches
		mu.Lock()
		a.runbooks = matches
		mu.Unlock()
		runbookStatus.count = len(matches)
		runbookStatus.status = retrievalSourceStatus(len(matches))
		log.InfofCtx(ctx, "[qa] runbook hits: %d %v", len(matches), runbookTitles(matches))
	}()

	go func() {
		defer wg.Done()
		var matches []domain.ServiceRecord
		if serviceScoped {
			matches = retrieve.configuredServiceMatches(ctx, servicePatterns, 8)
		} else {
			var result domain.SearchResult[domain.ServiceRecord]
			var err error
			if useSharedVectorPath {
				result, err = vectorTools.FindServicesWithVector(ctx, searchQuery, budget.service, input.QueryVector)
			} else {
				result, err = retrieve.tools.FindServices(ctx, searchQuery, budget.service)
			}
			if err != nil {
				serviceStatus.status, serviceStatus.err = "failed", err
				log.InfofCtx(ctx, "[qa] service search error: %v", err)
				return
			}
			matches = result.Matches
		}
		mu.Lock()
		for _, svc := range matches {
			if svc.ServiceName == "" {
				continue
			}
			if _, ok := a.svcMatches[svc.ServiceName]; !ok {
				a.svcMatches[svc.ServiceName] = serviceMatch{
					name:    svc.ServiceName,
					layer:   svc.Layer,
					lang:    svc.Language,
					summary: svc.Summary,
				}
			}
		}
		mu.Unlock()
		for _, svc := range matches {
			addSvc(svc.ServiceName)
		}
		serviceStatus.count = len(matches)
		serviceStatus.status = retrievalSourceStatus(len(matches))
	}()

	wg.Wait()
	return retrievalSourcesResult{
		anchor: a, code: codeStatus, runbook: runbookStatus, service: serviceStatus,
	}, nil
}

func retrievalSourceStatus(count int) string {
	if count == 0 {
		return "empty"
	}
	return "completed"
}

func matchesConfiguredService(name string, patterns []string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if strings.EqualFold(name, pattern) {
			return true
		}
		matched, err := filepath.Match(strings.ToLower(pattern), strings.ToLower(name))
		if err == nil && matched {
			return true
		}
	}
	return false
}

func (retrieve *Retriever) configuredServiceMatches(ctx context.Context, patterns []string, limit int) []domain.ServiceRecord {
	if retrieve.tools == nil {
		return nil
	}
	all, err := retrieve.tools.AllServices(ctx)
	if err != nil {
		log.WarnfCtx(ctx, "[qa] configured service lookup failed: %v", err)
		return nil
	}
	matches := make([]domain.ServiceRecord, 0)
	for _, service := range all {
		if !matchesConfiguredService(service.ServiceName, patterns) {
			continue
		}
		matches = append(matches, service)
		if limit > 0 && len(matches) >= limit {
			break
		}
	}
	return matches
}

// expand fans anchor hits into formatted parts plus the unified code pool.
func (retrieve *Retriever) expand(
	ctx context.Context, a anchor, terms QueryTerms, evidencePlan domain.EvidencePlan,
	query domain.QueryPlan,
) (parts []partial, codePool []codeDoc) {
	var mu sync.Mutex
	addPart := func(p partial) { mu.Lock(); parts = append(parts, p); mu.Unlock() }
	addCode := func(d codeDoc) { mu.Lock(); codePool = append(codePool, d); mu.Unlock() }
	var wg sync.WaitGroup

	if evidencePlan.Has(domain.Internal) {
		policy := retrievalPolicyFor(query.Kind)
		services := a.services
		if policy.maxExpandedServices > 0 && len(services) > policy.maxExpandedServices {
			services = services[:policy.maxExpandedServices]
		}
		wg.Add(1)
		go func() { defer wg.Done(); retrieve.collectServices(ctx, services, a.svcMatches, addPart) }()
		wg.Add(1)
		go func() { defer wg.Done(); retrieve.collectCode(ctx, a.codeHits, addCode) }()
		wg.Add(1)
		go func() { defer wg.Done(); retrieve.collectRunbooks(ctx, a.runbooks, addCode) }()
		wg.Add(1)
		go func() { defer wg.Done(); retrieve.collectDeps(ctx, services, addPart) }()

		cgKeywords := []string(nil)
		if policy.expandCodeGraph {
			cgKeywords = retrieve.buildCodeGraphKeywords(a.services, terms)
		}
		log.InfofCtx(ctx, "[qa] codegraph keywords (cleaned): %d %v", len(cgKeywords), cgKeywords)
		if len(cgKeywords) > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				retrieve.collectCodeGraph(ctx, cgKeywords, a.services, terms, addCode)
			}()
		}
	}
	wg.Wait()
	return parts, codePool
}

// assemble applies pool post-processing, joins sections, and enforces the context budget.
func (retrieve *Retriever) assemble(
	ctx context.Context,
	parts []partial,
	codePool []codeDoc,
	searchQuery string,
	query domain.QueryPlan,
) *RetrievedContext {
	if len(codePool) > 0 && retrieve.platform.RerankEnabled {
		codePool = retrieve.postProcessCodePool(ctx, codePool, searchQuery, query)
		parts = append(parts, retrieve.formatCodePool(ctx, codePool)...)
	} else if len(codePool) > 0 {
		log.InfofCtx(ctx, "[qa] code pool final (rerank disabled): %d docs\n%s", len(codePool), poolSummary(codePool, "rerank-disabled"))
		parts = append(parts, retrieve.formatCodePool(ctx, codePool)...)
	}
	if retrievalPolicyFor(query.Kind).coverageSelection {
		parts = selectOverviewEvidence(parts, domain.RequiredFacetsFor(query.Kind))
	}

	log.InfofCtx(ctx, "[qa] retrieve parts: %d sources", len(parts))
	for i, p := range parts {
		log.InfofCtx(ctx, "[qa]   part[%d]: %d chars, %d refs,content=%v", i, len(p.text), len(p.refs), p)
	}

	sortPartsByPriority(parts)

	var allText strings.Builder
	var allRefs []Reference
	evidenceLedger := evidence.New(nil, "")
	var evidenceConflicts []evidence.Conflict
	seenRefs := map[string]struct{}{}
	budget := retrieve.ContextBudget()
	stats := selectionStats{Selected: len(parts)}
	for _, p := range parts {
		if p.text == "" || budget <= 0 {
			continue
		}
		partText := retrieve.cleanWorkspacePaths(ctx, p.text)
		partTokens := tokenestimate.Count(partText)
		truncated := partTokens > budget
		deliveredText := partText
		if truncated {
			const marker = "\n...(truncated)"
			markerTokens := tokenestimate.Count(marker)
			contentBudget := max(0, budget-markerTokens)
			deliveredText = tokenestimate.Prefix(partText, contentBudget)
			if markerTokens <= budget {
				deliveredText += marker
			}
			allText.WriteString(deliveredText)
			budget = 0
			stats.Truncated++
		} else {
			allText.WriteString(deliveredText)
			budget -= partTokens
			if budget > 0 {
				allText.WriteByte('\n')
				budget -= tokenestimate.Count("\n")
			}
		}
		stats.Included++
		for _, ref := range p.refs {
			key := ref.Type + ":" + ref.Target
			if _, dup := seenRefs[key]; dup {
				continue
			}
			seenRefs[key] = struct{}{}
			allRefs = append(allRefs, ref)
		}
		for _, unit := range p.units {
			unit = evidence.CloneUnit(unit)
			if truncated {
				unit.Coverage.Complete = false
				unit.Coverage.Partial = true
			}
			unit.TokenCost = tokenestimate.Count(deliveredText)
			if conflicts := evidenceLedger.Add([]tool.EvidenceUnit{unit}, "retrieval"); len(conflicts) > 0 {
				evidenceConflicts = append(evidenceConflicts, conflicts...)
				for _, conflict := range conflicts {
					log.WarnfCtx(ctx,
						"[qa] conflicting retrieval evidence source=%s target=%s section=%s version=%s time_range=%s",
						conflict.Key.SourceKind, conflict.Key.Target, conflict.Key.Section,
						conflict.Key.Version, conflict.Key.TimeRange,
					)
				}
			}
		}
		if truncated {
			log.InfofCtx(ctx, "[qa] context budget reached; truncated rank-preserving evidence selection")
			break
		}
	}

	contextText := allText.String()
	stats.Dropped = stats.Selected - stats.Included
	stats.Chars = len([]rune(contextText))

	return &RetrievedContext{
		Text:              contextText,
		References:        allRefs,
		EvidenceUnits:     evidenceLedger.Units(),
		EvidenceConflicts: evidence.CloneConflicts(evidenceConflicts),
		HitCount:          len(allRefs),
		Query:             query,
		selection:         stats,
	}
}

var evidenceLayerHeadings = map[string]string{
	"server": "Server",
	"app":    "App",
	"front":  "Front",
	"mcu":    "MCU",
	"module": "Module",
	"docs":   "Docs",
}

// formatCodePool preserves rerank order and keeps each source independently budgetable.
func (retrieve *Retriever) formatCodePool(ctx context.Context, pool []codeDoc) []partial {
	if len(pool) == 0 {
		return nil
	}
	parts := make([]partial, 0, len(pool))
	for _, d := range pool {
		var text strings.Builder
		var ref Reference
		switch d.source {
		case "code":
			heading := evidenceLayerHeadings[d.layer]
			if heading == "" {
				heading = "Other"
			}
			fmt.Fprintf(&text, "## Evidence — %s\n", heading)
			label := retrieve.shortPath(ctx, d.filePath)
			if d.startLine > 0 {
				fmt.Fprintf(&text, "### %s (L%d-L%d)\n```\n%s\n```\n", label, d.startLine, d.endLine, d.text)
				ref = Reference{Type: "code", Label: fmt.Sprintf("%s:L%d", label, d.startLine), Target: d.filePath}
			} else {
				fmt.Fprintf(&text, "### %s\n```\n%s\n```\n", label, d.text)
				ref = Reference{Type: "code", Label: label, Target: d.filePath}
			}
		case "runbook":
			text.WriteString("## Evidence — Docs\n")
			title := d.funcName
			fmt.Fprintf(&text, "### %s", title)
			if d.kind != "" {
				fmt.Fprintf(&text, " (%s)", d.kind)
			}
			fmt.Fprintf(&text, "\n%s\n", d.text)
			ref = Reference{Type: string(tool.ReferenceRunbook), Label: title, Target: d.docID}
		case "codegraph":
			text.WriteString("## CodeGraph Deep Analysis\n")
			text.WriteString(d.text)
			text.WriteString("\n")
			ref = Reference{Type: "codegraph", Label: d.funcName, Target: d.filePath}
		default:
			continue
		}
		partText := text.String()
		unit := evidenceUnitForCodeDoc(d, partText)
		parts = append(parts, partial{
			text: partText, refs: []Reference{ref}, units: []tool.EvidenceUnit{unit},
			priority: partialPriorityEvidence, score: d.candidateScore(),
		})
	}
	return parts
}

// WithCodeGraph wires the codegraph SQLite DB for direct queries (replaces the
// old CLI subprocess approach).
func (retrieve *Retriever) WithCodeGraph(db *codegraph.DB) *Retriever {
	retrieve.codegraphDB = db
	return retrieve
}

// codeGraphQuery runs one scoped full-text search for the complete keyword set.
func (retrieve *Retriever) codeGraphQuery(ctx context.Context, keywords, services []string, limit int) []codegraph.Node {
	if retrieve.codegraphDB == nil {
		return nil
	}
	wanted := make(map[string]struct{}, len(services))
	for _, service := range services {
		wanted[platform.Normalize(service)] = struct{}{}
	}
	prefixes := make([]string, 0, len(services))
	seen := make(map[string]struct{}, len(services))
	if len(wanted) > 0 {
		for _, module := range retrieve.allServiceModules(ctx) {
			if _, ok := wanted[platform.Normalize(module.ServiceName)]; !ok {
				continue
			}
			prefix := "repos/" + modulePrefix(module)
			if _, ok := seen[prefix]; ok {
				continue
			}
			seen[prefix] = struct{}{}
			prefixes = append(prefixes, prefix)
		}
	}
	nodes, err := retrieve.codegraphDB.SearchSymbols(ctx, codegraph.SymbolQuery{
		Terms: keywords, PathPrefixes: prefixes, Limit: limit,
	})
	if err != nil {
		log.WarnfCtx(ctx, "[qa] codegraph query: %v", err)
		return nil
	}
	return nodes
}

// codeGraphNode reads the source file around a node and returns a formatted
// snippet for the agent context. It streams only the symbol's line window
// instead of buffering the whole file, so cost scales with symbol size.
func (retrieve *Retriever) codeGraphNode(ctx context.Context, name, file string, line int) string {
	if retrieve.codegraphDB == nil {
		return ""
	}
	dbNode, err := retrieve.codegraphDB.FindNodeAt(ctx, file, line)
	if err != nil || dbNode == nil {
		return ""
	}
	readPath := file
	if !filepath.IsAbs(readPath) && retrieve.workspaceRoot != "" {
		readPath = filepath.Join(retrieve.workspaceRoot, readPath)
	}
	f, err := os.Open(readPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	// Window mirrors the old extractLines padding: 2 lines above, 3 below.
	first := max(dbNode.StartLine-2, 1)
	last := dbNode.EndLine + 3
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var b strings.Builder
	idx := 0
	for sc.Scan() {
		idx++
		if idx < first {
			continue
		}
		if idx > last {
			break
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(sc.Text())
	}
	if err := sc.Err(); err != nil {
		return ""
	}
	if b.Len() == 0 {
		return ""
	}
	return fmt.Sprintf("### %s (%s:%d-%d)\n```%s\n%s\n```\n",
		dbNode.Name, retrieve.shortPath(ctx, file), dbNode.StartLine, dbNode.EndLine,
		dbNode.Language, b.String())
}
