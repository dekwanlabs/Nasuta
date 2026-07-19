package retrieval

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/platform/httpclient"
)

func doc(service, file, method, text string, score float64) codeDoc {
	return codeDoc{
		source:     "code",
		service:    service,
		filePath:   file,
		methodSig:  method,
		funcName:   method,
		text:       text,
		chars:      len(text),
		denseScore: score,
	}
}

func TestDedupCrossSource_SameFileKeepsMostInformative(t *testing.T) {
	docs := []codeDoc{
		{source: "code", filePath: "svc/Foo.java", text: "short", chars: 5, denseScore: 0.9},
		{source: "code", filePath: "svc/Foo.java", text: "much longer snippet with more info", chars: 36, denseScore: 0.5},
	}
	out := dedupBySource(docs)
	if len(out) != 1 {
		t.Fatalf("expected 1 after dedup, got %d", len(out))
	}
	if out[0].chars != 36 {
		t.Fatalf("expected the more informative (longer) doc kept, got chars=%d", out[0].chars)
	}
}

func TestDedupCrossSource_DifferentServiceSameMethodNotMerged(t *testing.T) {
	// Same method name in two different services must both survive.
	docs := []codeDoc{
		{source: "codegraph", service: "svc-a", filePath: "a/Foo.java", methodSig: "handle", funcName: "handle", text: "x", chars: 1},
		{source: "codegraph", service: "svc-b", filePath: "b/Foo.java", methodSig: "handle", funcName: "handle", text: "y", chars: 1},
	}
	out := dedupBySource(docs)
	if len(out) != 2 {
		t.Fatalf("same-named methods in different services must NOT merge; got %d", len(out))
	}
}

func TestDiversitySelect_PerServiceCap(t *testing.T) {
	docs := []codeDoc{
		doc("svc-a", "a/1.java", "m", "t1", 0.9),
		doc("svc-a", "a/2.java", "m", "t2", 0.8),
		doc("svc-a", "a/3.java", "m", "t3", 0.7),
		doc("svc-a", "a/4.java", "m", "t4", 0.6),
		doc("svc-b", "b/1.java", "m", "t5", 0.5),
	}
	// Score so ordering is deterministic.
	for i := range docs {
		docs[i].rerankScore = docs[i].denseScore
	}
	out := selectDiverse(docs, 5, 3, 0, true)
	aCount := 0
	for _, d := range out {
		if d.service == "svc-a" {
			aCount++
		}
	}
	if aCount > 3 {
		t.Fatalf("per-service cap violated: svc-a has %d", aCount)
	}
	// Strict mode: only 3 from a + 1 from b = 4, not 5.
	if len(out) != 4 {
		t.Fatalf("strict mode expected 4 (3+1), got %d", len(out))
	}
}

func TestDiversitySelect_BackfillRespectsCap(t *testing.T) {
	docs := []codeDoc{
		doc("svc-a", "a/1.java", "m", "t1", 0.9),
		doc("svc-a", "a/2.java", "m", "t2", 0.8),
		doc("svc-a", "a/3.java", "m", "t3", 0.7),
		doc("svc-a", "a/4.java", "m", "t4", 0.6),
		doc("svc-b", "b/1.java", "m", "t5", 0.5),
	}
	for i := range docs {
		docs[i].rerankScore = docs[i].denseScore
	}
	// In non-strict mode, backfill must not re-feed a service that already hit its cap.
	// svc-a stays capped at 3 even with a 4th high-score doc.
	// svc-b still contributes because it has unused capacity.
	out := selectDiverse(docs, 5, 3, 0, false)
	if len(out) != 4 {
		t.Fatalf("backfill must respect per-service cap: expected 4, got %d", len(out))
	}
	aCount := 0
	for _, d := range out {
		if d.service == "svc-a" {
			aCount++
		}
	}
	if aCount > 3 {
		t.Fatalf("svc-a exceeded cap 3 via backfill: got %d", aCount)
	}
}

