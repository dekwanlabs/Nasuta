package store

import (
	"testing"

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
	filter := buildSemanticFilter(SemanticFilter{
		Keywords:   map[string]string{"kind": "memory"},
		AnyInteger: map[string][]int64{"user_id": {42, 0}},
	})
	if filter == nil || len(filter.Must) != 2 {
		t.Fatalf("filter = %#v, want keyword and integer conditions", filter)
	}
}
