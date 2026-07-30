package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

// Deps bundles the stores and services used by tool handlers.
type Deps struct {
	DB            *store.SQLite
	Semantic      semantic.Store
	Embedder      embed.Embedder
	WorkspaceRoot string
	// DocStore is optional in deployments without MySQL.
	DocStore  docStore
	CallChain *callchain.Service
	Ontology  *ontology.Service
}

// docStore is the runbook-facing subset of the document store.
type docStore interface {
	RunbookMetas() ([]domain.RunbookRecord, error)
	RunbookMetaByID(id string) (domain.RunbookRecord, error)
	SearchRunbooksKeyword(query string, limit int) ([]domain.RunbookRecord, error)
	RunbookByID(id string) (domain.RunbookRecord, error)
	CountRunbooks() (int, error)
}

// Service exposes the retrieval and analysis tools used by the agent.
type Service struct {
	db                    *store.SQLite
	semantic              semantic.Store
	embedder              embed.Embedder
	workspaceRoot         string
	docStore              docStore
	callChain             *callchain.Service
	ontology              *ontology.Service
	bm25                  atomic.Pointer[retrieval.BM25Builder]
	mergedSvcCache        atomic.Pointer[[]domain.ServiceRecord]
	denseWarnOnce         sync.Once
	webSearchEngine       string // registered provider name
	webSearchAPIKey       string // API key for built-in credentialed providers
	webSearchProviderOnce sync.Once
	webSearchProvidersMu  sync.RWMutex
	webSearchProviders    map[string]WebSearchProvider
}

func NewTools(d Deps) *Service {
	return &Service{
		db:            d.DB,
		semantic:      d.Semantic,
		embedder:      d.Embedder,
		workspaceRoot: d.WorkspaceRoot,
		docStore:      d.DocStore,
		callChain:     d.CallChain,
		ontology:      d.Ontology,
	}
}

func (srv *Service) SetBM25(b *retrieval.BM25Builder) { srv.bm25.Store(b) }

func (srv *Service) BM25View() *retrieval.BM25Builder { return srv.bm25.Load() }

func (srv *Service) InvalidateServices() { srv.mergedSvcCache.Store(nil) }
func (srv *Service) SetWebSearchEngine(v string) {
	srv.webSearchEngine = strings.ToLower(strings.TrimSpace(v))
}
func (srv *Service) SetWebSearchAPIKey(v string) { srv.webSearchAPIKey = v }

// RegisterWebSearchProvider adds or replaces a named search provider.
// Registration is intended for application wiring before requests begin.
func (srv *Service) RegisterWebSearchProvider(name string, provider WebSearchProvider) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fmt.Errorf("web search provider name is empty")
	}
	if provider == nil {
		return fmt.Errorf("web search provider %q is nil", name)
	}
	srv.ensureWebSearchProviders()
	srv.webSearchProvidersMu.Lock()
	srv.webSearchProviders[name] = provider
	srv.webSearchProvidersMu.Unlock()
	return nil
}

func (srv *Service) semanticEnabled() bool {
	return srv.semantic != nil && srv.semantic.Capabilities().Dense &&
		srv.embedder != nil && srv.embedder.Enabled()
}

func (srv *Service) AllServices(ctx context.Context) ([]domain.ServiceRecord, error) {
	return srv.db.AllServices(ctx)
}

func (srv *Service) ServiceModules(ctx context.Context, repos []string) ([]domain.ServiceRecord, error) {
	if len(repos) == 0 {
		return srv.db.AllServices(ctx)
	}
	return srv.db.ServicesByRepos(ctx, repos)
}

func (srv *Service) services(ctx context.Context) ([]domain.ServiceRecord, error) {
	if cached := srv.mergedSvcCache.Load(); cached != nil {
		return *cached, nil
	}
	all, err := srv.db.AllServices(ctx)
	if err != nil {
		return nil, err
	}
	srv.mergedSvcCache.Store(&all)
	return all, nil
}

func (srv *Service) ServiceLookup(ctx context.Context, query string, limit int) map[string]any {
	result, err := srv.ServiceLookupResult(ctx, query, limit)
	if err != nil {
		return map[string]any{"matches": nil, "semantic": false, "error": err.Error()}
	}
	return result
}

// ServiceLookupResult returns the service lookup payload without hiding backend failures.
func (srv *Service) ServiceLookupResult(ctx context.Context, query string, limit int) (map[string]any, error) {
	limit = clampInt(limit, 1, 100)
	result, err := srv.FindServices(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"matches": result.Matches, "semantic": result.Semantic}, nil
}

// FindServices returns typed service matches for internal consumers.
func (srv *Service) FindServices(ctx context.Context, query string, limit int) (result domain.SearchResult[domain.ServiceRecord], resultErr error) {
	started := time.Now()
	if domain.TraceEnabled(ctx) {
		defer func() {
			status := "completed"
			output := map[string]any{"matches": len(result.Matches), "semantic": result.Semantic, "services": traceServiceNames(result.Matches)}
			if resultErr != nil {
				status = "failed"
				output["error"] = resultErr.Error()
			}
			domain.RecordTrace(ctx, domain.EvaluationTrace{
				Node: "service_search", Status: status, DurationMS: time.Since(started).Milliseconds(),
				Input: map[string]any{"query": query, "limit": limit}, Output: output,
			})
		}()
	}
	all, err := srv.services(ctx)
	if err != nil {
		return domain.SearchResult[domain.ServiceRecord]{}, err
	}
	matches := scoreServices(all, query, limit)

	semantic := false
	if srv.semanticEnabled() {
		if names, err := srv.semanticServiceNames(ctx, query, limit); err == nil {
			matches = mergeServiceMatches(names, all, matches, limit)
			semantic = true
		}
	}
	return domain.SearchResult[domain.ServiceRecord]{Matches: matches, Semantic: semantic}, nil
}

func traceServiceNames(matches []domain.ServiceRecord) []string {
	limit := min(len(matches), 10)
	names := make([]string, 0, limit)
	for _, match := range matches[:limit] {
		names = append(names, match.ServiceName)
	}
	return names
}

