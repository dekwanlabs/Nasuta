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
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

// Reference is one source surfaced with retrieved context.
type Reference struct {
	Type   string `json:"type"`
	Label  string `json:"label"`
	Target string `json:"target"`
}

// RetrievedContext is the assembled retrieval payload passed to the agent.
type RetrievedContext struct {
	Text             string      `json:"text"`
	References       []Reference `json:"references"`
	HitCount         int         `json:"hitCount"`
	OriginalQuestion string
	selection        selectionStats
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
	priority int
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
	FindAPIs(ctx context.Context, service, pathKeyword string, limit int) ([]domain.EndpointRecord, error)
	FindRunbooks(ctx context.Context, query string, limit int, includeText bool, scopeFilter string) (domain.SearchResult[domain.RunbookSearchHit], error)
	TraceDeps(context.Context, string, string, int) (domain.DependencyTrace, error)
	ServiceModules(ctx context.Context, repos []string) ([]domain.ServiceRecord, error)
}

// New builds a Retriever with dense fallback reranking enabled.
func New(t toolset, cfg config.Config) *Retriever {
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
func (retrieve *Retriever) RetrievePlan(ctx context.Context, searchQuery, rawQuestion string, terms QueryTerms, evidencePlan domain.EvidencePlan) (*RetrievedContext, error) {
	traceEnabled := domain.TraceEnabled(ctx)
	if !evidencePlan.Valid() {
		return nil, fmt.Errorf("retrieval: invalid source bits %08b", evidencePlan.Sources)
	}
	if rawQuestion == "" {
		rawQuestion = searchQuery
	}
	var a anchor
	if evidencePlan.Has(domain.Internal) {
		started := time.Now()
		a = retrieve.discover(ctx, searchQuery, nil, false)
		if traceEnabled {
			domain.RecordTrace(ctx, domain.EvaluationTrace{
				Node: "retrieval_discover", DurationMS: time.Since(started).Milliseconds(),
				Input:  map[string]any{"query": searchQuery, "service_scoped": false},
				Output: map[string]any{"services": len(a.services), "code_hits": len(a.codeHits), "runbooks": len(a.runbooks)},
			})
		}
	}
	expandStarted := time.Now()
	parts, codePool := retrieve.expand(ctx, a, rawQuestion, terms, evidencePlan)
	if traceEnabled {
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "retrieval_expand", DurationMS: time.Since(expandStarted).Milliseconds(),
			Output: map[string]any{"parts": len(parts), "code_pool": len(codePool), "sources": evidencePlan.SourceNames()},
		})
	}
	assembleStarted := time.Now()
	result := retrieve.assemble(ctx, parts, codePool, searchQuery)
	if traceEnabled {
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "retrieval_assemble", DurationMS: time.Since(assembleStarted).Milliseconds(),
			Output: map[string]any{
				"hit_count": result.HitCount, "references": len(result.References), "context_chars": result.selection.Chars,
				"selected": result.selection.Selected, "included": result.selection.Included,
				"dropped": result.selection.Dropped, "truncated": result.selection.Truncated,
			},
		})
	}
	return result, nil
}

