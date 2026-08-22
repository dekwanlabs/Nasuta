package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

type queryEmbeddingOutput struct {
	Vectors  [][]float32
	EmbedErr error
}

var queryEmbeddingSpec = runtrace.Spec[string, queryEmbeddingOutput]{
	Operation: "knowledge.query_embedding",
	Node:      "query_embedding",
	Input: func(query string) map[string]any {
		return map[string]any{"query": query}
	},
	Output: func(_ string, output queryEmbeddingOutput, err error) map[string]any {
		if err != nil {
			return map[string]any{"error": errString(output.EmbedErr), "vectors": len(output.Vectors)}
		}
		return map[string]any{"vectors": len(output.Vectors), "dimensions": len(output.Vectors[0])}
	},
}

type sparseQueryOutput struct {
	Indices []uint32
	Values  []float32
}

var sparseQuerySpec = runtrace.Spec[string, sparseQueryOutput]{
	Operation: "knowledge.sparse_query",
	Node:      "sparse_query",
	Input: func(query string) map[string]any {
		return map[string]any{"query": query}
	},
	Output: func(_ string, output sparseQueryOutput, _ error) map[string]any {
		return map[string]any{"terms": len(output.Indices), "known_terms": len(output.Indices) > 0}
	},
}

type vectorSearchInput struct {
	Query       semantic.Query
	Mode        string
	FetchLimit  int
	SparseTerms int
	Filters     map[string]string
}

var vectorSearchSpec = runtrace.Spec[vectorSearchInput, []semantic.Hit]{
	Operation: "knowledge.vector_search",
	Node:      "vector_search",
	Input: func(input vectorSearchInput) map[string]any {
		return map[string]any{
			"mode": input.Mode, "fetch_limit": input.FetchLimit,
			"sparse_terms": input.SparseTerms, "filters": input.Filters,
		}
	},
	Output: func(_ vectorSearchInput, hits []semantic.Hit, err error) map[string]any {
		output := map[string]any{"hits": len(hits), "top": traceSemanticHits(hits)}
		if err != nil {
			output["error"] = err.Error()
		}
		return output
	},
}

type fileDedupInput struct {
	Hits       []semantic.Hit
	FetchLimit int
}

var fileDedupSpec = runtrace.Spec[fileDedupInput, []semantic.Hit]{
	Operation: "knowledge.file_dedup",
	Node:      "file_dedup",
	Input: func(input fileDedupInput) map[string]any {
		return map[string]any{"hits": len(input.Hits), "fetch_limit": input.FetchLimit}
	},
	Output: func(_ fileDedupInput, candidates []semantic.Hit, _ error) map[string]any {
		return map[string]any{"files": len(candidates), "top": traceSemanticHits(candidates)}
	},
}

type codeRankInput struct {
	Query      string
	Candidates []semantic.Hit
	Limit      int
}

type codeRankOutput struct {
	Ranked  []rankedCodeHit
	Matches []domain.CodeSearchHit
}

var codeRankSpec = runtrace.Spec[codeRankInput, codeRankOutput]{
	Operation: "knowledge.code_rank",
	Node:      "code_rank",
	Input: func(input codeRankInput) map[string]any {
		return map[string]any{"candidates": len(input.Candidates), "limit": input.Limit}
	},
	Output: func(_ codeRankInput, output codeRankOutput, _ error) map[string]any {
		return map[string]any{"matches": len(output.Matches), "top": traceRankedCodeHits(output.Ranked)}
	},
}