// TestDiversitySelect_EmptyServiceBucketUncapped covers full service-resolution failure.
// If every doc lands in the "" bucket, a normal per-service cap would starve recall.
// The unresolved bucket must therefore stay uncapped.
func TestDiversitySelect_EmptyServiceBucketUncapped(t *testing.T) {
	docs := []codeDoc{
		doc("", "a/1.java", "m", "t1", 0.9),
		doc("", "a/2.java", "m", "t2", 0.8),
		doc("", "a/3.java", "m", "t3", 0.7),
		doc("", "a/4.java", "m", "t4", 0.6),
		doc("", "a/5.java", "m", "t5", 0.5),
	}
	for i := range docs {
		docs[i].rerankScore = docs[i].denseScore
	}
	out := selectDiverse(docs, 5, 3, 0, false)
	if len(out) != 5 {
		t.Fatalf("empty-service bucket must be uncapped: expected 5, got %d", len(out))
	}
}

func TestDiversitySelect_PreservesGlobalRankAcrossServices(t *testing.T) {
	docs := []codeDoc{
		doc("svc-a", "a/1.java", "m", "t1", 0.9),
		doc("svc-a", "a/2.java", "m", "t2", 0.8),
		doc("svc-b", "b/1.java", "m", "t3", 0.7),
		doc("svc-b", "b/2.java", "m", "t4", 0.6),
	}
	for i := range docs {
		docs[i].rerankScore = docs[i].denseScore
	}
	out := selectDiverse(docs, 4, 2, 0, true)
	if len(out) != 4 {
		t.Fatalf("expected 4, got %d", len(out))
	}
	if got := filePaths(out); !reflect.DeepEqual(got, []string{"a/1.java", "a/2.java", "b/1.java", "b/2.java"}) {
		t.Fatalf("global rank changed: %v", got)
	}
}

func TestThresholdFilter(t *testing.T) {
	docs := []codeDoc{
		{rerankScore: 0.9},
		{rerankScore: 0.4},
		{rerankScore: 0.35},
		{rerankScore: 0.34},
		{rerankScore: 0.1},
	}
	out := filterByScore(docs, 0.35)
	if len(out) != 3 {
		t.Fatalf("expected 3 above 0.35, got %d", len(out))
	}
}

func TestDiversitySelect_LowBandCapPrunesMirrorDDL(t *testing.T) {
	// Two low-band DDL chunks from the same service must not both survive.
	// maxPerServiceLowBand=1 should prune one while keeping the higher-band code doc.
	// The input order matches rerankPool's production ordering.
	docs := []codeDoc{
		{source: "code", service: "hsds-backstage-cookbook-provider", filePath: "Ctrl.java", text: "code", trustTier: domain.TrustCodeRuntime, rerankScore: 0.40},
		{source: "code", service: "hsds-backstage-cookbook-provider", filePath: "db/pro.sql", text: "ddl1", trustTier: domain.TrustRawDDL, rerankScore: 0.90},
		{source: "code", service: "hsds-backstage-cookbook-provider", filePath: "db/fat.sql", text: "ddl2", trustTier: domain.TrustRawDDL, rerankScore: 0.80},
	}
	out := selectDiverse(docs, 5, 3, 1, false)
	ddlCount := 0
	for _, d := range out {
		if domain.TrustBand(d.trustTier) <= 2 {
			ddlCount++
		}
	}
	if ddlCount != 1 {
		t.Fatalf("low-band cap=1 must keep exactly 1 DDL, got %d (out=%v)", ddlCount, filePaths(out))
	}
	if out[0].filePath != "Ctrl.java" {
		t.Fatalf("band-4 code must lead, got %s", out[0].filePath)
	}
}

func TestDiversitySelect_LowBandCapDisabledWhenZero(t *testing.T) {
	// maxPerServiceLowBand=0 disables the sub-cap: both DDLs survive.
	docs := []codeDoc{
		{source: "code", service: "svc", filePath: "db/pro.sql", text: "ddl1", trustTier: domain.TrustRawDDL, rerankScore: 0.90},
		{source: "code", service: "svc", filePath: "db/fat.sql", text: "ddl2", trustTier: domain.TrustRawDDL, rerankScore: 0.80},
	}
	out := selectDiverse(docs, 5, 3, 0, false)
	ddlCount := 0
	for _, d := range out {
		if domain.TrustBand(d.trustTier) <= 2 {
			ddlCount++
		}
	}
	if ddlCount != 2 {
		t.Fatalf("low-band cap disabled (0) must keep both DDLs, got %d", ddlCount)
	}
}

func filePaths(docs []codeDoc) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.filePath
	}
	return out
}