// discover gathers the narrow set of candidate services, code hits, and runbooks.
func (retrieve *Retriever) discover(ctx context.Context, searchQuery string, servicePatterns []string, serviceScoped bool) anchor {
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

	go func() {
		defer wg.Done()
		result, err := retrieve.tools.FindCode(ctx, searchQuery, "", 10)
		if err != nil {
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
			if snippet == "" {
				snippet = m.Preview
			}
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
		log.InfofCtx(ctx, "[qa] semantic code search raw hits: %d", len(localHits))
	}()

	go func() {
		defer wg.Done()
		result, err := retrieve.tools.FindRunbooks(ctx, searchQuery, 5, true, "")
		if err != nil {
			log.InfofCtx(ctx, "[qa] runbook search error: %v", err)
			return
		}
		matches := result.Matches
		mu.Lock()
		a.runbooks = matches
		mu.Unlock()
		log.InfofCtx(ctx, "[qa] runbook hits: %d %v", len(matches), runbookTitles(matches))
	}()

	go func() {
		defer wg.Done()
		var matches []domain.ServiceRecord
		if serviceScoped {
			matches = retrieve.configuredServiceMatches(ctx, servicePatterns, 8)
		} else {
			result, err := retrieve.tools.FindServices(ctx, searchQuery, 8)
			if err != nil {
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
	}()

	wg.Wait()

	preview := a.services
	if len(preview) > 8 {
		preview = preview[:8]
	}
	log.InfofCtx(ctx, "[qa] anchor: services=%d codeHits=%d runbooks=%d first=%v",
		len(a.services), len(a.codeHits), len(a.runbooks), preview)
	return a
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
	ctx context.Context, a anchor, rawQuestion string, terms QueryTerms, evidencePlan domain.EvidencePlan,
) (parts []partial, codePool []codeDoc) {
	var mu sync.Mutex
	addPart := func(p partial) { mu.Lock(); parts = append(parts, p); mu.Unlock() }
	addCode := func(d codeDoc) { mu.Lock(); codePool = append(codePool, d); mu.Unlock() }
	var wg sync.WaitGroup

	if evidencePlan.Has(domain.Internal) {
		wg.Add(1)
		go func() { defer wg.Done(); retrieve.collectServices(ctx, a.services, a.svcMatches, addPart) }()
		wg.Add(1)
		go func() { defer wg.Done(); retrieve.collectCode(ctx, a.codeHits, addCode) }()
		wg.Add(1)
		go func() { defer wg.Done(); retrieve.collectRunbooks(ctx, a.runbooks, addCode) }()
		wg.Add(1)
		go func() { defer wg.Done(); retrieve.collectDeps(ctx, a.services, addPart) }()

		cgKeywords := []string(nil)
		if shouldExpandCodeGraph(rawQuestion, terms) {
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
func (retrieve *Retriever) assemble(ctx context.Context, parts []partial, codePool []codeDoc, searchQuery string) *RetrievedContext {

	if len(codePool) > 0 && retrieve.platform.RerankEnabled {
		codePool = retrieve.postProcessCodePool(ctx, codePool, searchQuery)
		parts = append(parts, retrieve.formatCodePool(ctx, codePool)...)
	} else if len(codePool) > 0 {
		log.InfofCtx(ctx, "[qa] code pool final (rerank disabled): %d docs\n%s", len(codePool), poolSummary(codePool, "rerank-disabled"))
		parts = append(parts, retrieve.formatCodePool(ctx, codePool)...)
	}

	log.InfofCtx(ctx, "[qa] retrieve parts: %d sources", len(parts))
	for i, p := range parts {
		log.InfofCtx(ctx, "[qa]   part[%d]: %d chars, %d refs,content=%v", i, len(p.text), len(p.refs), p)
	}

	sortPartsByPriority(parts)

	var allText strings.Builder
	var allRefs []Reference
	seenRefs := map[string]struct{}{}
	budget := retrieve.ContextBudget()
	stats := selectionStats{Selected: len(parts)}
	for _, p := range parts {
		if p.text == "" || budget <= 0 {
			continue
		}
		runes := []rune(p.text)
		truncated := len(runes) > budget
		if truncated {
			marker := []rune("\n...(truncated)")
			if budget > len(marker) {
				allText.WriteString(string(runes[:budget-len(marker)]))
				allText.WriteString(string(marker))
			} else {
				allText.WriteString(string(runes[:budget]))
			}
			budget = 0
			stats.Truncated++
		} else {
			allText.WriteString(p.text)
			budget -= len(runes)
			if budget > 0 {
				allText.WriteByte('\n')
				budget--
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
		if truncated {
			log.InfofCtx(ctx, "[qa] context budget reached; truncated rank-preserving evidence selection")
			break
		}
	}

	contextText := retrieve.cleanWorkspacePaths(ctx, allText.String())
	stats.Dropped = stats.Selected - stats.Included
	stats.Chars = len([]rune(contextText))

	return &RetrievedContext{
		Text:       contextText,
		References: allRefs,
		HitCount:   len(allRefs),
		selection:  stats,
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
				ref = Reference{Type: "code", Label: fmt.Sprintf("%s:L%d", label, d.startLine), Target: label}
			} else {
				fmt.Fprintf(&text, "### %s\n```\n%s\n```\n", label, d.text)
				ref = Reference{Type: "code", Label: label, Target: label}
			}
		case "runbook":
			text.WriteString("## Evidence — Docs\n")
			title := d.funcName
			fmt.Fprintf(&text, "### %s", title)
			if d.kind != "" {
				fmt.Fprintf(&text, " (%s)", d.kind)
			}
			fmt.Fprintf(&text, "\n%s\n", d.text)
			ref = Reference{Type: "runbook", Label: title, Target: title}
		case "codegraph":
			text.WriteString("## CodeGraph Deep Analysis\n")
			text.WriteString(d.text)
			text.WriteString("\n")
			ref = Reference{Type: "codegraph", Label: d.funcName, Target: retrieve.shortPath(ctx, d.filePath)}
		default:
			continue
		}
		parts = append(parts, partial{text: text.String(), refs: []Reference{ref}, priority: partialPriorityEvidence})
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
