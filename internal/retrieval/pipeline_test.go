package retrieval

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dekwanlabs/astris/config"
	types "github.com/dekwanlabs/astris/internal/domain"
)

func TestRetrievePlanWebOnlySkipsInternalFanout(t *testing.T) {
	r := New(nil, config.Config{})
	rc, err := r.RetrievePlan(
		context.Background(), "external docs", "external docs", QueryTerms{},
		types.EvidencePlan{Sources: types.Web},
	)
	if err != nil {
		t.Fatalf("RetrievePlan: %v", err)
	}
	if rc.Text != "" || rc.HitCount != 0 {
		t.Fatalf("web-only pre-retrieval = %+v, want empty", rc)
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

func TestAssembleCountsReferenceForTruncatedEvidence(t *testing.T) {
	r := New(nil, config.Config{}).WithPlatform(&config.PlatformSettings{ContextBudget: 24})
	parts := []partial{{
		text:     "## Evidence\n" + strings.Repeat("证", 100),
		refs:     []Reference{{Type: "runbook", Label: "flow", Target: "flow"}},
		priority: partialPriorityEvidence,
	}}
	rc := r.assemble(context.Background(), parts, nil, "query")
	if got := len([]rune(rc.Text)); got > 24 {
		t.Fatalf("context runes = %d, want <= 24", got)
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
