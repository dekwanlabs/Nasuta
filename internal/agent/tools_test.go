package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	types "github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/internal/platform/embed"
	"github.com/dekwanlabs/astris/internal/platform/store"
	"github.com/dekwanlabs/astris/internal/platform/store/codegraph"
	"github.com/dekwanlabs/astris/internal/retrieval"
	"github.com/dekwanlabs/astris/llm"
	toolruntime "github.com/dekwanlabs/astris/tool"
)

type testEmbedder struct{}

type toolTraceRecorder struct{ events []types.EvaluationTrace }

func (recorder *toolTraceRecorder) RecordTrace(event types.EvaluationTrace) {
	recorder.events = append(recorder.events, event)
}

func (testEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1, 0}}, nil
}
func (testEmbedder) Dim() int      { return 2 }
func (testEmbedder) Enabled() bool { return true }

type searchFallbackSemantic struct {
	store.NoopSemantic
	searchCalls      int
	hybridCalls      int
	hybridShouldFail bool
}

func (s *searchFallbackSemantic) Enabled() bool { return true }
func (s *searchFallbackSemantic) Search(context.Context, []float32, map[string]string, int, string) ([]store.SemanticHit, error) {
	s.searchCalls++
	return []store.SemanticHit{{Score: 0.9, Payload: map[string]any{
		"path": "repos/team/orders/main.go", "lang": "go", "repo": "team/orders",
		"start_line": 1, "end_line": 2, "text": "func order() {}", "preview": "func order() {}",
	}}}, nil
}
func (s *searchFallbackSemantic) SearchFiltered(context.Context, []float32, store.SemanticFilter, int, string) ([]store.SemanticHit, error) {
	return nil, nil
}
func (s *searchFallbackSemantic) SearchHybrid(context.Context, []float32, []uint32, []float32, map[string]string, int, string) ([]store.SemanticHit, error) {
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
	ctx := types.WithTraceRecorder(context.Background(), recorder)
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
	if match["scoreKind"] != string(store.SemanticScoreFusion) || match["fusionScore"] != float64(semanticHybrid) {
		t.Fatalf("hybrid score metadata = %#v", match)
	}
	if _, leaked := match["semanticScore"]; leaked {
		t.Fatalf("RRF score must not be exposed as cosine: %#v", match)
	}
}

type fusionSemantic struct {
	store.NoopSemantic
	score float32
}

func (s *fusionSemantic) Enabled() bool { return true }
func (s *fusionSemantic) SearchHybrid(context.Context, []float32, []uint32, []float32, map[string]string, int, string) ([]store.SemanticHit, error) {
	return []store.SemanticHit{{Score: s.score, ScoreKind: store.SemanticScoreFusion, Payload: map[string]any{
		"path": "repos/team/orders/main.go", "lang": "go", "repo": "team/orders", "text": "checkout timeout",
	}}}, nil
}

func TestToolExecutorAllowsRetryAfterFailure(t *testing.T) {
	tries := 0
	registry := testRegistry(t, testAgentTool("unstable", ToolKindRead, func(context.Context, toolruntime.Arguments) (string, error) {
		tries++
		if tries == 1 {
			return "", errors.New("temporary failure")
		}
		return "ok", nil
	}))
	executor := NewToolExecutor(registry)
	seen := map[string]bool{}
	call := llm.ToolCall{ID: "1", Function: llm.ToolFunction{Name: "unstable", Arguments: `{}`}}
	policy := ToolPolicyForPlan(types.EvidencePlan{Sources: types.AllEvidence}, true)
	first, _ := executor.ExecuteWithPolicy(context.Background(), policy, call, seen, nil)
	second, _ := executor.ExecuteWithPolicy(context.Background(), policy, call, seen, nil)
	if first != "error: temporary failure" || second != "ok" || tries != 2 {
		t.Fatalf("retry behavior = first:%q second:%q tries:%d", first, second, tries)
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
