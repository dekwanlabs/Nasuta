package store

import (
	"context"
	"os"
	"testing"

	"github.com/dekwanlabs/astris/config"
)

// TestQdrantRoundTrip exercises Ensure -> Upsert -> Search -> DeleteByRepo.
// It runs only when QDRANT_HOST is set.
// Use it against a live local or remote Qdrant instance.
func TestQdrantRoundTrip(t *testing.T) {
	host := os.Getenv("QDRANT_HOST")
	if host == "" {
		t.Skip("QDRANT_HOST not set; skipping live Qdrant integration test")
	}
	ctx := context.Background()
	cfg := config.Load()
	cfg.QdrantHost = host
	cfg.QdrantCollection = "dreoverse_it_test"

	s, err := NewSemantic(cfg)
	if err != nil {
		t.Fatalf("NewSemantic: %v", err)
	}
	const dim = 4
	if err := s.Ensure(ctx, dim); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	points := []SemanticPoint{
		{ID: uuid("a1"), Vector: []float32{1, 0, 0, 0}, Payload: map[string]any{"kind": "runbook", "repo": "repoA", "id": "ra"}},
		{ID: uuid("b1"), Vector: []float32{0, 1, 0, 0}, Payload: map[string]any{"kind": "runbook", "repo": "repoB", "id": "rb"}},
	}
	if err := s.Upsert(ctx, points); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := s.Search(ctx, []float32{1, 0, 0, 0}, map[string]string{"kind": "runbook"}, 5, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("Search returned no hits")
	}
	if hits[0].Payload["id"] != "ra" {
		t.Errorf("top hit id = %v, want ra", hits[0].Payload["id"])
	}

	if err := s.DeleteByRepo(ctx, "repoA"); err != nil {
		t.Fatalf("DeleteByRepo: %v", err)
	}
	hits, err = s.Search(ctx, []float32{1, 0, 0, 0}, map[string]string{"kind": "runbook"}, 5, "")
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	for _, h := range hits {
		if h.Payload["repo"] == "repoA" {
			t.Errorf("repoA point still present after DeleteByRepo: %v", h.Payload)
		}
	}
}

// uuid produces a deterministic point id for the test.
func uuid(seed string) string {
	// reuse the same shape as platform.UUIDFromString without importing it here
	return "00000000-0000-5000-8000-0000000000" + seed[len(seed)-2:]
}
