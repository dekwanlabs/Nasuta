package indexing

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/platform/graph"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/semantic"
	"github.com/dekwanlabs/nasuta/semantic/contract"
)

type fakeEmbedder struct{ dim int }

func (f fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, f.dim)
		for j := range v {
			v[j] = 0.01 * float32(i+j)
		}
		out[i] = v
	}
	return out, nil
}
func (fakeEmbedder) Dim() int      { return 8 }
func (fakeEmbedder) Enabled() bool { return true }

type recordingSemantic struct {
	mu     sync.Mutex
	points map[string]semantic.Record
}

func newRecordingSemantic() *recordingSemantic {
	return &recordingSemantic{points: map[string]semantic.Record{}}
}

func (*recordingSemantic) Ensure(context.Context, semantic.Schema) error { return nil }
func (*recordingSemantic) Search(context.Context, semantic.Query) ([]semantic.Hit, error) {
	return nil, nil
}
func (s *recordingSemantic) Upsert(_ context.Context, points []semantic.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, point := range points {
		point.Metadata = clonePayload(point.Metadata)
		point.DenseVector = append([]float32(nil), point.DenseVector...)
		if point.SparseVector != nil {
			point.SparseVector = &semantic.SparseVector{Indices: append([]uint32(nil), point.SparseVector.Indices...), Values: append([]float32(nil), point.SparseVector.Values...)}
		}
		s.points[point.ID] = point
	}
	return nil
}
func (s *recordingSemantic) Delete(_ context.Context, query semantic.DeleteQuery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range query.IDs {
		delete(s.points, id)
	}
	filters := query.Filter.Keywords
	if query.Repository != "" {
		filters = map[string]string{"repo": query.Repository}
	}
	if query.DocumentID != "" {
		filters = map[string]string{"doc_id": query.DocumentID}
	}
	for id, point := range s.points {
		if payloadMatches(point.Metadata, filters) && !payloadMatches(point.Metadata, query.Except.Keywords) {
			delete(s.points, id)
		}
	}
	return nil
}
func (s *recordingSemantic) Count(_ context.Context, filter semantic.Filter) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, point := range s.points {
		if payloadMatches(point.Metadata, filter.Keywords) {
			n++
		}
	}
	return n, nil
}
func (*recordingSemantic) Capabilities() semantic.Capabilities {
	return semantic.RequiredCapabilities()
}
func (*recordingSemantic) Close() error { return nil }

func payloadMatches(payload map[string]any, filters map[string]string) bool {
	if len(filters) == 0 {
		return false
	}
	for key, value := range filters {
		if payload[key] != value {
			return false
		}
	}
	return true
}

func clonePayload(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (s *recordingSemantic) repoPoints(repo string) map[string]semantic.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]semantic.Record{}
	for id, point := range s.points {
		if point.Metadata["repo"] == repo {
			out[id] = point
		}
	}
	return out
}

func newBM25TestService(t *testing.T) (*Service, *agent.Service, string) {
	t.Helper()
	root := t.TempDir()
	svcDir := filepath.Join(root, "demo-svc")
	if err := os.MkdirAll(svcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svcDir, "main.go"),
		[]byte("package main\n\nfunc orderService() {}\nfunc paymentService() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	g := graph.New()
	emb := fakeEmbedder{dim: 8}
	cfg := config.Config{
		WorkspaceRoot:        root,
		SQLitePath:           filepath.Join(root, "index.db"),
		EmbeddingAPIKey:      "test",
		EmbeddingBatch:       4,
		EmbeddingConcurrency: 1,
		IndexCode:            true,
	}
	svc := &Service{
		Cfg:      cfg,
		DB:       db,
		Semantic: contract.NewMemory(),
		Embedder: emb,
		Graph:    g,
		ScanDirs: []string{"demo-svc"},
	}
	tools := agent.NewTools(agent.Deps{DB: db, Graph: g, Semantic: contract.NewMemory(), Embedder: emb})
	svc.SetTools(tools)
	return svc, tools, root
}

func TestService_BM25HandoffNoRace(t *testing.T) {
	t.Parallel()
	svc, tools, _ := newBM25TestService(t)
	defer os.RemoveAll(filepath.Dir(svc.bm25VocabPath()))
	defer svc.DB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if bm := tools.BM25View(); bm != nil {
				_ = bm.VocabularySize()
				_ = bm.QuerySparse("order service")
			}
		}
	}()

	if err := svc.EmbedCodeChunks(ctx, svc.ScanDirs); err != nil {
		t.Fatalf("EmbedCodeChunks: %v", err)
	}

	close(stop)
	wg.Wait()
	bm := tools.BM25View()
	if bm == nil || bm.VocabularySize() == 0 {
		t.Fatalf("BM25 not built after EmbedCodeChunks: %+v", bm)
	}
}

