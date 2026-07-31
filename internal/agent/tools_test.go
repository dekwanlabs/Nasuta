package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/tool"
)

type testEmbedder struct{}

type semanticTestBase struct{}

func (semanticTestBase) Ensure(context.Context, semantic.Schema) error       { return nil }
func (semanticTestBase) Upsert(context.Context, []semantic.Record) error     { return nil }
func (semanticTestBase) Delete(context.Context, semantic.DeleteQuery) error  { return nil }
func (semanticTestBase) Count(context.Context, semantic.Filter) (int, error) { return 0, nil }
func (semanticTestBase) Close() error                                        { return nil }

type toolTraceRecorder struct{ events []domain.EvaluationTrace }

func (recorder *toolTraceRecorder) RecordTrace(event domain.EvaluationTrace) {
	recorder.events = append(recorder.events, event)
}

func (testEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1, 0}}, nil
}
func (testEmbedder) Dim() int      { return 2 }
func (testEmbedder) Enabled() bool { return true }

type searchFallbackSemantic struct {
	semanticTestBase
	searchCalls      int
	hybridCalls      int
	hybridShouldFail bool
}

func (s *searchFallbackSemantic) Capabilities() semantic.Capabilities {
	return semantic.RequiredCapabilities()
}
func (s *searchFallbackSemantic) Search(ctx context.Context, query semantic.Query) ([]semantic.Hit, error) {
	if query.SparseVector != nil {
		return s.SearchHybrid(ctx, query)
	}
	s.searchCalls++
	return []semantic.Hit{{Score: 0.9, Metadata: map[string]any{
		"path": "repos/team/orders/main.go", "lang": "go", "repo": "team/orders",
		"start_line": 1, "end_line": 2, "text": "func order() {}", "preview": "func order() {}",
	}}}, nil
}
func (s *searchFallbackSemantic) SearchHybrid(_ context.Context, query semantic.Query) ([]semantic.Hit, error) {
	s.hybridCalls++
	if s.hybridShouldFail {
		return nil, errors.New("hybrid should not be called for unknown terms")
	}
	return nil, nil
}

var _ embed.Embedder = testEmbedder{}

func TestCodeSearchUsesDenseFallbackWhenBM25HasNoKnownTerms(t *testing.T) {
	semantic := &searchFallbackSemantic{hybridShouldFail: true}
	svc := NewTools(Deps{Semantic: semantic, Embedder: testEmbedder{}})
	svc.SetBM25(retrieval.NewBM25Builder())

	recorder := &toolTraceRecorder{}
	ctx := domain.WithTraceRecorder(context.Background(), recorder)
	result := svc.CodeSearch(ctx, "token-not-in-vocabulary", "", 5)
	if result["error"] != nil {
		t.Fatalf("CodeSearch returned error: %v", result["error"])
	}
	if semantic.searchCalls != 1 || semantic.hybridCalls != 0 {
		t.Fatalf("search calls = dense:%d hybrid:%d, want dense:1 hybrid:0", semantic.searchCalls, semantic.hybridCalls)
	}
	matches, ok := result["matches"].([]any)
	if !ok || len(matches) != 1 {
		t.Fatalf("matches = %#v, want one dense result", result["matches"])
	}
	wantNodes := []string{"query_embedding", "sparse_query", "vector_search", "file_dedup"}
	if len(recorder.events) != len(wantNodes) {
		t.Fatalf("trace events = %#v", recorder.events)
	}
	for i, want := range wantNodes {
		if recorder.events[i].Node != want {
			t.Fatalf("trace[%d] = %q, want %q", i, recorder.events[i].Node, want)
		}
	}
}

func TestCodeSearchKeepsRRFSeparateFromCosine(t *testing.T) {
	semanticHybrid := float32(0.75)
	svc := NewTools(Deps{Semantic: &fusionSemantic{score: semanticHybrid}, Embedder: testEmbedder{}})
	bm25 := retrieval.NewBM25Builder()
	bm25.AddDoc("checkout timeout")
	svc.SetBM25(bm25)
	result := svc.CodeSearch(context.Background(), "checkout", "", 5)
	matches := result["matches"].([]any)
	match := matches[0].(map[string]any)
	if match["scoreKind"] != string(semantic.ScoreFusion) || match["fusionScore"] != float64(semanticHybrid) {
		t.Fatalf("hybrid score metadata = %#v", match)
	}
	if _, leaked := match["semanticScore"]; leaked {
		t.Fatalf("RRF score must not be exposed as cosine: %#v", match)
	}
}