var errEmptyQueryEmbedding = fmt.Errorf("empty query embedding")

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
			"startLine": hit.StartLine, "endLine": hit.EndLine, "text": hit.Text,
			"score": hit.Score, "scoreKind": hit.ScoreKind,
			"evidenceClass": hit.EvidenceClass, "trustTier": hit.TrustTier,
		}
		if hit.ScoreKind == string(semantic.ScoreFusion) {
			match["fusionScore"] = hit.FusionScore
		}
		if hit.HasDenseScore {
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

func toCodeSearchResult(found domain.SearchResult[domain.CodeSearchHit]) knowledge.CodeSearchResult {
	matches := make([]knowledge.CodeSearchHit, 0, len(found.Matches))
	for _, hit := range found.Matches {
		match := knowledge.CodeSearchHit{
			Path:          hit.Path,
			Lang:          hit.Lang,
			Repo:          hit.Repo,
			Layer:         hit.Layer,
			StartLine:     hit.StartLine,
			EndLine:       hit.EndLine,
			Text:          hit.Text,
			Score:         hit.Score,
			ScoreKind:     hit.ScoreKind,
			EvidenceClass: hit.EvidenceClass,
			TrustTier:     hit.TrustTier,
		}
		if hit.ScoreKind == string(semantic.ScoreFusion) {
			match.FusionScore = &hit.FusionScore
		}
		if hit.HasDenseScore {
			match.SemanticScore = &hit.SemanticScore
		}
		matches = append(matches, match)
	}
	return knowledge.CodeSearchResult{Matches: matches, Semantic: found.Semantic}
}

// FindCode searches indexed code and returns typed hits for internal consumers.
func (srv *Service) FindCode(ctx context.Context, query, lang string, limit int) (domain.SearchResult[domain.CodeSearchHit], error) {
	if limit <= 0 {
		limit = 10
	}
	if !srv.semanticEnabled() {
		return domain.SearchResult[domain.CodeSearchHit]{}, fmt.Errorf("search_code requires semantic search and embedding configuration")
	}
	vector, err := srv.EmbedQuery(ctx, query)
	if err != nil {
		return domain.SearchResult[domain.CodeSearchHit]{}, err
	}
	return srv.FindCodeByVector(ctx, query, lang, limit, vector)
}

func (srv *Service) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	if !srv.semanticEnabled() {
		return nil, fmt.Errorf("query embedding requires semantic search and embedding configuration")
	}
	embedding, err := runtrace.Invoke(ctx, queryEmbeddingSpec, query, func(ctx context.Context, query string) (queryEmbeddingOutput, error) {
		vectors, embedErr := srv.embedder.Embed(ctx, []string{query})
		output := queryEmbeddingOutput{Vectors: vectors, EmbedErr: embedErr}
		if embedErr != nil {
			return output, embedErr
		}
		if len(vectors) == 0 || len(vectors[0]) == 0 {
			return output, errEmptyQueryEmbedding
		}
		return output, nil
	})
	if err != nil {
		log.ErrorfCtx(ctx, "[query_embedding] embed failed: err=%v vecs=%d", err, len(embedding.Vectors))
		return nil, fmt.Errorf("embed query: %w", err)
	}
	return embedding.Vectors[0], nil
}