func TestEmbedRepoCodeKeepsOtherRepoSparseCoordinates(t *testing.T) {
	root := t.TempDir()
	repoA := filepath.Join(root, "repos", "team", "orders")
	repoB := filepath.Join(root, "repos", "team", "payments")
	for _, dir := range []string{repoA, repoB} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoA, "main.go"), []byte("package orders\nfunc orderWorkflow() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoB, "main.go"), []byte("package payments\nfunc paymentWorkflow() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	semantic := newRecordingSemantic()
	svc := &Service{
		Cfg: config.Config{
			WorkspaceRoot: root, SQLitePath: filepath.Join(root, "index.db"),
			EmbeddingBatch: 4, EmbeddingConcurrency: 1, IndexCode: true,
		},
		DB: db, Semantic: semantic, Embedder: fakeEmbedder{dim: 8}, Graph: graph.New(),
	}
	dirs := []string{"repos/team/orders", "repos/team/payments"}
	if err := svc.EmbedCodeChunks(context.Background(), dirs); err != nil {
		t.Fatal(err)
	}
	orderBefore := svc.bm25.Load().QuerySparse("order")
	pointsBefore := semantic.repoPoints("team/orders")
	if len(orderBefore) != 1 || len(pointsBefore) == 0 {
		t.Fatalf("full embed missing order sparse state: query=%v points=%d", orderBefore, len(pointsBefore))
	}

	if err := os.WriteFile(filepath.Join(repoB, "main.go"), []byte("package payments\nfunc settlementWorkflow() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.EmbedRepoCode(context.Background(), "team/payments"); err != nil {
		t.Fatal(err)
	}
	orderAfter := svc.bm25.Load().QuerySparse("order")
	if !reflect.DeepEqual(orderBefore, orderAfter) {
		t.Fatalf("order sparse coordinate changed: before=%v after=%v", orderBefore, orderAfter)
	}
	if !reflect.DeepEqual(pointsBefore, semantic.repoPoints("team/orders")) {
		t.Fatal("incremental payment embed changed order repository points")
	}
	if got := svc.bm25.Load().QuerySparse("settlement"); len(got) != 1 {
		t.Fatalf("new repository token not appended to vocabulary: %v", got)
	}
	if got := semantic.repoPoints("team/payments"); len(got) == 0 {
		t.Fatal("incremental payment generation was not stored")
	}
}

func TestBootstrapClearsStaleServiceVectors(t *testing.T) {
	root := t.TempDir()
	db, err := store.Open(filepath.Join(root, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	recorded := newRecordingSemantic()
	recorded.points["stale-service"] = semantic.Record{
		ID: "stale-service", Metadata: map[string]any{"repo": serviceRepoBucket},
	}
	svc := &Service{
		Cfg: config.Config{WorkspaceRoot: root, SQLitePath: filepath.Join(root, "index.db")},
		DB:  db, Semantic: recorded, Embedder: fakeEmbedder{dim: 8}, Graph: graph.New(),
	}
	if err := svc.Bootstrap(context.Background()); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got := recorded.repoPoints(serviceRepoBucket); len(got) != 0 {
		t.Fatalf("stale service vectors remain after bootstrap: %v", got)
	}
}