func TestRunbookResultFromHitsDeduplicatesDocumentsByBestChunk(t *testing.T) {
	hits := []semantic.Hit{
		runbookHit("doc-a", 3, 0.7),
		runbookHit("doc-b", 1, 0.8),
		runbookHit("doc-a", 2, 0.9),
	}
	result := runbookResultFromHits(hits, domain.RunbookRecord{}, knowledge.RunbookQuery{Limit: 2})

	if len(result.Matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(result.Matches))
	}
	if got := result.Matches[0].Chunks[0].ChunkIndex; got != 2 {
		t.Fatalf("doc-a chunk = %d, want best chunk 2", got)
	}
	if got := result.Matches[1].DocID; got != "doc-b" {
		t.Fatalf("second doc = %q, want doc-b", got)
	}
}

func TestRunbookResultFromHitsScopesDeduplicatesTruncatesAndSortsChunks(t *testing.T) {
	hits := []semantic.Hit{
		runbookHit("doc-a", 4, 0.9),
		runbookHit("doc-a", 2, 0.8),
		runbookHit("doc-a", 4, 0.7),
		runbookHit("doc-a", 3, 0.6),
	}
	meta := domain.RunbookRecord{ID: "doc-a", Title: "Architecture", Path: "docs/a.md", Scope: "flow"}
	result := runbookResultFromHits(hits, meta, knowledge.RunbookQuery{DocID: "doc-a", Limit: 2})

	if !result.Semantic || !result.DocScoped || !result.Truncated {
		t.Fatalf("result flags = semantic:%v scoped:%v truncated:%v", result.Semantic, result.DocScoped, result.Truncated)
	}
	if len(result.Matches) != 1 || len(result.Matches[0].Chunks) != 2 {
		t.Fatalf("matches = %#v, want one document with two chunks", result.Matches)
	}
	chunks := result.Matches[0].Chunks
	if chunks[0].ChunkIndex != 2 || chunks[1].ChunkIndex != 4 {
		t.Fatalf("chunk order = [%d %d], want [2 4]", chunks[0].ChunkIndex, chunks[1].ChunkIndex)
	}
}

func runbookHit(docID string, chunkIndex int, score float32) semantic.Hit {
	return semantic.Hit{Score: score, Metadata: map[string]any{
		"doc_id": docID, "title": docID, "path": "docs/" + docID + ".md", "scope": "flow",
		"chunk_index": chunkIndex, "section_header": "Section", "text": docID,
	}}
}

type fusionSemantic struct {
	semanticTestBase
	score float32
}

func (s *fusionSemantic) Capabilities() semantic.Capabilities { return semantic.RequiredCapabilities() }
func (s *fusionSemantic) Search(ctx context.Context, query semantic.Query) ([]semantic.Hit, error) {
	return s.SearchHybrid(ctx, query)
}
func (s *fusionSemantic) SearchHybrid(context.Context, semantic.Query) ([]semantic.Hit, error) {
	return []semantic.Hit{{Score: s.score, ScoreKind: semantic.ScoreFusion, Metadata: map[string]any{
		"path": "repos/team/orders/main.go", "lang": "go", "repo": "team/orders", "text": "checkout timeout",
	}}}, nil
}

