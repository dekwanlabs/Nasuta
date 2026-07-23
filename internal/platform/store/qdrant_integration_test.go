package store_test

import (
	"os"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/platform/semanticstore"
	"github.com/dekwanlabs/nasuta/internal/semantic/contract"
)

// TestQdrantContract runs the shared semantic contract against a live Qdrant
// instance. Skipped when QDRANT_HOST is not set.
func TestQdrantContract(t *testing.T) {
	host := os.Getenv("QDRANT_HOST")
	if host == "" {
		t.Skip("QDRANT_HOST not set; skipping live Qdrant contract test")
	}
	cfg := config.Load()
	cfg.Semantic.Provider = "qdrant"
	cfg.Semantic.Endpoint = host + ":6334"
	cfg.Semantic.Collection = "codeloom_qdrant_it_test"

	store, err := semanticstore.New(cfg.Semantic)
	if err != nil {
		t.Fatalf("semanticstore.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	contract.Run(t, store)
}
