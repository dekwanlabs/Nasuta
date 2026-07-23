package indexing

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/platform/graph"
	"github.com/dekwanlabs/nasuta/internal/semantic/contract"
)

func TestOptionalDocStorePathsAreSafe(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repos", "team", "orders"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		Cfg:      config.Config{WorkspaceRoot: root},
		Semantic: contract.NewMemory(),
		Embedder: fakeEmbedder{dim: 4},
		Graph:    graph.New(),
	}

	ctx := context.Background()
	if err := svc.EmbedDocs(ctx); err != nil {
		t.Fatalf("EmbedDocs with nil DocStore: %v", err)
	}
	if err := svc.EmbedDocuments(ctx); err != nil {
		t.Fatalf("EmbedDocuments with nil DocStore: %v", err)
	}
	if err := svc.GenerateDocsForRepo(ctx, "team/orders"); err != nil {
		t.Fatalf("GenerateDocsForRepo with nil DocStore: %v", err)
	}
	if err := svc.embedDocsForRepo(ctx, "team/orders"); err != nil {
		t.Fatalf("embedDocsForRepo with nil DocStore: %v", err)
	}
	svc.Close()
}