func TestDenseReranker_NormalizesAndPreservesOrder(t *testing.T) {
	docs := []codeDoc{
		{denseScore: 0.8},
		{denseScore: 0.4},
		{denseScore: 0.0},
	}
	rr := denseReranker{}
	scores, err := rr.Score(context.Background(), "q", docs)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 3 {
		t.Fatalf("expected 3 scores, got %d", len(scores))
	}
	if scores[0] != 1.0 {
		t.Fatalf("max should normalize to 1.0, got %f", scores[0])
	}
	if scores[2] != 0.0 {
		t.Fatalf("zero dense should stay 0, got %f", scores[2])
	}
	if rr.Enabled() {
		t.Fatal("denseReranker must report Enabled()=false (it's the fallback)")
	}
}

func TestRerankPool_FallsBackToDenseWhenRerankerDisabled(t *testing.T) {
	docs := []codeDoc{
		{denseScore: 0.1},
		{denseScore: 0.9},
		{denseScore: 0.5},
	}
	out := rerankPool(context.Background(), denseReranker{}, "q", docs, 0)
	if len(out) != 3 {
		t.Fatalf("expected 3, got %d", len(out))
	}
	// Should be sorted desc by normalized dense score.
	if out[0].denseScore != 0.9 || out[1].denseScore != 0.5 || out[2].denseScore != 0.1 {
		t.Fatalf("not sorted desc by dense: %v %v %v", out[0].denseScore, out[1].denseScore, out[2].denseScore)
	}
	if out[0].rerankScore != 1.0 {
		t.Fatalf("top rerank score should be 1.0, got %f", out[0].rerankScore)
	}
}

func TestRerankPool_FallsBackToDenseOnRerankerError(t *testing.T) {
	docs := []codeDoc{
		{denseScore: 0.1},
		{denseScore: 0.9},
	}
	// errorReranker returns an error → must fall back to dense ordering.
	out := rerankPool(context.Background(), errorReranker{}, "q", docs, 0)
	if out[0].denseScore != 0.9 {
		t.Fatalf("on rerank error should fall back to dense order, got top dense=%f", out[0].denseScore)
	}
}

type errorReranker struct{}

func (errorReranker) Score(context.Context, string, []codeDoc) ([]float64, error) {
	return nil, errReranker
}
func (errorReranker) Enabled() bool { return true }

// errReranker is a sentinel error for TestRerankPool_FallsBackToDenseOnRerankerError.
var errReranker = strErr("reranker unavailable")

type strErr string

func (e strErr) Error() string { return string(e) }

type rerankTraceRecorder struct{ events []domain.EvaluationTrace }

func (recorder *rerankTraceRecorder) RecordTrace(event domain.EvaluationTrace) {
	recorder.events = append(recorder.events, event)
}

// TestPostProcessCodePipeline is an end-to-end check of the 4 steps using the
// dense fallback reranker (no LLM), ensuring dedup+threshold+diversity compose.
func TestPostProcessCodePipeline(t *testing.T) {
	r := &Retriever{
		reranker: denseReranker{},
		platform: rerankTestCfg(),
	}
	docs := []codeDoc{
		doc("svc-a", "a/1.java", "m", "aaaa", 0.95),
		doc("svc-a", "a/1.java", "m", "bbbb", 0.90), // dup file with a/1.java → deduped
		doc("svc-b", "b/1.java", "m", "cccc", 0.80),
		doc("svc-c", "c/1.java", "m", "dddd", 0.30), // dense 0.30/0.95=0.31 < 0.35 → dropped
	}
	recorder := &rerankTraceRecorder{}
	ctx := domain.WithTraceRecorder(context.Background(), recorder)
	out := r.postProcessCodePool(ctx, docs, "q")
	// Expect: 3 after dedup (a/1 once, b/1, c/1), c/1 dropped by threshold → 2.
	if len(out) != 2 {
		t.Fatalf("expected 2 after full pipeline, got %d", len(out))
	}
	for _, d := range out {
		if d.service == "svc-c" {
			t.Fatal("svc-c (below threshold) should have been dropped")
		}
	}
	wantNodes := []string{"candidate_truncate", "candidate_dedup", "candidate_rerank", "candidate_threshold", "candidate_diversity"}
	if len(recorder.events) != len(wantNodes) {
		t.Fatalf("trace events = %#v", recorder.events)
	}
	for i, want := range wantNodes {
		if recorder.events[i].Node != want {
			t.Fatalf("trace[%d] = %q, want %q", i, recorder.events[i].Node, want)
		}
	}
}