func (srv *Service) FindCodeByVector(ctx context.Context, query, lang string, limit int, vector []float32) (domain.SearchResult[domain.CodeSearchHit], error) {
	if limit <= 0 {
		limit = 10
	}
	if len(vector) == 0 {
		return domain.SearchResult[domain.CodeSearchHit]{}, errEmptyQueryEmbedding
	}
	filters := map[string]string{"kind": "code_chunk"}
	if lang != "" {
		filters["lang"] = lang
	}
	fetchLimit := min(max(limit*4, 40), 64)
	mode := "dense"
	sparseTerms := 0
	searchQuery := semantic.Query{
		DenseVector: vector, Filter: semantic.Filter{Keywords: filters},
		Limit: fetchLimit, GroupBy: "path",
	}
	if bm := srv.BM25View(); bm != nil {
		sparse, _ := runtrace.Invoke(ctx, sparseQuerySpec, query, func(_ context.Context, query string) (sparseQueryOutput, error) {
			vector := bm.QuerySparse(query)
			indices, values := retrieval.SparseToSorted(vector)
			return sparseQueryOutput{Indices: indices, Values: values}, nil
		})
		sparseTerms = len(sparse.Indices)
		if sparseTerms == 0 {
			log.InfofCtx(ctx, "[search_code] dense fallback: query has no known BM25 terms, dim=%d", len(vector))
		} else {
			mode = "hybrid"
			log.InfofCtx(ctx, "[search_code] hybrid: dim=%d sparseTerms=%d", len(vector), sparseTerms)
			searchQuery.SparseVector = &semantic.SparseVector{Indices: sparse.Indices, Values: sparse.Values}
		}
	} else {
		srv.denseWarnOnce.Do(func() {
			log.WarnfCtx(ctx, "[search_code] hybrid search disabled (BM25 nil) — running dense-only; run the full code embedding operation to enable it")
		})
		log.InfofCtx(ctx, "[search_code] dense-only: dim=%d (BM25 nil)", len(vector))
	}
	hits, err := runtrace.Invoke(ctx, vectorSearchSpec, vectorSearchInput{
		Query: searchQuery, Mode: mode, FetchLimit: fetchLimit, SparseTerms: sparseTerms, Filters: filters,
	}, func(ctx context.Context, input vectorSearchInput) ([]semantic.Hit, error) {
		return srv.semantic.Search(ctx, input.Query)
	})
	if err != nil {
		log.ErrorfCtx(ctx, "[search_code] semantic search failed: %v", err)
		return domain.SearchResult[domain.CodeSearchHit]{}, err
	}
	log.InfofCtx(ctx, "[search_code] semantic backend returned %d hits before filtering", len(hits))
	candidates, _ := runtrace.Invoke(ctx, fileDedupSpec, fileDedupInput{
		Hits: hits, FetchLimit: fetchLimit,
	}, func(_ context.Context, input fileDedupInput) ([]semantic.Hit, error) {
		sort.SliceStable(input.Hits, func(i, j int) bool {
			return input.Hits[i].Score > input.Hits[j].Score
		})
		candidates := make([]semantic.Hit, 0, len(input.Hits))
		seenFile := make(map[string]struct{}, len(input.Hits))
		for _, hit := range input.Hits {
			path, _ := hit.Metadata["path"].(string)
			if strings.Contains(path, "/"+platform.WorkspaceMetadataDir) || strings.HasPrefix(path, platform.WorkspaceMetadataDir) {
				continue
			}
			if _, duplicate := seenFile[path]; duplicate {
				continue
			}
			seenFile[path] = struct{}{}
			candidates = append(candidates, hit)
		}
		return candidates, nil
	})
	log.InfofCtx(ctx, "[search_code] dedup by file: %d hits -> %d files", len(hits), len(candidates))

	ranked, _ := runtrace.Invoke(ctx, codeRankSpec, codeRankInput{
		Query: query, Candidates: candidates, Limit: limit,
	}, func(_ context.Context, input codeRankInput) (codeRankOutput, error) {
		ranked := rankCodeHits(input.Query, input.Candidates, input.Limit)
		matches := make([]domain.CodeSearchHit, 0, len(ranked))
		for _, candidate := range ranked {
			hit := candidate.hit
			evidenceClass, trustTier := evidenceFromCodeHit(hit)
			adjusted := domain.TrustAdjustedScore(float64(hit.Score), trustTier)
			match := domain.CodeSearchHit{
				Path: payloadString(hit.Metadata, "path"), Lang: payloadString(hit.Metadata, "lang"),
				Repo: payloadString(hit.Metadata, "repo"), Layer: payloadString(hit.Metadata, "layer"),
				StartLine: payloadInt(hit.Metadata["start_line"]), EndLine: payloadInt(hit.Metadata["end_line"]),
				Text:  payloadString(hit.Metadata, "text"),
				Score: adjusted, ScoreKind: string(hit.ScoreKind), HasDenseScore: hit.ScoreKind != semantic.ScoreFusion || hit.DenseRank > 0,
				EvidenceClass: evidenceClass, TrustTier: trustTier,
			}
			if hit.ScoreKind == semantic.ScoreFusion {
				match.FusionScore = float64(hit.FusionScore)
			}
			if match.HasDenseScore {
				match.SemanticScore = float64(hit.DenseScore)
			}
			matches = append(matches, match)
		}
		return codeRankOutput{Ranked: ranked, Matches: matches}, nil
	})
	return domain.SearchResult[domain.CodeSearchHit]{Matches: ranked.Matches, Semantic: true}, nil
}

func traceSemanticHits(hits []semantic.Hit) []map[string]any {
	limit := min(len(hits), 10)
	items := make([]map[string]any, 0, limit)
	for _, hit := range hits[:limit] {
		items = append(items, map[string]any{
			"path": payloadString(hit.Metadata, "path"), "score": hit.Score,
			"dense_score": hit.DenseScore, "sparse_score": hit.SparseScore, "fusion_score": hit.FusionScore,
			"dense_rank": hit.DenseRank, "sparse_rank": hit.SparseRank, "score_kind": hit.ScoreKind,
		})
	}
	return items
}

func traceRankedCodeHits(hits []rankedCodeHit) []map[string]any {
	limit := min(len(hits), 10)
	items := make([]map[string]any, 0, limit)
	for _, hit := range hits[:limit] {
		items = append(items, map[string]any{
			"path": payloadString(hit.hit.Metadata, "path"), "base_score": hit.hit.Score,
			"rank_score": hit.rankScore, "lexical_coverage": hit.lexicalCoverage,
			"identity_coverage": hit.identityCoverage, "covered_terms": len(hit.coveredQueryUnit),
		})
	}
	return items
}

func evidenceFromCodeHit(hit semantic.Hit) (string, int) {
	class, _ := hit.Metadata["evidence_class"].(string)
	trust := trustTierFromPayload(hit.Metadata["trust_tier"])
	if class != "" && trust > 0 {
		return class, trust
	}
	lang, _ := hit.Metadata["lang"].(string)
	repo, _ := hit.Metadata["repo"].(string)
	return domain.EvidenceForCodeChunk(lang, repo)
}
