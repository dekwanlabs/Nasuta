package store

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/qdrant/go-client/qdrant"
)

func TestCodeSparseVectorUsesServerIDF(t *testing.T) {
	if codeSparseVector != "bm25" {
		t.Fatalf("code sparse vector name = %q; want existing bm25", codeSparseVector)
	}
	params := codeSparseVectorParams()
	if params.Modifier == nil || *params.Modifier != qdrant.Modifier_Idf {
		t.Fatalf("code sparse modifier = %v; want IDF", params.Modifier)
	}
}

func TestQdrantSearchRejectsOutOfRangeLimit(t *testing.T) {
	_, err := (&Qdrant{}).Search(context.Background(), semantic.Query{
		DenseVector: []float32{1}, Limit: maxSemanticSearchLimit + 1,
	})
	if err == nil {
		t.Fatal("out-of-range search limit unexpectedly accepted")
	}
}

func TestPayloadToMapPreservesIntegerType(t *testing.T) {
	payload := payloadToMap(map[string]*qdrant.Value{
		"user_id": qdrant.NewValueInt(42),
	})
	userID, ok := payload["user_id"].(int64)
	if !ok || userID != 42 {
		t.Fatalf("user_id = %#v (%T), want int64(42)", payload["user_id"], payload["user_id"])
	}
}

func TestBuildSemanticFilterIncludesIntegerScope(t *testing.T) {
	filter := buildSemanticFilter(semantic.Filter{
		Keywords:   map[string]string{"kind": "memory"},
		AnyInteger: map[string][]int64{"user_id": {42, 0}},
	})
	if filter == nil || len(filter.Must) != 2 {
		t.Fatalf("filter = %#v, want keyword and integer conditions", filter)
	}
}

func TestDeduplicateHitsKeepsBestHitPerGroup(t *testing.T) {
	hits := []semantic.Hit{
		{ID: "a1", Score: 0.9, Metadata: map[string]any{"repo": "a"}},
		{ID: "a2", Score: 0.8, Metadata: map[string]any{"repo": "a"}},
		{ID: "b1", Score: 0.7, Metadata: map[string]any{"repo": "b"}},
	}

	grouped := deduplicateHits(hits, "repo", 2)
	if len(grouped) != 2 || grouped[0].ID != "a1" || grouped[1].ID != "b1" {
		t.Fatalf("grouped hits = %#v, want best hit from a then b", grouped)
	}
}

func TestFuseHybridHitsPreservesBranchSignals(t *testing.T) {
	dense := []semantic.Hit{
		{ID: "dense-only", Score: 0.91, Metadata: map[string]any{"path": "dense.go"}},
		{ID: "both", Score: 0.82, Metadata: map[string]any{"path": "both.go"}},
	}
	sparse := []semantic.Hit{
		{ID: "sparse-only", Score: 12, Metadata: map[string]any{"path": "sparse.go"}},
		{ID: "both", Score: 8, Metadata: map[string]any{"path": "both.go"}},
	}

	hits := fuseHybridHits(dense, sparse, 3, "")
	if len(hits) != 3 {
		t.Fatalf("fused hits = %d, want 3", len(hits))
	}
	if hits[0].ID != "both" {
		t.Fatalf("top hit = %q, want dual-branch hit", hits[0].ID)
	}
	both := hits[0]
	if both.DenseScore != 0.82 || both.SparseScore != 8 || both.DenseRank != 2 || both.SparseRank != 2 {
		t.Fatalf("dual-branch signals = %#v", both)
	}
	if both.ScoreKind != semantic.ScoreFusion || both.Score != both.FusionScore {
		t.Fatalf("fusion metadata = %#v", both)
	}
	if hits[1].ID != "dense-only" || hits[2].ID != "sparse-only" {
		t.Fatalf("single-branch order = [%s %s], want dense before sparse", hits[1].ID, hits[2].ID)
	}
}

func TestFuseHybridHitsGroupsAfterFusion(t *testing.T) {
	dense := []semantic.Hit{
		{ID: "a-dense", Score: 0.9, Metadata: map[string]any{"path": "a.go"}},
		{ID: "b", Score: 0.8, Metadata: map[string]any{"path": "b.go"}},
	}
	sparse := []semantic.Hit{
		{ID: "a-sparse", Score: 9, Metadata: map[string]any{"path": "a.go"}},
		{ID: "c", Score: 8, Metadata: map[string]any{"path": "c.go"}},
	}

	hits := fuseHybridHits(dense, sparse, 3, "path")
	if len(hits) != 3 {
		t.Fatalf("grouped fused hits = %d, want 3", len(hits))
	}
	seen := make(map[string]struct{}, len(hits))
	for _, hit := range hits {
		path := hit.Metadata["path"].(string)
		if _, duplicate := seen[path]; duplicate {
			t.Fatalf("duplicate group %q in %#v", path, hits)
		}
		seen[path] = struct{}{}
	}
}

func TestQdrantAddressAcceptsURLAndHostPort(t *testing.T) {
	tests := []struct {
		endpoint string
		host     string
		port     int
		tls      bool
	}{
		{endpoint: "qdrant:6334", host: "qdrant", port: 6334},
		{endpoint: "https://qdrant.example:7443", host: "qdrant.example", port: 7443, tls: true},
	}
	for _, test := range tests {
		host, port, useTLS, err := qdrantAddress(test.endpoint)
		if err != nil {
			t.Fatalf("qdrantAddress(%q): %v", test.endpoint, err)
		}
		if host != test.host || port != test.port || useTLS != test.tls {
			t.Fatalf("qdrantAddress(%q) = (%q,%d,%v), want (%q,%d,%v)", test.endpoint, host, port, useTLS, test.host, test.port, test.tls)
		}
	}
}
