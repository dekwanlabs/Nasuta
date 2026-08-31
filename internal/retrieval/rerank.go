package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/platform/httpclient"
	"github.com/dekwanlabs/nasuta/tool"
)

type codeDoc struct {
	source        string
	service       string
	layer         string
	filePath      string
	methodSig     string
	funcName      string
	docID         string
	kind          string
	sections      []string
	coverage      tool.EvidenceCoverage
	startLine     int
	endLine       int
	text          string
	chars         int
	refs          int
	recallScore   float64
	denseScore    float64
	hasDense      bool
	scoreKind     string
	rerankScore   float64
	evidenceClass string
	trustTier     int
	evidenceUnits []tool.EvidenceUnit
}

func (d codeDoc) candidateScore() float64 {
	if d.recallScore != 0 {
		return d.recallScore
	}
	return d.denseScore
}

func (d codeDoc) supportsDensePreflight() bool { return d.scoreKind != "rrf" }

// W_REFS nudges codegraph-heavy hits upward during dedupe.
const W_REFS = 30

type Reranker interface {
	Score(ctx context.Context, query string, docs []codeDoc) ([]float64, error)
	Enabled() bool
}

func (retrieve *Retriever) postProcessCodePool(ctx context.Context, pool []codeDoc, query string, plan domain.QueryPlan) []codeDoc {
	log.InfofCtx(ctx, "[qa] code pool input: %d docs\n%s", len(pool), poolSummary(pool, "input"))

	rerankLimit := retrieve.platform.RerankPool
	budget := retrievalPolicyFor(plan.Kind).budget.rerank
	if rerankLimit <= 0 || rerankLimit > budget {
		rerankLimit = budget
	}
	pool, _ = runtrace.Invoke(ctx, candidateDedupSpec, candidateDedupInput{Docs: pool}, func(_ context.Context, input candidateDedupInput) ([]codeDoc, error) {
		return dedupBySource(input.Docs), nil
	})
	log.InfofCtx(ctx, "[qa] code pool dedup: → %d\n%s", len(pool), poolSummary(pool, "dedup"))

	pool, _ = runtrace.Invoke(ctx, candidateTruncateSpec, candidateTruncateInput{
		Docs: pool, Limit: rerankLimit, MinimumPerSource: 2,
	}, func(_ context.Context, input candidateTruncateInput) ([]codeDoc, error) {
		return selectRerankCandidates(input.Docs, input.Limit, input.MinimumPerSource), nil
	})
	log.InfofCtx(ctx, "[qa] code pool coarse-truncate: → %d", len(pool))

	poolBeforeRerank := append([]codeDoc(nil), pool...)
	reranked, _ := runtrace.Invoke(ctx, candidateRerankSpec, candidateRerankInput{
		Docs: pool, Query: query, Reranker: retrieve.reranker, MinDensePreflight: retrieve.platform.RerankMinDensePreflight,
	}, func(ctx context.Context, input candidateRerankInput) (rerankResult, error) {
		result := rerankPool(ctx, input.Reranker, input.Query, input.Docs, input.MinDensePreflight)
		return result, nil
	})
	pool = reranked.docs
	{
		top, bot := 0.0, 0.0
		if len(pool) > 0 {
			top = pool[0].rerankScore
			bot = pool[len(pool)-1].rerankScore
		}
		log.InfofCtx(ctx, "[qa] code pool rerank: %d scored (top=%.2f bot=%.2f)", len(pool), top, bot)
		log.InfofCtx(ctx, "[qa] code pool BEFORE rerank (dense order):\n%s", poolSummary(poolBeforeRerank, "before-rerank"))
		log.InfofCtx(ctx, "[qa] code pool AFTER rerank:\n%s", poolSummary(pool, "after-rerank"))
	}

	before := len(pool)
	pool, _ = runtrace.Invoke(ctx, candidateThresholdSpec, candidateThresholdInput{
		Docs: pool, MinScore: retrieve.platform.RerankMinScore,
	}, func(_ context.Context, input candidateThresholdInput) ([]codeDoc, error) {
		return filterByScore(input.Docs, input.MinScore), nil
	})
	log.InfofCtx(ctx, "[qa] code pool threshold(%.2f): %d → %d", retrieve.platform.RerankMinScore, before, len(pool))

	pool, _ = runtrace.Invoke(ctx, candidateDiversitySpec, candidateDiversityInput{
		Docs: pool, TopK: retrieve.platform.RerankTopK, MaxPerService: retrieve.platform.RerankMaxPerService,
		MaxPerServiceLowBand: retrieve.platform.RerankMaxPerServiceLowBand,
	}, func(_ context.Context, input candidateDiversityInput) ([]codeDoc, error) {
		return selectDiverse(input.Docs, input.TopK, input.MaxPerService, input.MaxPerServiceLowBand), nil
	})
	log.InfofCtx(ctx, "[qa] code pool diversity(topK=%d, max/svc=%d, lowband/svc=%d): → %d\n%s", retrieve.platform.RerankTopK, retrieve.platform.RerankMaxPerService, retrieve.platform.RerankMaxPerServiceLowBand, len(pool), poolSummary(pool, "final"))
	return pool
}