func (srv *Service) semanticServiceNames(ctx context.Context, query string, limit int) ([]string, error) {
	vecs, err := srv.embedder.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 {
		return nil, err
	}
	hits, err := srv.semantic.Search(ctx, semantic.Query{
		DenseVector: vecs[0], Filter: semantic.Filter{Keywords: map[string]string{"kind": "service"}}, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, h := range hits {
		if n, ok := h.Metadata["service_name"].(string); ok && n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// RunbookSearch searches the runbook corpus with semantic and keyword fallback.
func (srv *Service) RunbookSearch(ctx context.Context, query knowledge.RunbookQuery) map[string]any {
	result, err := srv.RunbookSearchResult(ctx, query)
	if err != nil {
		return map[string]any{"matches": nil, "semantic": false, "error": err.Error()}
	}
	return result
}

// RunbookSearchResult returns the runbook payload without hiding store failures.
func (srv *Service) RunbookSearchResult(ctx context.Context, query knowledge.RunbookQuery) (map[string]any, error) {
	query.Limit = clampInt(query.Limit, 1, 10)
	result, err := srv.FindRunbooks(ctx, query)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"matches": result.Matches, "semantic": result.Semantic,
		"docScoped": result.DocScoped, "truncated": result.Truncated,
	}, nil
}

// FindRunbooks returns typed runbook matches for internal consumers.
func (srv *Service) FindRunbooks(ctx context.Context, query knowledge.RunbookQuery) (result domain.RunbookSearchResult, resultErr error) {
	started := time.Now()
	if domain.TraceEnabled(ctx) {
		defer func() {
			status := "completed"
			output := map[string]any{"matches": len(result.Matches), "semantic": result.Semantic, "runbooks": traceRunbookNames(result.Matches)}
			if resultErr != nil {
				status = "failed"
				output["error"] = resultErr.Error()
			}
			domain.RecordTrace(ctx, domain.EvaluationTrace{
				Node: "runbook_search", Status: status, DurationMS: time.Since(started).Milliseconds(),
				Input: map[string]any{"query": query.Query, "limit": query.Limit, "doc_id": query.DocID}, Output: output,
			})
		}()
	}
	if srv.docStore == nil {
		return domain.RunbookSearchResult{Matches: []domain.RunbookSearchHit{}, DocScoped: query.DocID != ""}, nil
	}
	if srv.semanticEnabled() {
		var meta domain.RunbookRecord
		if query.DocID != "" {
			var err error
			meta, err = srv.docStore.RunbookMetaByID(query.DocID)
			if err != nil {
				return domain.RunbookSearchResult{}, fmt.Errorf("runbook_not_found: doc_id %q: %w", query.DocID, err)
			}
		}
		vecs, err := srv.embedder.Embed(ctx, []string{query.Query})
		if err != nil {
			return domain.RunbookSearchResult{}, fmt.Errorf("embed runbook query: %w", err)
		}
		if len(vecs) == 0 {
			return domain.RunbookSearchResult{}, fmt.Errorf("embed runbook query: empty vector")
		}
		keywords := map[string]string{"kind": "runbook"}
		fetchLimit := max(query.Limit*4, 12)
		if query.DocID != "" {
			keywords["doc_id"] = query.DocID
			fetchLimit = query.Limit + 1
		}
		hits, err := srv.semantic.Search(ctx, semantic.Query{
			DenseVector: vecs[0], Filter: semantic.Filter{Keywords: keywords}, Limit: fetchLimit,
		})
		if err != nil {
			return domain.RunbookSearchResult{}, fmt.Errorf("search runbooks: %w", err)
		}
		return runbookResultFromHits(hits, meta, query), nil
	} else {
		log.InfofCtx(ctx, "[runbook_search] semantic disabled (Semantic=%v Embedder=%v) → keyword fallback", srv.semantic != nil, srv.embedder != nil)
	}
	var all []domain.RunbookRecord
	if query.DocID != "" {
		meta, metaErr := srv.docStore.RunbookMetaByID(query.DocID)
		if metaErr != nil {
			return domain.RunbookSearchResult{}, fmt.Errorf("runbook_not_found: doc_id %q: %w", query.DocID, metaErr)
		}
		full, fullErr := srv.docStore.RunbookByID(meta.ID)
		if fullErr != nil {
			return domain.RunbookSearchResult{}, fmt.Errorf("load runbook %q: %w", meta.ID, fullErr)
		}
		all = []domain.RunbookRecord{full}
	} else {
		var err error
		all, err = srv.docStore.SearchRunbooksKeyword(query.Query, query.Limit*2)
		if err != nil {
			return domain.RunbookSearchResult{}, fmt.Errorf("keyword search runbooks: %w", err)
		}
	}
	seed := scoreRunbooks(all, query.Query, query.Limit*2)
	if len(seed) == 0 {
		return domain.RunbookSearchResult{Matches: []domain.RunbookSearchHit{}, DocScoped: query.DocID != ""}, nil
	}
	q := platform.Normalize(query.Query)
	scored := make([]scoredRunbook, 0, len(seed))
	for _, rb := range seed {
		if sc := scoreRunbook(rb, q); sc > 0 {
			scored = append(scored, scoredRunbook{rb, sc})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	n := min(len(scored), query.Limit)
	out := make([]domain.RunbookSearchHit, n)
	for i := range n {
		rb := scored[i].rec
		evidenceClass, trustTier := domain.EvidenceForRunbookScope(rb.Scope)
		text := rb.Text
		if runes := []rune(text); len(runes) > 4000 {
			text = string(runes[:4000])
		}
		out[i] = domain.RunbookSearchHit{
			DocID: rb.ID, Title: rb.Title, Path: rb.Path, DocKind: rb.Scope,
			EvidenceClass: evidenceClass, TrustTier: trustTier,
			Chunks: []domain.RunbookChunk{{ChunkText: text}},
		}
	}
	return domain.RunbookSearchResult{Matches: out, DocScoped: query.DocID != ""}, nil
}

func traceRunbookNames(matches []domain.RunbookSearchHit) []string {
	limit := min(len(matches), 10)
	names := make([]string, 0, limit)
	for _, match := range matches[:limit] {
		names = append(names, match.Title)
	}
	return names
}

// filterRunbooksByScope returns runbooks whose scope field equals scope.
func filterRunbooksByScope(all []domain.RunbookRecord, scope string) []domain.RunbookRecord {
	out := make([]domain.RunbookRecord, 0, len(all))
	for _, rb := range all {
		if rb.Scope == scope {
			out = append(out, rb)
		}
	}
	return out
}

func (srv *Service) CodeSearch(ctx context.Context, query, lang string, limit int) map[string]any {
	result, err := srv.CodeSearchResult(ctx, query, lang, limit)
	if err != nil {
		return map[string]any{"matches": []any{}, "error": err.Error()}
	}
	return result
}

// CodeSearchResult returns the code search payload without hiding backend failures.
func (srv *Service) CodeSearchResult(ctx context.Context, query, lang string, limit int) (map[string]any, error) {
	limit = clampInt(limit, 1, 100)
	result, err := srv.FindCode(ctx, query, lang, limit)
	if err != nil {
		return nil, err
	}
	matches := make([]any, 0, len(result.Matches))
	for _, hit := range result.Matches {
		match := map[string]any{
			"path": hit.Path, "lang": hit.Lang, "repo": hit.Repo, "layer": hit.Layer,
			"startLine": hit.StartLine, "endLine": hit.EndLine, "text": hit.Text, "preview": hit.Preview,
			"score": hit.Score, "scoreKind": hit.ScoreKind,
			"evidenceClass": hit.EvidenceClass, "trustTier": hit.TrustTier,
		}
		if hit.ScoreKind == string(semantic.ScoreFusion) {
			match["fusionScore"] = hit.FusionScore
		} else {
			match["semanticScore"] = hit.SemanticScore
		}
		matches = append(matches, match)
	}
	return map[string]any{"matches": matches, "semantic": result.Semantic}, nil
}

// SearchCode exposes typed code search to built-in and scenario tools.
func (srv *Service) SearchCode(ctx context.Context, query knowledge.CodeSearchQuery) (knowledge.CodeSearchResult, error) {
	query.Limit = clampInt(query.Limit, 1, 100)
	found, err := srv.FindCode(ctx, query.Query, query.Lang, query.Limit)
	if err != nil {
		return knowledge.CodeSearchResult{}, err
	}
	return toCodeSearchResult(found), nil
}

// TraceDependencies exposes evidence-backed ontology dependencies without tool JSON.
func (srv *Service) TraceDependencies(ctx context.Context, query knowledge.DependencyQuery) (knowledge.DependencyResult, error) {
	if query.Direction == "" {
		query.Direction = "both"
	}
	depth := clampInt(query.Depth, 1, 5)
	result, err := srv.TraceDeps(ctx, query.Service, query.Direction, depth)
	if err != nil {
		return knowledge.DependencyResult{}, err
	}
	return toDependencyResult(result), nil
}

// SearchRunbooks exposes typed runbook search to extensions.
func (srv *Service) SearchRunbooks(ctx context.Context, query knowledge.RunbookQuery) (knowledge.RunbookSearchResult, error) {
	query.Limit = clampInt(query.Limit, 1, 10)
	found, err := srv.FindRunbooks(ctx, query)
	if err != nil {
		return knowledge.RunbookSearchResult{}, err
	}
	return toRunbookSearchResult(found), nil
}

// SearchServices exposes typed service search to extensions.
func (srv *Service) SearchServices(ctx context.Context, query knowledge.ServiceSearchQuery) (knowledge.ServiceSearchResult, error) {
	query.Limit = clampInt(query.Limit, 1, 100)
	found, err := srv.FindServices(ctx, query.Query, query.Limit)
	if err != nil {
		return knowledge.ServiceSearchResult{}, err
	}
	return toServiceSearchResult(found), nil
}

// toCodeSearchResult maps the internal typed answer onto the stable public contract.
func toCodeSearchResult(found domain.SearchResult[domain.CodeSearchHit]) knowledge.CodeSearchResult {
	matches := make([]knowledge.CodeSearchHit, 0, len(found.Matches))
	for _, hit := range found.Matches {
		matches = append(matches, knowledge.CodeSearchHit{
			Path:          hit.Path,
			Lang:          hit.Lang,
			Repo:          hit.Repo,
			StartLine:     hit.StartLine,
			EndLine:       hit.EndLine,
			Preview:       hit.Preview,
			Score:         hit.Score,
			ScoreKind:     hit.ScoreKind,
			EvidenceClass: hit.EvidenceClass,
			TrustTier:     hit.TrustTier,
		})
	}
	return knowledge.CodeSearchResult{Matches: matches, Semantic: found.Semantic}
}

// toRunbookSearchResult maps the internal typed answer onto the stable public contract.
func toRunbookSearchResult(found domain.RunbookSearchResult) knowledge.RunbookSearchResult {
	matches := make([]knowledge.RunbookSearchHit, 0, len(found.Matches))
	for _, hit := range found.Matches {
		chunks := make([]knowledge.RunbookChunk, 0, len(hit.Chunks))
		for _, chunk := range hit.Chunks {
			chunks = append(chunks, knowledge.RunbookChunk{
				ChunkIndex: chunk.ChunkIndex, SectionHeader: chunk.SectionHeader,
				ChunkText: chunk.ChunkText, SemanticScore: chunk.SemanticScore,
			})
		}
		matches = append(matches, knowledge.RunbookSearchHit{
			DocID: hit.DocID, Title: hit.Title, Path: hit.Path, DocKind: hit.DocKind,
			EvidenceClass: hit.EvidenceClass, TrustTier: hit.TrustTier, Chunks: chunks,
		})
	}
	return knowledge.RunbookSearchResult{
		Matches: matches, Semantic: found.Semantic,
		DocScoped: found.DocScoped, Truncated: found.Truncated,
	}
}

// toServiceSearchResult maps the internal typed answer onto the stable public contract.
func toServiceSearchResult(found domain.SearchResult[domain.ServiceRecord]) knowledge.ServiceSearchResult {
	matches := make([]knowledge.ServiceRecord, 0, len(found.Matches))
	for _, svc := range found.Matches {
		matches = append(matches, knowledge.ServiceRecord{
			ServiceName: svc.ServiceName,
			Repo:        svc.Repo,
			Layer:       svc.Layer,
			Language:    svc.Language,
			Owner:       svc.Owner,
			Status:      svc.Status,
			Summary:     svc.Summary,
			Tags:        svc.Tags,
			Docs:        svc.Docs,
			Confidence:  svc.Confidence,
		})
	}
	return knowledge.ServiceSearchResult{Matches: matches, Semantic: found.Semantic}
}

// toDependencyResult maps the internal ontology answer onto the stable public contract.
func toDependencyResult(found domain.DependencyTrace) knowledge.DependencyResult {
	conv := func(edges []domain.DependencyEdge) []knowledge.DependencyEdge {
		out := make([]knowledge.DependencyEdge, 0, len(edges))
		for _, edge := range edges {
			out = append(out, knowledge.DependencyEdge{
				From:           edge.From,
				To:             edge.To,
				Type:           string(edge.Type),
				ExternalTarget: edge.ExternalTarget,
				Confidence:     edge.Confidence,
			})
		}
		return out
	}
	return knowledge.DependencyResult{
		Service: found.Service, Candidates: found.Candidates,
		Upstream: conv(found.Upstream), Downstream: conv(found.Downstream), Truncated: found.Truncated,
	}
}

// FindCode searches indexed code and returns typed hits for internal consumers.
func (srv *Service) FindCode(ctx context.Context, query, lang string, limit int) (domain.SearchResult[domain.CodeSearchHit], error) {
	traceEnabled := domain.TraceEnabled(ctx)
	if limit <= 0 {
		limit = 10
	}
	if !srv.semanticEnabled() {
		return domain.SearchResult[domain.CodeSearchHit]{}, fmt.Errorf("search_code requires semantic search and embedding configuration")
	}
	embedStarted := time.Now()
	vecs, err := srv.embedder.Embed(ctx, []string{query})
	if err != nil || len(vecs) == 0 {
		if traceEnabled {
			domain.RecordTrace(ctx, domain.EvaluationTrace{
				Node: "query_embedding", Status: "failed", DurationMS: time.Since(embedStarted).Milliseconds(),
				Input: map[string]any{"query": query}, Output: map[string]any{"error": errString(err), "vectors": len(vecs)},
			})
		}
		log.ErrorfCtx(ctx, "[search_code] embed failed: err=%v vecs=%d", err, len(vecs))
		return domain.SearchResult[domain.CodeSearchHit]{}, fmt.Errorf("embed query: %s", errString(err))
	}
	if traceEnabled {
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "query_embedding", DurationMS: time.Since(embedStarted).Milliseconds(),
			Input: map[string]any{"query": query}, Output: map[string]any{"vectors": len(vecs), "dimensions": len(vecs[0])},
		})
	}
	filters := map[string]string{"kind": "code_chunk"}
	if lang != "" {
		filters["lang"] = lang
	}
	fetchLimit := max(limit*3, limit)
	var hits []semantic.Hit
	mode := "dense"
	sparseTerms := 0
	searchStarted := time.Now()
	if bm := srv.BM25View(); bm != nil {
		sparseStarted := time.Now()
		sv := bm.QuerySparse(query)
		indices, values := retrieval.SparseToSorted(sv)
		sparseTerms = len(indices)
		if traceEnabled {
			domain.RecordTrace(ctx, domain.EvaluationTrace{
				Node: "sparse_query", DurationMS: time.Since(sparseStarted).Milliseconds(),
				Input: map[string]any{"query": query}, Output: map[string]any{"terms": sparseTerms, "known_terms": sparseTerms > 0},
			})
		}
		if len(indices) == 0 {
			log.InfofCtx(ctx, "[search_code] dense fallback: query has no known BM25 terms, dim=%d", len(vecs[0]))
			hits, err = srv.semantic.Search(ctx, semantic.Query{
				DenseVector: vecs[0], Filter: semantic.Filter{Keywords: filters}, Limit: fetchLimit, GroupBy: "path",
			})
		} else {
			mode = "hybrid"
			log.InfofCtx(ctx, "[search_code] hybrid: dim=%d sparseTerms=%d", len(vecs[0]), len(indices))
			hits, err = srv.semantic.Search(ctx, semantic.Query{
				DenseVector: vecs[0], SparseVector: &semantic.SparseVector{Indices: indices, Values: values},
				Filter: semantic.Filter{Keywords: filters}, Limit: fetchLimit, GroupBy: "path",
			})
		}
	} else {
		srv.denseWarnOnce.Do(func() {
			log.WarnfCtx(ctx, "[search_code] hybrid search disabled (BM25 nil) — running dense-only; run the full code embedding operation to enable it")
		})
		log.InfofCtx(ctx, "[search_code] dense-only: dim=%d (BM25 nil)", len(vecs[0]))
		hits, err = srv.semantic.Search(ctx, semantic.Query{
			DenseVector: vecs[0], Filter: semantic.Filter{Keywords: filters}, Limit: fetchLimit, GroupBy: "path",
		})
	}
	if traceEnabled {
		searchStatus := "completed"
		searchOutput := map[string]any{"hits": len(hits), "top": traceSemanticHits(hits)}
		if err != nil {
			searchStatus = "failed"
			searchOutput["error"] = err.Error()
		}
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "vector_search", Status: searchStatus, DurationMS: time.Since(searchStarted).Milliseconds(),
			Input:  map[string]any{"mode": mode, "fetch_limit": fetchLimit, "sparse_terms": sparseTerms, "filters": filters},
			Output: searchOutput,
		})
	}
	if err != nil {
		log.ErrorfCtx(ctx, "[search_code] semantic search failed: %v", err)
		return domain.SearchResult[domain.CodeSearchHit]{}, err
	}
	log.InfofCtx(ctx, "[search_code] semantic backend returned %d hits before filtering", len(hits))
	// Rank by raw semantic similarity, not trust-adjusted score.
	// Trust matters later during reranking, not at recall time.
	// A high-trust low-relevance hit should not displace a relevant one here.
	sort.SliceStable(hits, func(i, j int) bool {
		return hits[i].Score > hits[j].Score
	})
	matches := make([]domain.CodeSearchHit, 0, len(hits))
	// One chunk per file: hits are score-sorted so first-seen is the best match.
	// Recalling multiple windows of one long doc floods the pool and starves
	// unique sources; rerank's dedupBySource collapses them to one before
	// scoring anyway, so dedup here keeps recall diverse from the start.
	seenFile := make(map[string]struct{}, len(hits))
	for _, h := range hits {
		path, _ := h.Metadata["path"].(string)
		// Skip Nasuta-generated artifacts until they are filtered at index time.
		if strings.Contains(path, "/"+platform.WorkspaceMetadataDir) || strings.HasPrefix(path, platform.WorkspaceMetadataDir) {
			continue
		}
		if _, dup := seenFile[path]; dup {
			continue
		}
		seenFile[path] = struct{}{}
		evidenceClass, trustTier := evidenceFromCodeHit(h)
		adjusted := domain.TrustAdjustedScore(float64(h.Score), trustTier)
		match := domain.CodeSearchHit{
			Path: path, Lang: payloadString(h.Metadata, "lang"), Repo: payloadString(h.Metadata, "repo"), Layer: payloadString(h.Metadata, "layer"),
			StartLine: payloadInt(h.Metadata["start_line"]), EndLine: payloadInt(h.Metadata["end_line"]),
			Text: payloadString(h.Metadata, "text"), Preview: payloadString(h.Metadata, "preview"),
			Score: adjusted, ScoreKind: string(h.ScoreKind), EvidenceClass: evidenceClass, TrustTier: trustTier,
		}
		if h.ScoreKind == semantic.ScoreFusion {
			fusionScore := h.FusionScore
			if fusionScore == 0 {
				fusionScore = h.Score
			}
			match.FusionScore = float64(fusionScore)
		} else {
			denseScore := h.DenseScore
			if denseScore == 0 {
				denseScore = h.Score
			}
			match.SemanticScore = float64(denseScore)
		}
		matches = append(matches, match)
		if len(matches) >= limit {
			break
		}
	}
	log.InfofCtx(ctx, "[search_code] dedup by file: %d hits -> %d files", len(hits), len(matches))
	if traceEnabled {
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: "file_dedup", Input: map[string]any{"hits": len(hits), "limit": limit},
			Output: map[string]any{"files": len(matches), "top": traceCodeMatches(matches)},
		})
	}
	return domain.SearchResult[domain.CodeSearchHit]{Matches: matches, Semantic: true}, nil
}

