package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

// docStore is the runbook-facing subset of the document store.
type docStore interface {
	RunbookMetas() ([]domain.RunbookRecord, error)
	RunbookMetaByID(id string) (domain.RunbookRecord, error)
	SearchRunbooksKeyword(query string, limit int) ([]domain.RunbookRecord, error)
	RunbookByID(id string) (domain.RunbookRecord, error)
	CountRunbooks() (int, error)
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
		"matches":   result.Matches,
		"semantic":  result.Semantic,
		"docScoped": result.DocScoped,
		"truncated": result.Truncated,
	}, nil
}

// FindRunbooks returns typed runbook matches for internal consumers.
func (srv *Service) FindRunbooks(ctx context.Context, query knowledge.RunbookQuery) (domain.RunbookSearchResult, error) {
	return executiontrace.Invoke(ctx, runbookSearchSpec, query, func(ctx context.Context, query knowledge.RunbookQuery) (domain.RunbookSearchResult, error) {
		return srv.findRunbooks(ctx, query)
	})
}

var runbookSearchSpec = executiontrace.Spec[knowledge.RunbookQuery, domain.RunbookSearchResult]{
	Operation: "knowledge.runbook_search",
	Node:      "runbook_search",
	Input: func(query knowledge.RunbookQuery) map[string]any {
		return map[string]any{"query": query.Query, "limit": query.Limit, "doc_id": query.DocID}
	},
	Output: func(_ knowledge.RunbookQuery, result domain.RunbookSearchResult, err error) map[string]any {
		output := map[string]any{
			"matches":  len(result.Matches),
			"semantic": result.Semantic,
			"runbooks": traceRunbookNames(result.Matches),
		}
		if err != nil {
			output["error"] = err.Error()
		}
		return output
	},
}

func (srv *Service) findRunbooks(ctx context.Context, query knowledge.RunbookQuery) (domain.RunbookSearchResult, error) {
	if srv.docStore == nil {
		return emptyRunbookResult(query), nil
	}
	if srv.semanticEnabled() {
		return srv.findRunbooksSemantically(ctx, query)
	}
	log.InfofCtx(
		ctx,
		"[runbook_search] semantic disabled (Semantic=%v Embedder=%v) → keyword fallback",
		srv.semantic != nil,
		srv.embedder != nil,
	)
	return srv.findRunbooksByKeyword(query)
}

func (srv *Service) findRunbooksSemantically(
	ctx context.Context,
	query knowledge.RunbookQuery,
) (domain.RunbookSearchResult, error) {
	var meta domain.RunbookRecord
	if query.DocID != "" {
		var err error
		meta, err = srv.docStore.RunbookMetaByID(query.DocID)
		if err != nil {
			return domain.RunbookSearchResult{}, fmt.Errorf("runbook_not_found: doc_id %q: %w", query.DocID, err)
		}
	}
	vectors, err := srv.embedder.Embed(ctx, []string{query.Query})
	if err != nil {
		return domain.RunbookSearchResult{}, fmt.Errorf("embed runbook query: %w", err)
	}
	if len(vectors) == 0 {
		return domain.RunbookSearchResult{}, fmt.Errorf("embed runbook query: empty vector")
	}
	keywords := map[string]string{"kind": "runbook"}
	fetchLimit := max(query.Limit*4, 12)
	if query.DocID != "" {
		keywords["doc_id"] = query.DocID
		fetchLimit = query.Limit + 1
	}
	hits, err := srv.semantic.Search(ctx, semantic.Query{
		DenseVector: vectors[0],
		Filter:      semantic.Filter{Keywords: keywords},
		Limit:       fetchLimit,
	})
	if err != nil {
		return domain.RunbookSearchResult{}, fmt.Errorf("search runbooks: %w", err)
	}
	return runbookResultFromHits(hits, meta, query), nil
}