type candidateDedupInput struct{ Docs []codeDoc }

var candidateDedupSpec = runtrace.Spec[candidateDedupInput, []codeDoc]{
	Operation: "retrieval.candidate_dedup", Node: "candidate_dedup",
	Input: func(input candidateDedupInput) map[string]any { return map[string]any{"candidates": len(input.Docs)} },
	Output: func(_ candidateDedupInput, result []codeDoc, _ error) map[string]any {
		return map[string]any{"candidates": len(result), "top": tracePool(result)}
	},
}

type candidateTruncateInput struct {
	Docs             []codeDoc
	Limit            int
	MinimumPerSource int
}

var candidateTruncateSpec = runtrace.Spec[candidateTruncateInput, []codeDoc]{
	Operation: "retrieval.candidate_truncate", Node: "candidate_truncate",
	Input: func(input candidateTruncateInput) map[string]any {
		return map[string]any{"candidates": len(input.Docs), "limit": input.Limit, "minimum_per_source": input.MinimumPerSource}
	},
	Output: func(_ candidateTruncateInput, result []codeDoc, _ error) map[string]any {
		return map[string]any{"candidates": len(result), "top": tracePool(result)}
	},
}

type candidateRerankInput struct {
	Docs              []codeDoc
	Query             string
	Reranker          Reranker
	MinDensePreflight float64
}

var candidateRerankSpec = runtrace.Spec[candidateRerankInput, rerankResult]{
	Operation: "retrieval.candidate_rerank", Node: "candidate_rerank",
	Input: func(input candidateRerankInput) map[string]any {
		return map[string]any{"candidates": len(input.Docs), "enabled": input.Reranker.Enabled()}
	},
	Output: func(_ candidateRerankInput, result rerankResult, _ error) map[string]any {
		return map[string]any{
			"candidates": len(result.docs), "mode": result.mode,
			"error": errorString(result.err), "top": tracePool(result.docs),
		}
	},
	Status: func(result rerankResult, _ error) string {
		if result.err != nil {
			return "degraded"
		}
		return ""
	},
}

type candidateThresholdInput struct {
	Docs     []codeDoc
	MinScore float64
}

var candidateThresholdSpec = runtrace.Spec[candidateThresholdInput, []codeDoc]{
	Operation: "retrieval.candidate_threshold", Node: "candidate_threshold",
	Input: func(input candidateThresholdInput) map[string]any {
		return map[string]any{"candidates": len(input.Docs), "min_score": input.MinScore}
	},
	Output: func(input candidateThresholdInput, result []codeDoc, _ error) map[string]any {
		return map[string]any{
			"candidates": len(result), "filtered": len(input.Docs) - len(result),
			"ranked_top": tracePool(input.Docs), "top": tracePool(result),
		}
	},
}

type candidateDiversityInput struct {
	Docs                 []codeDoc
	TopK                 int
	MaxPerService        int
	MaxPerServiceLowBand int
}

var candidateDiversitySpec = runtrace.Spec[candidateDiversityInput, []codeDoc]{
	Operation: "retrieval.candidate_diversity", Node: "candidate_diversity",
	Input: func(input candidateDiversityInput) map[string]any {
		return map[string]any{"candidates": len(input.Docs), "top_k": input.TopK, "max_per_service": input.MaxPerService}
	},
	Output: func(_ candidateDiversityInput, result []codeDoc, _ error) map[string]any {
		return map[string]any{"candidates": len(result), "top": tracePool(result)}
	},
}

