package retrieval

import (
	"context"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestCollectRunbooksUsesMatchedChunksAndDeduplicates(t *testing.T) {
	retrieve := New(nil, config.Config{})
	hits := []domain.RunbookSearchHit{
		{DocID: "flow-1", Title: "flow", DocKind: "event", Chunks: []domain.RunbookChunk{
			{ChunkText: "matched section", SectionHeader: "main", SemanticScore: 0.9},
			{ChunkText: "matched section", SectionHeader: "main", SemanticScore: 0.8},
			{ChunkText: "branch section", SectionHeader: "branch", SemanticScore: 0.7},
		}},
	}
	var docs []codeDoc
	retrieve.collectRunbooks(context.Background(), hits, func(doc codeDoc) { docs = append(docs, doc) })
	if len(docs) != 1 {
		t.Fatalf("docs = %d, want 1", len(docs))
	}
	if strings.Contains(docs[0].text, "FULL DOCUMENT") {
		t.Fatalf("full document leaked into matched-chunk context: %q", docs[0].text)
	}
	if strings.Count(docs[0].text, "matched section") != 1 || !strings.Contains(docs[0].text, "branch section") {
		t.Fatalf("chunk merge = %q", docs[0].text)
	}
}

func TestCollectRunbooksBoundsMergedChunksByRunes(t *testing.T) {
	retrieve := New(nil, config.Config{})
	hit := domain.RunbookSearchHit{
		DocID: "flow-1", Title: "flow",
		Chunks: []domain.RunbookChunk{{
			ChunkText: strings.Repeat("证", 5000), SemanticScore: 0.9,
		}},
	}
	var docs []codeDoc
	retrieve.collectRunbooks(context.Background(), []domain.RunbookSearchHit{hit}, func(doc codeDoc) { docs = append(docs, doc) })
	if len(docs) != 1 {
		t.Fatalf("docs = %d, want 1", len(docs))
	}
	if got := len([]rune(docs[0].text)); got > 4020 {
		t.Fatalf("runbook runes = %d, want bounded near 4000", got)
	}
}

func TestCollectRunbooksKeepsSameTitleDocumentsSeparate(t *testing.T) {
	retrieve := New(nil, config.Config{})
	hits := []domain.RunbookSearchHit{
		{DocID: "domestic", Title: "设备控制流程", Chunks: []domain.RunbookChunk{{ChunkText: "Kafka", SemanticScore: 0.9}}},
		{DocID: "overseas", Title: "设备控制流程", Chunks: []domain.RunbookChunk{{ChunkText: "MQTT", SemanticScore: 0.8}}},
	}
	var docs []codeDoc
	retrieve.collectRunbooks(context.Background(), hits, func(doc codeDoc) { docs = append(docs, doc) })
	if len(docs) != 2 {
		t.Fatalf("docs = %d, want 2", len(docs))
	}
	if docs[0].docID == docs[1].docID || strings.Contains(docs[0].text, docs[1].text) {
		t.Fatalf("same-title documents were merged: %#v", docs)
	}
}
