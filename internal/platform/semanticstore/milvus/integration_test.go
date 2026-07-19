package milvus_test

import (
	"os"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/platform/semanticstore"
	"github.com/dekwanlabs/nasuta/semantic/contract"
)

// TestMilvusContract runs the shared semantic contract against a live Milvus
// instance. Skipped when MILVUS_HOST is not set. Set SEMANTIC_USERNAME /
// SEMANTIC_PASSWORD too when the instance has auth enabled. The adapter reads
// with Strong consistency, so upsert-then-search is immediately visible.
func TestMilvusContract(t *testing.T) {
	host := os.Getenv("MILVUS_HOST")
	if host == "" {
		t.Skip("MILVUS_HOST not set; skipping live Milvus contract test")
	}
	cfg := config.Load()
	cfg.Semantic.Provider = "milvus"
	cfg.Semantic.Endpoint = host + ":19530"
	cfg.Semantic.Collection = "codeloom_milvus_it_test"

	store, err := semanticstore.New(cfg.Semantic)
	if err != nil {
		t.Fatalf("semanticstore.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	contract.Run(t, store)
}