func tracePool(pool []codeDoc) []map[string]any {
	limit := min(len(pool), 10)
	items := make([]map[string]any, 0, limit)
	for _, doc := range pool[:limit] {
		items = append(items, map[string]any{
			"source": doc.source, "service": doc.service, "path": doc.filePath,
			"recall_score": doc.candidateScore(), "dense_score": doc.denseScore,
			"rerank_score": doc.rerankScore, "score_kind": doc.scoreKind,
		})
	}
	return items
}

func poolSummary(pool []codeDoc, label string) string {
	if len(pool) == 0 {
		return "  (" + label + ": empty)"
	}
	var sb strings.Builder
	for i, d := range pool {
		loc := d.filePath
		if d.source == "code" && d.startLine > 0 {
			loc = d.filePath
		}
		score := d.rerankScore
		if score == 0 {
			score = d.candidateScore()
		}
		fmt.Fprintf(&sb, "  [%d] %s trust=%d band=%d score=%.3f recall=%.3f dense=%.3f kind=%s\n",
			i, shortLogPath(loc), d.trustTier, domain.TrustBand(d.trustTier), score, d.candidateScore(), d.denseScore, d.scoreKind)
	}
	return sb.String()
}

func topByRecall(docs []codeDoc, n int) []codeDoc {
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].candidateScore() > docs[j].candidateScore() })
	if n > 0 && len(docs) > n {
		return docs[:n]
	}
	return docs
}

func selectRerankCandidates(docs []codeDoc, limit, minimumPerSource int) []codeDoc {
	docs = topByRecall(docs, 0)
	if limit <= 0 || len(docs) <= limit {
		return docs
	}
	selected := make([]bool, len(docs))
	sourceCounts := make(map[string]int)
	out := make([]codeDoc, 0, limit)
	for i, doc := range docs {
		if sourceCounts[doc.source] >= minimumPerSource {
			continue
		}
		selected[i] = true
		sourceCounts[doc.source]++
		out = append(out, doc)
		if len(out) == limit {
			return topByRecall(out, 0)
		}
	}
	for i, doc := range docs {
		if selected[i] {
			continue
		}
		out = append(out, doc)
		if len(out) == limit {
			break
		}
	}
	return topByRecall(out, 0)
}

func dedupBySource(docs []codeDoc) []codeDoc {
	kept := map[string]codeDoc{}
	info := func(d codeDoc) int { return d.chars + W_REFS*d.refs }
	for _, d := range docs {
		keys := [][3]string{
			{"file", d.filePath, ""},
		}
		if d.methodSig != "" {
			keys = append(keys, [3]string{"method", d.service, d.filePath + "|" + d.methodSig})
		}
		for _, k := range keys {
			kk := k[0] + "\x00" + k[1] + "\x00" + k[2]
			prev, ok := kept[kk]
			if !ok || info(d) > info(prev) {
				kept[kk] = d
			}
		}
	}
	seen := map[string]struct{}{}
	// Collapse same-file variants so codegraph fan-out does not flood the pool.
	fileBest := map[string]int{}
	out := make([]codeDoc, 0, len(kept))
	for k := range kept {
		d := kept[k]
		id := d.source + "\x00" + d.filePath + "\x00" + d.methodSig + "\x00" + d.funcName
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		fileKey := d.source + "\x00" + d.filePath
		if prev, ok := fileBest[fileKey]; ok {
			if info(d) > info(out[prev]) {
				out[prev] = d
			}
			continue
		}
		fileBest[fileKey] = len(out)
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].candidateScore() > out[j].candidateScore() })
	return out
}

const rerankMaxWait = 2 * time.Second

// Trust is a tie-breaker here, not a hard veto on relevance.
const bandBonusStep = 0.04

func bandBonus(trustTier int) float64 {
	return bandBonusStep * float64(domain.TrustBand(trustTier))
}

func (d codeDoc) rankScore() float64 { return d.rerankScore + bandBonus(d.trustTier) }

const rerankWeight = 0.7

type rerankResult struct {
	docs []codeDoc
	mode string
	err  error
}