func traceSemanticHits(hits []semantic.Hit) []map[string]any {
	limit := min(len(hits), 10)
	items := make([]map[string]any, 0, limit)
	for _, hit := range hits[:limit] {
		items = append(items, map[string]any{
			"path": payloadString(hit.Metadata, "path"), "score": hit.Score,
			"dense_score": hit.DenseScore, "fusion_score": hit.FusionScore, "score_kind": hit.ScoreKind,
		})
	}
	return items
}

func traceCodeMatches(matches []domain.CodeSearchHit) []map[string]any {
	limit := min(len(matches), 10)
	items := make([]map[string]any, 0, limit)
	for _, match := range matches[:limit] {
		items = append(items, map[string]any{
			"path": match.Path, "repo": match.Repo, "score": match.Score,
			"semantic_score": match.SemanticScore, "fusion_score": match.FusionScore, "score_kind": match.ScoreKind,
		})
	}
	return items
}

func payloadString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func payloadInt(value any) int { return trustTierFromPayload(value) }

func evidenceFromCodeHit(h semantic.Hit) (string, int) {
	class, _ := h.Metadata["evidence_class"].(string)
	trust := trustTierFromPayload(h.Metadata["trust_tier"])
	if class != "" && trust > 0 {
		return class, trust
	}
	lang, _ := h.Metadata["lang"].(string)
	repo, _ := h.Metadata["repo"].(string)
	return domain.EvidenceForCodeChunk(lang, repo)
}

