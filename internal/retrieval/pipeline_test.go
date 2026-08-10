package retrieval

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/tokenestimate"
)

func TestRetrievePlanWebOnlySkipsInternalFanout(t *testing.T) {
	r := New(nil, config.Config{})
	rc, err := r.RetrievePlan(
		context.Background(), "external docs", QueryTerms{},
		domain.EvidencePlan{Sources: domain.Web},
		domain.RetrievalIntent{Kind: domain.RetrievalFocusedFact},
	)
	if err != nil {
		t.Fatalf("RetrievePlan: %v", err)
	}
	if rc.Text != "" || rc.HitCount != 0 {
		t.Fatalf("web-only pre-retrieval = %+v, want empty", rc)
	}
}

func TestRetrievePlanReportsStructuredProgress(t *testing.T) {
	var events []ProgressEvent
	ctx := WithProgress(t.Context(), func(event ProgressEvent) {
		events = append(events, event)
	})
	r := New(servicePathFakeTools{}, config.Config{})
	_, err := r.RetrievePlan(
		ctx, "checkout timeout", QueryTerms{},
		domain.EvidencePlan{Sources: domain.Internal},
		domain.RetrievalIntent{Kind: domain.RetrievalFocusedFact},
	)
	if err != nil {
		t.Fatalf("RetrievePlan: %v", err)
	}
	seen := map[string]bool{}
	for _, event := range events {
		seen[event.Code] = true
	}
	for _, code := range []string{
		"retrieval.embedding", "retrieval.discover", "retrieval.expand", "retrieval.rerank",
	} {
		if !seen[code] {
			t.Fatalf("progress codes = %#v, missing %q", events, code)
		}
	}
}

func TestBudgetForIntent(t *testing.T) {
	tests := []struct {
		intent domain.RetrievalIntentKind
		want   retrievalBudget
	}{
		{domain.RetrievalFocusedFact, retrievalBudget{code: 12, runbook: 8, service: 6, rerank: 20}},
		{domain.RetrievalRuntimeDiagnosis, retrievalBudget{code: 16, runbook: 12, service: 8, rerank: 24}},
		{domain.RetrievalInventory, retrievalBudget{code: 16, runbook: 12, service: 8, rerank: 24}},
		{domain.RetrievalFlow, retrievalBudget{code: 16, runbook: 8, service: 6, rerank: 24}},
		{domain.RetrievalOverview, retrievalBudget{code: 16, runbook: 16, service: 8, rerank: 24}},
	}
	for _, test := range tests {
		if got := budgetForIntent(domain.RetrievalIntent{Kind: test.intent}); got != test.want {
			t.Fatalf("budgetForIntent(%q) = %+v, want %+v", test.intent, got, test.want)
		}
	}
}

func TestDiscoverPreservesRetrievalSourcesTraceContract(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := executiontrace.WithScope(t.Context(), executiontrace.NewScope(executiontrace.Evaluation, func(event domain.EvaluationTrace) {
		events = append(events, event)
	}))
	retrieve := New(servicePathFakeTools{}, config.Config{})
	retrieve.discover(ctx, "checkout", nil, false, nil, false, domain.RetrievalIntent{Kind: domain.RetrievalFocusedFact})
	if len(events) != 1 || events[0].Node != "retrieval_sources" {
		t.Fatalf("events = %#v", events)
	}
	for _, source := range []string{"code", "runbook", "service"} {
		item, ok := events[0].Output[source].(map[string]any)
		if !ok || item["status"] != "empty" || item["candidate_count"] != 0 {
			t.Fatalf("source %q = %#v", source, events[0].Output[source])
		}
	}
}

func TestSortPartsByPriority(t *testing.T) {
	// Intentionally appended in "wrong" order: services first, code last —
	// mirroring how the fanout goroutines append nondeterministically.
	parts := []partial{
		{text: "## Relevant Services\n- foo", priority: partialPriorityService},
		{text: "## Runbooks\n- bar", priority: partialPriorityEvidence},
		{text: "## Code Evidence\n- baz", priority: partialPriorityEvidence},
		{text: "## Matching Endpoints\n- qux", priority: partialPriorityGeneral},
	}
	sortPartsByPriority(parts)
	wantOrder := []string{
		"## Runbooks",
		"## Code Evidence",
		"## Matching Endpoints",
		"## Relevant Services",
	}
	for i, want := range wantOrder {
		if !strings.HasPrefix(parts[i].text, want) {
			t.Errorf("parts[%d] = %q; want prefix %q", i, parts[i].text, want)
		}
	}
}

