package retrieval

import (
	"context"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
)

type dependencyTraceTools struct {
	servicePathFakeTools
	trace domain.DependencyTrace
}

func (tools dependencyTraceTools) TraceDeps(context.Context, string, string, int) (domain.DependencyTrace, error) {
	return tools.trace, nil
}

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

func TestCollectDepsPreservesConditionalTraceContract(t *testing.T) {
	tests := []struct {
		name       string
		trace      domain.DependencyTrace
		wantEvents int
		wantParts  int
	}{
		{name: "empty"},
		{
			name:       "edge",
			trace:      domain.DependencyTrace{Downstream: []domain.DependencyEdge{{From: "svc-a", To: "svc-b"}}},
			wantEvents: 1,
			wantParts:  1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var events []domain.EvaluationTrace
			ctx := executiontrace.WithScope(t.Context(), executiontrace.NewScope(executiontrace.Evaluation, func(event domain.EvaluationTrace) {
				events = append(events, event)
			}))
			retrieve := New(dependencyTraceTools{trace: test.trace}, config.Config{})
			var parts []partial
			retrieve.collectDeps(ctx, []string{"svc-a"}, func(part partial) { parts = append(parts, part) })
			if len(events) != test.wantEvents || len(parts) != test.wantParts {
				t.Fatalf("events = %#v, parts = %#v", events, parts)
			}
			if test.wantEvents == 1 {
				event := events[0]
				if event.Node != "dependency_collect" || event.Output["queried_services"] != 1 ||
					event.Output["unqueried_services"] != 0 || event.Output["selected_edges"] != 1 || event.Output["omitted_edges"] != 0 {
					t.Fatalf("event = %#v", event)
				}
			}
		})
	}
}

func TestCollectCodeGraphPreservesSearchTraceContract(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := executiontrace.WithScope(t.Context(), executiontrace.NewScope(executiontrace.Evaluation, func(event domain.EvaluationTrace) {
		events = append(events, event)
	}))
	retrieve := New(nil, config.Config{})
	retrieve.collectCodeGraph(ctx, []string{"checkout"}, []string{"svc-a"}, QueryTerms{}, func(codeDoc) {})
	if len(events) != 1 || events[0].Node != "codegraph_search" || events[0].Output["hits"] != 0 {
		t.Fatalf("events = %#v", events)
	}
	keywords, ok := events[0].Input["keywords"].([]string)
	if !ok || len(keywords) != 1 || keywords[0] != "checkout" {
		t.Fatalf("keywords = %#v", events[0].Input["keywords"])
	}
	services, ok := events[0].Input["services"].([]string)
	if !ok || len(services) != 1 || services[0] != "svc-a" {
		t.Fatalf("services = %#v", events[0].Input["services"])
	}
}