func rerankPool(ctx context.Context, rr Reranker, query string, docs []codeDoc, minDensePreflight float64) rerankResult {
	if len(docs) == 0 {
		return rerankResult{docs: docs, mode: "empty"}
	}
	maxDense := 0.0
	allSupportDensePreflight := true
	for _, d := range docs {
		if !d.supportsDensePreflight() {
			allSupportDensePreflight = false
		} else if d.denseScore > maxDense {
			maxDense = d.denseScore
		}
	}
	maxRecall := 0.0
	for _, d := range docs {
		if score := d.candidateScore(); score > maxRecall {
			maxRecall = score
		}
	}
	blend := func(rerank, recall float64) float64 {
		normalized := 0.0
		if maxRecall > 0 {
			normalized = recall / maxRecall
		}
		return rerankWeight*rerank + (1-rerankWeight)*normalized
	}
	if rr != nil && rr.Enabled() {
		if minDensePreflight > 0 && allSupportDensePreflight && maxDense < minDensePreflight {
			// Skip external rerank when recall itself is too weak to justify the latency.
			log.InfofCtx(ctx, "[qa] rerank preflight skip: best dense=%.2f < %.2f", maxDense, minDensePreflight)
		} else {
			rerankCtx, cancel := context.WithTimeout(ctx, rerankMaxWait)
			scores, err := rr.Score(rerankCtx, query, docs)
			cancel()
			if err == nil && len(scores) == len(docs) {
				for i := range docs {
					docs[i].rerankScore = blend(scores[i], docs[i].candidateScore())
				}
				sortByRankScore(docs)
				return rerankResult{docs: docs, mode: "remote"}
			}
			if err == nil {
				err = fmt.Errorf("reranker returned %d scores for %d candidates", len(scores), len(docs))
			}
			log.WarnfCtx(ctx, "[qa] rerank failed/timed out (%v), preserving recall order", err)
			scoreByRecall(docs)
			return rerankResult{docs: docs, mode: "recall_after_error", err: err}
		}
	}
	// The local scorer is the configured mode when no external reranker is enabled.
	var dr denseReranker
	scores, _ := dr.Score(ctx, query, docs)
	for i := range docs {
		docs[i].rerankScore = scores[i]
	}
	sortByRankScore(docs)
	return rerankResult{docs: docs, mode: "local"}
}

func scoreByRecall(docs []codeDoc) {
	maxRecall := 0.0
	for _, doc := range docs {
		maxRecall = max(maxRecall, doc.candidateScore())
	}
	for i := range docs {
		if maxRecall > 0 {
			docs[i].rerankScore = docs[i].candidateScore() / maxRecall
		}
	}
	sortByRankScore(docs)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func sortByRankScore(docs []codeDoc) {
	sort.SliceStable(docs, func(i, j int) bool {
		return docs[i].rankScore() > docs[j].rankScore()
	})
}

func filterByScore(docs []codeDoc, min float64) []codeDoc {
	out := make([]codeDoc, 0, len(docs))
	for _, d := range docs {
		if d.rerankScore >= min {
			out = append(out, d)
		}
	}
	return out
}

func selectDiverse(docs []codeDoc, topK, maxPerService, maxPerServiceLowBand int) []codeDoc {
	if topK <= 0 || len(docs) == 0 {
		return docs
	}
	if maxPerService <= 0 {
		maxPerService = topK
	}

	result := make([]codeDoc, 0, topK)
	count := map[string]int{}
	lowBandCount := map[string]int{}
	for _, d := range docs {
		key := d.layer + ":" + d.service
		groupCap := maxPerService
		if d.service == "" {
			groupCap = topK
		}
		if count[key] >= groupCap || (maxPerServiceLowBand > 0 && domain.TrustBand(d.trustTier) <= 2 && lowBandCount[key] >= maxPerServiceLowBand) {
			continue
		}
		result = append(result, d)
		count[key]++
		if domain.TrustBand(d.trustTier) <= 2 {
			lowBandCount[key]++
		}
		if len(result) >= topK {
			return result
		}
	}
	return result
}

type denseReranker struct{}

func (denseReranker) Score(_ context.Context, query string, docs []codeDoc) ([]float64, error) {
	qTerms := ExtractTechTerms(query)
	qLower := make([]string, 0, len(qTerms))
	for _, t := range qTerms {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			qLower = append(qLower, t)
		}
	}
	raw := make([]float64, len(docs))
	maxDense, maxRaw := 0.0, 0.0
	for i, d := range docs {
		dense := d.candidateScore()
		if dense > maxDense {
			maxDense = dense
		}
		overlap := 0.0
		if len(qLower) > 0 {
			hay := strings.ToLower(d.text + " " + d.filePath + " " + d.funcName)
			hits := 0
			for _, t := range qLower {
				if strings.Contains(hay, t) {
					hits++
				}
			}
			overlap = 0.25 * float64(hits) / float64(len(qLower))
		}
		refs := 0.0
		if d.refs > 0 {
			refs = 0.1
			if d.refs > 3 {
				refs = 0.1 * 3 / float64(d.refs)
			}
		}
		raw[i] = dense + overlap + refs
		if raw[i] > maxRaw {
			maxRaw = raw[i]
		}
	}
	out := make([]float64, len(docs))
	if maxRaw > 0 {
		for i := range raw {
			out[i] = raw[i] / maxRaw
		}
	}
	return out, nil
}
func (denseReranker) Enabled() bool { return false }