// TestAssembleRuneSafeTruncation verifies the budget cut lands on a rune
// boundary (no broken UTF-8) and that the highest-priority part survives even
// when a lower-priority part is appended first.
func TestAssembleRuneSafeTruncation(t *testing.T) {
	r := &Retriever{}                                                  // cleanWorkspacePaths is safe with empty repoToSvc
	big := "## Relevant Services\n" + strings.Repeat("元", tokenBudget) // pushes past budget
	parts := []partial{
		{text: big, priority: partialPriorityService},
		{text: "## Code Evidence\n关键代码证据", priority: partialPriorityEvidence},
	}
	rc := r.assemble(context.TODO(), parts, nil, "q")
	if !utf8.ValidString(rc.Text) {
		t.Fatal("assembled context is not valid UTF-8 — truncation cut a multi-byte rune")
	}
	if !strings.Contains(rc.Text, "关键代码证据") {
		t.Fatal("high-priority Code Evidence was dropped; priority ordering not applied")
	}
}

func TestFormatCodePoolPreservesRerankOrderAcrossLayers(t *testing.T) {
	r := &Retriever{}
	pool := []codeDoc{
		{source: "runbook", layer: "docs", filePath: "flow", funcName: "rank-one", text: "main flow"},
		{source: "code", layer: "server", filePath: "repos/svc/Foo.java", text: "branch code"},
	}
	parts := r.formatCodePool(context.Background(), pool)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	if !strings.Contains(parts[0].text, "rank-one") || !strings.Contains(parts[1].text, "Foo.java") {
		t.Fatalf("format order changed: first=%q second=%q", parts[0].text, parts[1].text)
	}
}

func TestFormatCodePoolKeepsCanonicalReferenceTargets(t *testing.T) {
	r := &Retriever{}
	path := "repos/hsds/hsds-aiot-service/service/device_service.py"
	parts := r.formatCodePool(context.Background(), []codeDoc{
		{source: "code", layer: "server", filePath: path, startLine: 12, endLine: 18, text: "def update_shadow(): pass"},
		{source: "codegraph", filePath: path, funcName: "update_shadow", text: "call graph"},
	})
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	for index, part := range parts {
		if len(part.refs) != 1 || part.refs[0].Target != path {
			t.Fatalf("parts[%d] references = %#v, want canonical target %q", index, part.refs, path)
		}
	}
	if parts[0].refs[0].Label == path {
		t.Fatalf("code reference label = %q, want compact display label", parts[0].refs[0].Label)
	}
}

func TestAssembleCountsReferenceForTruncatedEvidence(t *testing.T) {
	r := New(nil, config.Config{}).WithPlatform(&config.PlatformSettings{ContextBudget: 24})
	parts := []partial{{
		text:     "## Evidence\n" + strings.Repeat("证", 100),
		refs:     []Reference{{Type: "runbook", Label: "flow", Target: "flow"}},
		priority: partialPriorityEvidence,
	}}
	rc := r.assemble(context.Background(), parts, nil, "query")
	if got := tokenestimate.Count(rc.Text); got > 24 {
		t.Fatalf("context tokens = %d, want <= 24", got)
	}
	if rc.HitCount != 1 || len(rc.References) != 1 {
		t.Fatalf("references = %d hitCount = %d, want 1/1", len(rc.References), rc.HitCount)
	}
}

func TestCodeHitPassesFloor(t *testing.T) {
	// A disabled floor (0) keeps everything — backward-compatible with the
	// dense-only path.
	if !codeHitPassesFloor(0.05, 0) {
		t.Fatal("floor=0 must admit any score")
	}
	// An enabled floor admits at/above and rejects below, independent of trust.
	if !codeHitPassesFloor(0.40, 0.35) {
		t.Fatal("score at floor must pass")
	}
	if codeHitPassesFloor(0.09, 0.35) {
		t.Fatal("score below floor must be rejected — trust never buys a seat")
	}
}

func TestFusionRecallDoesNotUseDenseScoreFloor(t *testing.T) {
	r := &Retriever{platform: &config.PlatformSettings{CodeMinScore: 0.5}}
	kept := 0
	r.collectCode(context.Background(), []codeHit{{path: "svc/Foo.go", recallScore: 0.1, scoreKind: "rrf"}}, func(codeDoc) {
		kept++
	})
	if kept != 1 {
		t.Fatal("RRF candidate was incorrectly filtered by cosine threshold")
	}
}