func TestToolExecutorAllowsRetryAfterFailure(t *testing.T) {
	tries := 0
	registry := testRegistry(t, testAgentTool("unstable", ToolKindRead, func(context.Context, tool.Arguments) (string, error) {
		tries++
		if tries == 1 {
			return "", errors.New("temporary failure")
		}
		return "ok", nil
	}))
	executor := NewToolExecutor(registry)
	seen := map[string]bool{}
	call := llm.ToolCall{ID: "1", Function: llm.ToolFunction{Name: "unstable", Arguments: `{}`}}
	policy := ToolPolicyForPlan(domain.EvidencePlan{Sources: domain.AllEvidence}, true)
	first := executor.ExecuteWithPolicy(context.Background(), policy, call, seen, nil)
	second := executor.ExecuteWithPolicy(context.Background(), policy, call, seen, nil)
	if first.FullContent != "error: temporary failure" || first.Evidence || second.FullContent != "ok" || !second.Evidence || tries != 2 {
		t.Fatalf("retry behavior = first:%q second:%q tries:%d", first.FullContent, second.FullContent, tries)
	}
}

func TestReadNodeSourceHandlesGitlabPrefixedPath(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("repos", "hsas", "svc", "Foo.java")
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := strings.Join([]string{
		"class Foo {",
		"  void recipeFavorite() {",
		"    service.recipeFavorite();",
		"  }",
		"}",
	}, "\n")
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	got := readNodeSource(root, codegraph.Node{
		FilePath:  "repos/hsas/svc/Foo.java",
		StartLine: 2,
		EndLine:   4,
	})
	if !strings.Contains(got, "service.recipeFavorite();") {
		t.Fatalf("readNodeSource returned %q, want method body", got)
	}
}

func TestSymbolQueryTokens(t *testing.T) {
	got := symbolQueryTokens("H5RecipeFeign recipeFavorite")
	want := []string{"H5RecipeFeign", "recipeFavorite"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("symbolQueryTokens = %#v, want %#v", got, want)
	}
}

func TestGetSymbolResultRequiresLookupKey(t *testing.T) {
	for _, test := range []struct {
		name, query, qualifiedName, wantError string
	}{
		{name: "empty", wantError: "query or qualified_name is required"},
		{name: "qualified name", qualifiedName: "com.example.FirebaseService.tryAcquireFirebaseRequestGuard", wantError: "workspace root"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := (&Service{}).GetSymbolResult(context.Background(), test.query, "", test.qualifiedName, 5)
			if result != nil || err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("symbol lookup = result:%#v err:%v", result, err)
			}
		})
	}
}

type apiTargetRepository struct{}

func (apiTargetRepository) Resolve(context.Context, ontology.ResolveQuery) (ontology.ResolveResult, error) {
	return ontology.ResolveResult{
		Generation: "test",
		Entities: []ontology.EntityRef{{
			ID: "endpoint", Class: ontology.ClassAPIEndpoint, Name: "POST /orders",
		}},
	}, nil
}

func (apiTargetRepository) EntitiesByID(context.Context, ontology.EntityQuery) ([]ontology.EntityRef, error) {
	return []ontology.EntityRef{{ID: "symbol", Class: ontology.ClassCodeSymbol, Name: "OrdersController.CreateOrder"}}, nil
}

func (apiTargetRepository) Neighbors(context.Context, ontology.NeighborQuery) ([]ontology.Fact, bool, error) {
	return []ontology.Fact{{
		ID: "implementation", SubjectID: "endpoint", Predicate: ontology.PredicateImplementedBy, ObjectID: "symbol",
		Evidence: []ontology.Evidence{{
			Path: "repos/team/orders/OrdersController.java", Line: 42, Symbol: "OrdersController.CreateOrder",
			Source: ontology.EvidenceSourceCodeScan,
		}},
	}}, false, nil
}

func (apiTargetRepository) Stats(context.Context) (ontology.Stats, error) {
	return ontology.Stats{}, nil
}

func TestResolveAPICallTargetUsesOntologyImplementationEvidence(t *testing.T) {
	svc := &Service{ontology: ontology.NewService(apiTargetRepository{})}
	request, err := svc.resolveAPICallTarget(context.Background(), callchain.Request{Query: "POST /orders"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Query != "OrdersController.CreateOrder" || request.QualifiedName != request.Query {
		t.Fatalf("resolved query = %+v", request)
	}
	if request.File != "repos/team/orders/OrdersController.java" || request.Line != 42 {
		t.Fatalf("resolved location = %+v", request)
	}
}