type dashscopeReranker struct {
	apiKey  string
	model   string
	baseURL string
	rc      *resty.Client
}

func newDashScopeReranker(p *config.PlatformSettings) dashscopeReranker {
	return dashscopeReranker{
		apiKey:  p.RerankAPIKey,
		model:   p.RerankModel,
		baseURL: p.RerankBaseURL,
		rc: httpclient.New(120*time.Second, map[string]string{
			"Authorization": "Bearer " + p.RerankAPIKey,
		}),
	}
}

func (dashReranker dashscopeReranker) Enabled() bool { return dashReranker.apiKey != "" }

// Limit per-doc payload before sending it to the reranker endpoint.
const rerankDocChars = 1500

type dashscopeRerankRequest struct {
	Model string `json:"model"`
	Input struct {
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
	} `json:"input"`
	Parameters struct {
		ReturnDocuments bool `json:"return_documents"`
	} `json:"parameters"`
}

type dashscopeRerankResponse struct {
	Output struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	} `json:"output"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (dashReranker dashscopeReranker) Score(ctx context.Context, query string, docs []codeDoc) ([]float64, error) {
	if dashReranker.apiKey == "" || len(docs) == 0 {
		return nil, fmt.Errorf("dashscope reranker not available")
	}
	var req dashscopeRerankRequest
	req.Model = dashReranker.model
	req.Input.Query = query
	req.Input.Documents = make([]string, len(docs))
	for i, d := range docs {
		req.Input.Documents[i] = truncateRerankDocument(d.text, rerankDocChars)
	}

	var out dashscopeRerankResponse
	resp, err := httpclient.Request(ctx, dashReranker.rc).
		SetBody(req).
		SetResult(&out).
		Post(dashReranker.baseURL)
	if err != nil {
		return nil, fmt.Errorf("dashscope rerank: %w", err)
	}
	if resp.IsError() {
		return nil, fmt.Errorf("dashscope rerank API %d: %s", resp.StatusCode(), platform.TruncateForLog(string(resp.Body()), 200))
	}
	if len(out.Output.Results) != len(docs) {
		return nil, fmt.Errorf("dashscope rerank result count mismatch: got %d want %d (code=%q msg=%q)",
			len(out.Output.Results), len(docs), out.Code, platform.TruncateForLog(out.Message, 120))
	}

	// DashScope returns results sorted by relevance with the ORIGINAL index;
	// the Reranker contract requires scores aligned to the input docs order, so
	// scatter each score back to its index rather than filling sequentially.
	scores := make([]float64, len(docs))
	for _, res := range out.Output.Results {
		if res.Index < 0 || res.Index >= len(docs) {
			return nil, fmt.Errorf("dashscope rerank index out of range: %d (n=%d)", res.Index, len(docs))
		}
		s := res.RelevanceScore
		if s < 0 {
			s = 0
		} else if s > 1 {
			s = 1
		}
		scores[res.Index] = s
	}
	// Normalize to the batch max so the top doc is 1.0.
	// This keeps DashScope scores aligned with the dense-reranker scale.
	// Without it, modest raw scores could wipe the pool under RerankMinScore.
	maxScore := 0.0
	for _, s := range scores {
		if s > maxScore {
			maxScore = s
		}
	}
	if maxScore > 0 {
		for i := range scores {
			scores[i] /= maxScore
		}
	}
	return scores, nil
}

func truncateRerankDocument(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes])
}