func (srv *Service) findRunbooksByKeyword(query knowledge.RunbookQuery) (domain.RunbookSearchResult, error) {
	var records []domain.RunbookRecord
	if query.DocID != "" {
		meta, err := srv.docStore.RunbookMetaByID(query.DocID)
		if err != nil {
			return domain.RunbookSearchResult{}, fmt.Errorf("runbook_not_found: doc_id %q: %w", query.DocID, err)
		}
		full, err := srv.docStore.RunbookByID(meta.ID)
		if err != nil {
			return domain.RunbookSearchResult{}, fmt.Errorf("load runbook %q: %w", meta.ID, err)
		}
		records = []domain.RunbookRecord{full}
	} else {
		var err error
		records, err = srv.docStore.SearchRunbooksKeyword(query.Query, query.Limit*2)
		if err != nil {
			return domain.RunbookSearchResult{}, fmt.Errorf("keyword search runbooks: %w", err)
		}
	}
	scored := scoreRunbooks(records, query.Query, query.Limit)
	if len(scored) == 0 {
		return emptyRunbookResult(query), nil
	}
	count := min(len(scored), query.Limit)
	matches := make([]domain.RunbookSearchHit, count)
	for index := range count {
		record := scored[index]
		evidenceClass, trustTier := domain.EvidenceForRunbookScope(record.Scope)
		text := record.Text
		if runes := []rune(text); len(runes) > 4000 {
			text = string(runes[:4000])
		}
		matches[index] = domain.RunbookSearchHit{
			DocID: record.ID, Title: record.Title, Path: record.Path, DocKind: record.Scope,
			EvidenceClass: evidenceClass, TrustTier: trustTier,
			Chunks: []domain.RunbookChunk{{ChunkText: text}},
		}
	}
	return domain.RunbookSearchResult{Matches: matches, DocScoped: query.DocID != ""}, nil
}

func emptyRunbookResult(query knowledge.RunbookQuery) domain.RunbookSearchResult {
	return domain.RunbookSearchResult{
		Matches:   []domain.RunbookSearchHit{},
		DocScoped: query.DocID != "",
	}
}

func traceRunbookNames(matches []domain.RunbookSearchHit) []string {
	limit := min(len(matches), 10)
	names := make([]string, 0, limit)
	for _, match := range matches[:limit] {
		names = append(names, match.Title)
	}
	return names
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

func (srv *Service) runbookCount() (int, error) {
	if srv.docStore == nil {
		return 0, nil
	}
	count, err := srv.docStore.CountRunbooks()
	if err != nil {
		return 0, err
	}
	return count, nil
}

type scoredRunbook struct {
	record domain.RunbookRecord
	score  int
}

func scoreRunbooks(all []domain.RunbookRecord, query string, limit int) []domain.RunbookRecord {
	normalizedQuery := platform.Normalize(query)
	scored := make([]scoredRunbook, 0, len(all))
	for _, record := range all {
		if score := scoreRunbook(record, normalizedQuery); score > 0 {
			scored = append(scored, scoredRunbook{record: record, score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})
	count := min(len(scored), limit)
	matches := make([]domain.RunbookRecord, count)
	for index := range count {
		matches[index] = scored[index].record
	}
	return matches
}

func scoreRunbook(record domain.RunbookRecord, normalizedQuery string) int {
	fields := []string{record.ID, record.Title, record.Path, record.Scope}
	fields = append(fields, record.Tags...)
	for _, field := range fields {
		if field != "" && strings.Contains(platform.Normalize(field), normalizedQuery) {
			return 80
		}
	}
	body := platform.Normalize(record.Text)
	if strings.Contains(body, normalizedQuery) {
		return 30
	}
	score := 0
	for _, token := range strings.Split(normalizedQuery, "-") {
		if len(token) > 2 && strings.Contains(body, token) {
			score += 10
		}
	}
	return score
}

func runbookResultFromHits(
	hits []semantic.Hit,
	meta domain.RunbookRecord,
	query knowledge.RunbookQuery,
) domain.RunbookSearchResult {
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
		sort.Slice(chunks, func(i, j int) bool {
			return chunks[i].ChunkIndex < chunks[j].ChunkIndex
		})
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
				result.Matches[position].Chunks[0] = runbookChunkFromHit(
					hit,
					intFromPayload(hit.Metadata["chunk_index"]),
				)
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
			Chunks: []domain.RunbookChunk{
				runbookChunkFromHit(hit, intFromPayload(hit.Metadata["chunk_index"])),
			},
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

func runbookEvidence(hit semantic.Hit, record domain.RunbookRecord) (string, int) {
	class, _ := hit.Metadata["evidence_class"].(string)
	trust := trustTierFromPayload(hit.Metadata["trust_tier"])
	if class != "" && trust > 0 {
		return class, trust
	}
	scope := record.Scope
	if scope == "" {
		scope, _ = hit.Metadata["scope"].(string)
	}
	return domain.EvidenceForRunbookScope(scope)
}