func trustTierFromPayload(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (srv *Service) TraceDeps(ctx context.Context, service, direction string, depth int) (domain.DependencyTrace, error) {
	if srv.ontology == nil {
		return domain.DependencyTrace{}, ontology.ErrUnavailable
	}
	dir := ontology.DirectionBoth
	switch direction {
	case "upstream":
		dir = ontology.DirectionIncoming
	case "downstream":
		dir = ontology.DirectionOutgoing
	case "both":
	default:
		return domain.DependencyTrace{}, fmt.Errorf("unsupported dependency direction %q", direction)
	}
	result, err := srv.ontology.TraceDependencies(ctx, ontology.DependencyQuery{
		Service: service, Direction: dir, MaxDepth: depth, MaxNodes: 500, MaxFanout: 100,
	})
	if err != nil {
		return domain.DependencyTrace{}, err
	}
	return dependencyTrace(result), nil
}

func dependencyTrace(result ontology.DependencyResult) domain.DependencyTrace {
	convert := func(facts []ontology.RelationFact) []domain.DependencyEdge {
		edges := make([]domain.DependencyEdge, 0, len(facts))
		for _, fact := range facts {
			evidence := make([]domain.Evidence, 0, len(fact.Evidence))
			for _, item := range fact.Evidence {
				evidence = append(evidence, domain.Evidence{
					Path: item.Path, Line: item.Line, Symbol: item.Symbol, Kind: domain.SourceKind(item.Source),
				})
			}
			edge := domain.DependencyEdge{
				CallerServiceKey: fact.Subject.ID, From: fact.Subject.Name, To: fact.Object.Name,
				Type: domain.EdgeType(fact.Qualifiers["protocol"]), Evidence: evidence, Confidence: fact.Confidence,
			}
			if fact.Object.Class == ontology.ClassExternalSystem {
				edge.TargetKind = domain.DependencyTargetExternal
				edge.ExternalTarget = fact.Object.Name
			} else {
				edge.TargetKind = domain.DependencyTargetService
				edge.TargetServiceKey = fact.Object.ID
			}
			edges = append(edges, edge)
		}
		return edges
	}
	trace := domain.DependencyTrace{
		Upstream: convert(result.Upstream), Downstream: convert(result.Downstream), Truncated: result.Truncated,
	}
	if result.Root != nil {
		trace.Service = result.Root.Name
	}
	trace.Candidates = make([]string, 0, len(result.Candidates))
	for _, candidate := range result.Candidates {
		trace.Candidates = append(trace.Candidates, candidate.Name)
	}
	return trace
}

func (srv *Service) ListApis(ctx context.Context, service, keyword string, limit int) map[string]any {
	result, err := srv.ListApisResult(ctx, service, keyword, limit)
	if err != nil {
		return map[string]any{"matches": nil, "error": err.Error()}
	}
	return result
}

// ListApisResult returns indexed APIs without hiding storage failures.
func (srv *Service) ListApisResult(ctx context.Context, service, keyword string, limit int) (map[string]any, error) {
	limit = clampInt(limit, 1, 100)
	matches, err := srv.FindAPIs(ctx, service, keyword, limit)
	if err != nil {
		return nil, err
	}
	return map[string]any{"matches": matches}, nil
}

// FindAPIs returns typed endpoint records for internal consumers.
func (srv *Service) FindAPIs(ctx context.Context, service, keyword string, limit int) ([]domain.EndpointRecord, error) {
	page, err := srv.db.ListApis(ctx, service, keyword, 1, limit)
	if err != nil || page == nil {
		return nil, err
	}
	return page.List, nil
}

func (srv *Service) DocGapCheck(ctx context.Context, serviceName string) map[string]any {
	result, err := srv.DocGapCheckResult(ctx, serviceName)
	if err != nil {
		return map[string]any{"service": serviceName, "found": false, "error": err.Error()}
	}
	return result
}

// DocGapCheckResult reports documentation gaps without folding store failures into data.
func (srv *Service) DocGapCheckResult(ctx context.Context, serviceName string) (map[string]any, error) {
	all, err := srv.services(ctx)
	if err != nil {
		return nil, err
	}
	var svc domain.ServiceRecord
	found := false
	for _, candidate := range all {
		if candidate.ServiceName == serviceName {
			svc = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("service_not_found: service %q", serviceName)
	}
	endpoints, err := srv.db.EndpointCountFor(ctx, svc.ServiceName)
	if err != nil {
		return nil, fmt.Errorf("endpoint count for %q: %w", svc.ServiceName, err)
	}
	outgoing, err := srv.db.OutgoingCountFor(ctx, svc.ServiceName)
	if err != nil {
		return nil, fmt.Errorf("outgoing dependency count for %q: %w", svc.ServiceName, err)
	}

	missing := []string{}
	if len(svc.Docs) == 0 {
		missing = append(missing, "service-doc")
	}
	if len(svc.Entrypoints) == 0 {
		missing = append(missing, "entrypoints")
	}
	if endpoints == 0 {
		missing = append(missing, "endpoints")
	}
	if outgoing == 0 {
		missing = append(missing, "dependencies")
	}
	if len(svc.SourceOfTruth) == 0 {
		missing = append(missing, "source_of_truth")
	}
	res := map[string]any{
		"service": svc.ServiceName,
		"found":   true,
		"missing": missing,
		"counts": map[string]int{
			"docs":                 len(svc.Docs),
			"entrypoints":          len(svc.Entrypoints),
			"endpoints":            endpoints,
			"outgoingDependencies": outgoing,
		},
	}
	return res, nil
}

func (srv *Service) IndexSummary(ctx context.Context) map[string]any {
	result, err := srv.IndexSummaryResult(ctx)
	if err != nil {
		return map[string]any{
			"services": 0, "endpoints": 0, "dependencies": 0, "runbooks": 0, "repos": 0,
			"semanticEnabled": srv.semanticEnabled(), "error": err.Error(),
		}
	}
	return result
}

// IndexSummaryResult returns index health without hiding configured backend failures.
func (srv *Service) IndexSummaryResult(ctx context.Context) (map[string]any, error) {
	sm, err := srv.db.Summary(ctx)
	if err != nil {
		return nil, err
	}
	runbooks, err := srv.runbookCount()
	if err != nil {
		return nil, fmt.Errorf("count runbooks: %w", err)
	}
	result := map[string]any{
		"services":        sm.Services,
		"endpoints":       sm.Endpoints,
		"dependencies":    sm.Dependencies,
		"runbooks":        runbooks,
		"repos":           sm.Repos,
		"semanticEnabled": srv.semanticEnabled(),
	}
	if srv.ontology == nil {
		result["ontology"] = map[string]any{"enabled": false, "status": "unavailable"}
		return result, nil
	}
	stats, ontologyErr := srv.ontology.Stats(ctx)
	if ontologyErr != nil {
		return nil, fmt.Errorf("ontology stats: %w", ontologyErr)
	}
	result["ontology"] = map[string]any{
		"enabled": true, "status": "available", "generation": stats.Generation,
		"entities": stats.Entities, "facts": stats.Facts, "evidence": stats.Evidence,
	}
	return result, nil
}

func (srv *Service) runbookCount() (int, error) {
	if srv.docStore == nil {
		return 0, nil
	}
	n, err := srv.docStore.CountRunbooks()
	if err != nil {
		return 0, err
	}
	return n, nil
}

type scoredService struct {
	rec   domain.ServiceRecord
	score int
}

func scoreServices(all []domain.ServiceRecord, query string, limit int) []domain.ServiceRecord {
	q := platform.Normalize(query)
	var scored []scoredService
	for _, svc := range all {
		if sc := scoreService(svc, q); sc > 0 {
			scored = append(scored, scoredService{svc, sc})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	n := min(len(scored), limit)
	out := make([]domain.ServiceRecord, n)
	for i := range n {
		out[i] = scored[i].rec
	}
	return out
}

func scoreService(svc domain.ServiceRecord, q string) int {
	// Exact service name match is the strongest signal — check it first
	// before normalizing all other fields.
	if platform.Normalize(svc.ServiceName) == q {
		return 100
	}
	fields := []string{svc.ServiceName, svc.ModulePath, svc.Layer, svc.Owner}
	fields = append(fields, svc.Tags...)
	fields = append(fields, svc.Docs...)
	norm := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			norm = append(norm, platform.Normalize(f))
		}
	}
	for _, f := range norm {
		if f == q {
			return 80
		}
	}
	for _, f := range norm {
		if strings.Contains(f, q) {
			return 50
		}
	}
	if svc.Summary != "" && strings.Contains(platform.Normalize(svc.Summary), q) {
		return 20
	}
	return 0
}

type scoredRunbook struct {
	rec   domain.RunbookRecord
	score int
}

func scoreRunbooks(all []domain.RunbookRecord, query string, limit int) []domain.RunbookRecord {
	q := platform.Normalize(query)
	var scored []scoredRunbook
	for _, rb := range all {
		if sc := scoreRunbook(rb, q); sc > 0 {
			scored = append(scored, scoredRunbook{rb, sc})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	n := min(len(scored), limit)
	out := make([]domain.RunbookRecord, n)
	for i := range n {
		out[i] = scored[i].rec
	}
	return out
}

func scoreRunbook(rb domain.RunbookRecord, q string) int {
	fields := []string{rb.ID, rb.Title, rb.Path, rb.Scope}
	fields = append(fields, rb.Tags...)
	for _, f := range fields {
		if f != "" && strings.Contains(platform.Normalize(f), q) {
			return 80
		}
	}
	body := platform.Normalize(rb.Text)
	if strings.Contains(body, q) {
		return 30
	}
	score := 0
	for _, token := range strings.Split(q, "-") {
		if len(token) > 2 && strings.Contains(body, token) {
			score += 10
		}
	}
	return score
}

// mergeServiceMatches appends semantic-only hits to keyword matches for extra recall.
func mergeServiceMatches(semNames []string, all, base []domain.ServiceRecord, limit int) []domain.ServiceRecord {
	byName := map[string]domain.ServiceRecord{}
	for _, s := range all {
		byName[s.ServiceName] = s
	}
	seen := map[string]struct{}{}
	out := []domain.ServiceRecord{}
	for _, svc := range base {
		out = append(out, svc)
		seen[svc.ServiceName] = struct{}{}
	}
	for _, n := range semNames {
		if _, dup := seen[n]; dup {
			continue
		}
		if svc, ok := byName[n]; ok {
			out = append(out, svc)
			seen[n] = struct{}{}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func runbookResultFromHits(hits []semantic.Hit, meta domain.RunbookRecord, query knowledge.RunbookQuery) domain.RunbookSearchResult {
	result := domain.RunbookSearchResult{
		Matches: []domain.RunbookSearchHit{}, Semantic: true, DocScoped: query.DocID != "",
	}
	if query.DocID != "" {
		seen := make(map[int]struct{}, query.Limit+1)
		chunks := make([]domain.RunbookChunk, 0, query.Limit)
		for _, hit := range hits {
			index := intFromPayload(hit.Metadata["chunk_index"])
			if _, ok := seen[index]; ok {
				continue
			}
			seen[index] = struct{}{}
			if len(chunks) == query.Limit {
				result.Truncated = true
				break
			}
			chunks = append(chunks, runbookChunkFromHit(hit, index))
		}
		sort.Slice(chunks, func(i, j int) bool { return chunks[i].ChunkIndex < chunks[j].ChunkIndex })
		if len(chunks) > 0 {
			class, trust := runbookEvidence(semantic.Hit{}, meta)
			result.Matches = append(result.Matches, domain.RunbookSearchHit{
				DocID: meta.ID, Title: meta.Title, Path: meta.Path, DocKind: meta.Scope,
				EvidenceClass: class, TrustTier: trust, Chunks: chunks,
			})
		}
		return result
	}

	positions := make(map[string]int, query.Limit)
	for _, hit := range hits {
		docID, _ := hit.Metadata["doc_id"].(string)
		if docID == "" {
			continue
		}
		if position, ok := positions[docID]; ok {
			if hit.Score > float32(result.Matches[position].Chunks[0].SemanticScore) {
				result.Matches[position].Chunks[0] = runbookChunkFromHit(hit, intFromPayload(hit.Metadata["chunk_index"]))
			}
			continue
		}
		if len(result.Matches) == query.Limit {
			continue
		}
		title, _ := hit.Metadata["title"].(string)
		path, _ := hit.Metadata["path"].(string)
		kind, _ := hit.Metadata["scope"].(string)
		if kind == "" {
			kind, _ = hit.Metadata["doc_kind"].(string)
		}
		class, trust := runbookEvidence(hit, domain.RunbookRecord{Scope: kind})
		positions[docID] = len(result.Matches)
		result.Matches = append(result.Matches, domain.RunbookSearchHit{
			DocID: docID, Title: title, Path: path, DocKind: kind,
			EvidenceClass: class, TrustTier: trust,
			Chunks: []domain.RunbookChunk{runbookChunkFromHit(hit, intFromPayload(hit.Metadata["chunk_index"]))},
		})
	}
	return result
}

func runbookChunkFromHit(hit semantic.Hit, index int) domain.RunbookChunk {
	text, _ := hit.Metadata["text"].(string)
	section, _ := hit.Metadata["section_header"].(string)
	return domain.RunbookChunk{
		ChunkIndex: index, SectionHeader: section,
		ChunkText: text, SemanticScore: float64(hit.Score),
	}
}

func intFromPayload(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func runbookEvidence(h semantic.Hit, rb domain.RunbookRecord) (string, int) {
	class, _ := h.Metadata["evidence_class"].(string)
	trust := trustTierFromPayload(h.Metadata["trust_tier"])
	if class != "" && trust > 0 {
		return class, trust
	}
	scope := rb.Scope
	if scope == "" {
		scope, _ = h.Metadata["scope"].(string)
	}
	return domain.EvidenceForRunbookScope(scope)
}

// GetSymbol searches codegraph symbols and loads their source snippets.
func (srv *Service) GetSymbol(ctx context.Context, query string, limit int) map[string]any {
	return srv.GetSymbolFiltered(ctx, query, "", "", limit)
}

// GetSymbolFiltered applies explicit file and qualified-name disambiguation.
func (srv *Service) GetSymbolFiltered(ctx context.Context, query, file, qualifiedName string, limit int) map[string]any {
	result, err := srv.GetSymbolResult(ctx, query, file, qualifiedName, limit)
	if err != nil {
		return map[string]any{"matches": nil, "error": err.Error()}
	}
	return result
}

// GetSymbolResult queries codegraph without hiding availability or query failures.
func (srv *Service) GetSymbolResult(ctx context.Context, query, file, qualifiedName string, limit int) (map[string]any, error) {
	root := srv.workspaceRoot
	if root == "" {
		return nil, fmt.Errorf("codegraph: no workspace root configured")
	}
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}

	// Open codegraph SQLite (read-only). Graceful nil when DB not yet built.
	db, err := codegraph.Open(root)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("codegraph not indexed")
	}
	defer db.Close()

	nodes, err := db.SearchSymbols(ctx, codegraph.SymbolQuery{
		Terms: symbolQueryTokens(query), PathPrefixes: nonEmptyStrings(file), Limit: limit * 4,
	})
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return map[string]any{"matches": []any{}}, nil
	}

	// Build results with source from file system.
	matches := make([]any, 0, limit)
	added := 0
	for _, n := range nodes {
		if added >= limit {
			break
		}
		// Skip non-callable kinds.
		switch n.Kind {
		case "field", "import", "namespace", "file", "constant":
			continue
		}
		if qualifiedName != "" && !strings.EqualFold(n.QualifiedName, qualifiedName) {
			continue
		}
		source := readNodeSource(root, n)
		matches = append(matches, map[string]any{
			"id": n.ID, "function": n.Name, "qualifiedName": n.QualifiedName,
			"kind": n.Kind, "file": n.FilePath, "line": n.StartLine, "source": source,
		})
		added++
	}
	return map[string]any{"matches": matches}, nil
}

func nonEmptyStrings(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

// TraceCalls resolves a symbol and walks its callers or callees.
func (srv *Service) TraceCalls(ctx context.Context, request callchain.Request) map[string]any {
	result, err := srv.TraceCallsResult(ctx, request)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return result
}

// TraceCallsResult walks codegraph call edges without hiding broken prerequisites.
func (srv *Service) TraceCallsResult(ctx context.Context, request callchain.Request) (map[string]any, error) {
	if srv.callChain == nil || !srv.callChain.Available() {
		return nil, fmt.Errorf("call chain unavailable: codegraph or structure index is not ready")
	}
	var err error
	request, err = srv.resolveAPICallTarget(ctx, request)
	if err != nil {
		return nil, err
	}
	result, err := srv.callChain.Trace(ctx, request)
	if err != nil {
		return nil, err
	}
	return callChainResult(srv.workspaceRoot, result), nil
}

func (srv *Service) resolveAPICallTarget(ctx context.Context, request callchain.Request) (callchain.Request, error) {
	if srv.ontology == nil || request.Query == "" || request.File != "" || request.Line > 0 || request.QualifiedName != "" {
		return request, nil
	}
	result, err := srv.ontology.QueryRelations(ctx, ontology.RelationQuery{
		Entity: request.Query, EntityClass: ontology.ClassAPIEndpoint,
		Predicates: []ontology.Predicate{ontology.PredicateImplementedBy}, Direction: ontology.DirectionOutgoing,
		MaxDepth: 1, MaxNodes: 20, MaxFanout: 20,
	})
	if err != nil {
		return request, fmt.Errorf("resolve API call target: %w", err)
	}
	if result.Root == nil || len(result.Facts) != 1 {
		return request, nil
	}
	fact := result.Facts[0]
	request.Query = fact.Object.Name
	request.QualifiedName = fact.Object.Name
	if len(fact.Evidence) > 0 {
		request.File = fact.Evidence[0].Path
		request.Line = fact.Evidence[0].Line
	}
	return request, nil
}

func callChainResult(root string, result callchain.Result) map[string]any {
	const sourceCap = 1500
	decorate := func(direction callchain.DirectionResult, callers bool) map[string]any {
		nodes := make([]map[string]any, 0, len(direction.Hops))
		for _, hop := range direction.Hops {
			node := hop.Target
			if callers {
				node = hop.Source
			}
			source := readNodeSource(root, node.Node)
			if len(source) > sourceCap {
				source = source[:sourceCap] + "\n...(truncated)"
			}
			nodes = append(nodes, map[string]any{
				"id": node.ID, "function": node.Name, "qualifiedName": node.QualifiedName,
				"kind": node.Kind, "file": node.FilePath, "line": node.StartLine,
				"service": node.ServiceName, "depth": hop.Depth, "source": source,
				"callSite":   map[string]any{"line": hop.Edge.Line, "col": hop.Edge.Col},
				"confidence": hop.Edge.Confidence, "provenance": hop.Edge.Provenance, "bridge": hop.Bridge,
			})
		}
		return map[string]any{
			"nodes": nodes, "hops": direction.Hops, "truncated": direction.Truncated,
			"nextFrontier": direction.NextFrontier, "unresolved": direction.Unresolved,
		}
	}
	response := map[string]any{
		"direction": result.Direction, "maxDepth": result.MaxDepth,
		"maxNodes": result.MaxNodes, "maxFanout": result.MaxFanout,
		"callers": decorate(result.Callers, true), "callees": decorate(result.Callees, false),
	}
	if result.Target != nil {
		response["target"] = result.Target
	}
	if len(result.Candidates) > 0 {
		response["candidates"] = result.Candidates
		response["error"] = "ambiguous symbol; provide file or qualified_name"
	}
	return response
}

func symbolQueryTokens(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '.', '#', '/', '\\', ':':
			return true
		default:
			return false
		}
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if len([]rune(f)) >= 3 {
			out = append(out, f)
		}
	}
	return out
}

// readNodeSource reads a node's source from its file using start/end line ranges.
func readNodeSource(root string, n codegraph.Node) string {
	if n.FilePath == "" || n.StartLine <= 0 {
		return ""
	}
	absPath := filepath.Join(root, n.FilePath)
	if !strings.HasPrefix(n.FilePath, "repos/") {
		absPath = filepath.Join(root, "repos", n.FilePath)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if n.EndLine > len(lines) {
		n.EndLine = len(lines)
	}
	if n.EndLine <= n.StartLine {
		n.EndLine = n.StartLine + 40 // max 40 lines if range missing
	}
	// Add a few lines of context before the start.
	start := n.StartLine - 3
	if start < 0 {
		start = 0
	}
	end := n.EndLine
	if end > len(lines) {
		end = len(lines)
	}
	body := strings.Join(lines[start:end], "\n")
	if len(body) > 4000 {
		body = body[:4000] + "\n...(truncated)"
	}
	return body
}