// newTestDashScopeReranker points a dashscopeReranker at a fake server.
func newTestDashScopeReranker(url string, srv *httptest.Server) dashscopeReranker {
	return dashscopeReranker{
		apiKey: "k", model: "gte-rerank-v2", baseURL: url,
		rc: httpclient.New(120*time.Second, map[string]string{"Authorization": "Bearer k"}),
	}
}

// TestDashScopeReranker_ScattersScoresByIndex is the critical correctness test:
// DashScope returns results sorted by relevance with the ORIGINAL index, and
// Score must realign them to the input docs order.
func TestDashScopeReranker_ScattersScoresByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 3 docs in; return results sorted desc by relevance with shuffled index:
		// doc[2] most relevant, doc[0] middle, doc[1] least.
		resp := map[string]any{
			"output": map[string]any{
				"results": []map[string]any{
					{"index": 2, "relevance_score": 0.95},
					{"index": 0, "relevance_score": 0.50},
					{"index": 1, "relevance_score": 0.05},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	rr := newTestDashScopeReranker(srv.URL, srv)
	docs := []codeDoc{{text: "a"}, {text: "b"}, {text: "c"}}
	scores, err := rr.Score(context.Background(), "q", docs)
	if err != nil {
		t.Fatalf("Score error: %v", err)
	}
	// Scores realigned to input order (NOT response order) and normalized to the
	// batch max (0.95) so the top doc is 1.0 — matching the dense-reranker scale.
	want := []float64{0.50 / 0.95, 0.05 / 0.95, 1}
	for i := range want {
		if math.Abs(scores[i]-want[i]) > 1e-9 {
			t.Fatalf("score[%d]=%v want %v (index realignment/normalization broken); got %v", i, scores[i], want[i], scores)
		}
	}
}

func TestDashScopeReranker_EnabledRequiresKey(t *testing.T) {
	if (dashscopeReranker{}).Enabled() {
		t.Fatal("no api key must report Enabled()=false")
	}
	if !(dashscopeReranker{apiKey: "k"}).Enabled() {
		t.Fatal("with api key must report Enabled()=true")
	}
}

func TestDashScopeReranker_ErrorsOnHTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	rr := newTestDashScopeReranker(srv.URL, srv)
	if _, err := rr.Score(context.Background(), "q", []codeDoc{{text: "a"}}); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestDashScopeReranker_ErrorsOnCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"output": map[string]any{"results": []map[string]any{
			{"index": 0, "relevance_score": 0.9}, // 1 result for 2 docs
		}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	rr := newTestDashScopeReranker(srv.URL, srv)
	if _, err := rr.Score(context.Background(), "q", []codeDoc{{text: "a"}, {text: "b"}}); err == nil {
		t.Fatal("expected error on result count mismatch")
	}
}

// TestRerankPool_FallsBackOnDashScopeError wires a failing DashScope reranker
// into rerankPool and asserts it degrades to dense order without panicking.
func TestRerankPool_FallsBackOnDashScopeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	rr := newTestDashScopeReranker(srv.URL, srv)
	docs := []codeDoc{{denseScore: 0.1}, {denseScore: 0.9}, {denseScore: 0.5}}
	out := rerankPool(context.Background(), rr, "q", docs, 0)
	if len(out) != 3 {
		t.Fatalf("expected 3, got %d", len(out))
	}
	if out[0].denseScore != 0.9 {
		t.Fatalf("on dashscope error should fall back to dense order, got top dense=%f", out[0].denseScore)
	}
}

func TestRerankPoolDoesNotApplyDensePreflightToRRF(t *testing.T) {
	docs := []codeDoc{
		{filePath: "a.go", recallScore: 0.9, scoreKind: "rrf"},
		{filePath: "b.go", recallScore: 0.1, scoreKind: "rrf"},
	}
	out := rerankPool(context.Background(), fixedReranker{scores: []float64{0, 1}}, "q", docs, 0.5)
	if out[0].filePath != "b.go" {
		t.Fatalf("RRF candidates incorrectly used dense preflight; top=%s", out[0].filePath)
	}
}

func rerankTestCfg() *config.PlatformSettings {
	return &config.PlatformSettings{
		RerankEnabled:         true,
		RerankPool:            50,
		RerankTopK:            10,
		RerankMinScore:        0.35,
		RerankMaxPerService:   3,
		RerankStrictDiversity: false,
	}
}

// fixedReranker returns a preset relevance per doc index, letting tests pin
// relevance so trust-band precedence can be asserted independently.
type fixedReranker struct{ scores []float64 }

func (f fixedReranker) Score(_ context.Context, _ string, docs []codeDoc) ([]float64, error) {
	return f.scores, nil
}
func (f fixedReranker) Enabled() bool { return true }

func TestTrustBand_HigherTierIsHigherBand(t *testing.T) {
	// Ordering property: code > schema/runbook/genDoc > ddl/userDoc/config > serviceMeta > repoDoc.
	if domain.TrustBand(domain.TrustCodeRuntime) <= domain.TrustBand(domain.TrustCuratedSchema) {
		t.Fatal("code must band above curated schema")
	}
	if domain.TrustBand(domain.TrustCuratedSchema) <= domain.TrustBand(domain.TrustRawDDL) {
		t.Fatal("curated schema must band above raw DDL")
	}
	if domain.TrustBand(domain.TrustCuratedRunbook) <= domain.TrustBand(domain.TrustRawDDL) {
		t.Fatal("curated runbook must band above raw DDL")
	}
	if domain.TrustBand(domain.TrustRawDDL) <= domain.TrustBand(domain.TrustRepoDoc) {
		t.Fatal("raw DDL must band above repo doc")
	}
}

// TestRerankPool_RelevanceNotVetoedByBand guards the soft-trust ordering.
// A high-relevance lower-trust doc must beat a low-relevance higher-trust one.
// Trust should only nudge close calls, not override relevance.
func TestRerankPool_RelevanceNotVetoedByBand(t *testing.T) {
	docs := []codeDoc{
		{source: "code", filePath: "db/pro.sql", text: "ddl", trustTier: domain.TrustRawDDL},                             // idx 0
		{source: "code", filePath: "Ctrl.java", text: "code", trustTier: domain.TrustCodeRuntime},                        // idx 1
		{source: "runbook", filePath: "schema-x", funcName: "schema-x", text: "s", trustTier: domain.TrustCuratedSchema}, // idx 2
	}
	rr := fixedReranker{scores: []float64{0.99, 0.40, 0.60}}
	out := rerankPool(context.Background(), rr, "q", docs, 0)

	if out[0].filePath != "db/pro.sql" {
		t.Fatalf("highest-relevance DDL must lead despite lowest band; got %s", out[0].filePath)
	}
	if out[1].filePath != "schema-x" {
		t.Fatalf("schema (rel 0.60) must beat low-relevance code (rel 0.40); got %s", out[1].filePath)
	}
	if out[2].filePath != "Ctrl.java" {
		t.Fatalf("lowest-relevance code must rank last; got %s", out[2].filePath)
	}
}

// TestRerankPool_TrustNudgesCloseCall confirms trust still breaks near-ties:
// the relevance gap (0.05) is smaller than the band nudge (0.16), so the
// higher-band doc wins despite being slightly less relevant.
func TestRerankPool_TrustNudgesCloseCall(t *testing.T) {
	docs := []codeDoc{
		{source: "code", filePath: "repo.md", text: "r", trustTier: domain.TrustRepoDoc},       // band 0, rel 0.55
		{source: "code", filePath: "Ctrl.java", text: "c", trustTier: domain.TrustCodeRuntime}, // band 4, rel 0.50
	}
	rr := fixedReranker{scores: []float64{0.55, 0.50}}
	out := rerankPool(context.Background(), rr, "q", docs, 0)
	if out[0].filePath != "Ctrl.java" {
		t.Fatalf("higher-band doc must win a close call (rel gap 0.05 < band nudge 0.16); got %s", out[0].filePath)
	}
}

func TestRerankPool_WithinBandOrdersByRelevance(t *testing.T) {
	// Two docs in the same band must order by relevance.
	docs := []codeDoc{
		{source: "code", filePath: "a.sql", text: "a", trustTier: domain.TrustRawDDL},
		{source: "code", filePath: "b.sql", text: "b", trustTier: domain.TrustUserDocument}, // same band as raw DDL
	}
	rr := fixedReranker{scores: []float64{0.30, 0.80}}
	out := rerankPool(context.Background(), rr, "q", docs, 0)
	if out[0].filePath != "b.sql" {
		t.Fatalf("within a band, higher relevance must lead; got %s first", out[0].filePath)
	}
}
