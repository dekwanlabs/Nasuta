package tools

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/semantic"
)

func TestRankCodeHitsPrefersDiscriminativeQueryCoverage(t *testing.T) {
	hits := []semantic.Hit{
		codeRankTestHit(0.95, "repos/team/workflow/Processor.java", "class Processor { void lookupRecords() {} }"),
		codeRankTestHit(0.72, "repos/team/storage/ledger_entry_index.sql", `
			SELECT * FROM ledger_entries
			WHERE tenant_key = ? AND sequence_no = ?
			ORDER BY tenant_key, sequence_no;
			bulk_write(records);`),
	}

	ranked := rankCodeHits("locate ledger_entries by tenant_key and sequence_no for bulk_write", hits, 2)
	if got := payloadString(ranked[0].hit.Metadata, "path"); got != "repos/team/storage/ledger_entry_index.sql" {
		t.Fatalf("top path = %q, want exact query coverage; ranking = %#v", got, traceRankedCodeHits(ranked))
	}
	if ranked[0].lexicalCoverage <= ranked[1].lexicalCoverage || ranked[0].identityCoverage == 0 {
		t.Fatalf("dynamic query coverage was not reflected: %#v", traceRankedCodeHits(ranked))
	}
}

func TestRankCodeHitsSpreadsDistinctQueryTerms(t *testing.T) {
	hits := []semantic.Hit{
		codeRankTestHit(0.95, "repos/team/alpha_target.go", "func alphaTarget() {}"),
		codeRankTestHit(0.90, "repos/team/alpha_target_helper.go", "func prepareAlphaTarget() {}"),
		codeRankTestHit(0.85, "repos/team/beta_target.go", "func betaTarget() {}"),
		codeRankTestHit(0.84, "repos/team/gamma_target.go", "func gammaTarget() {}"),
	}

	ranked := rankCodeHits("alpha_target beta_target gamma_target implementations", hits, 3)
	got := make(map[string]struct{}, len(ranked))
	for _, candidate := range ranked {
		got[payloadString(candidate.hit.Metadata, "path")] = struct{}{}
	}
	for _, want := range []string{
		"repos/team/alpha_target.go",
		"repos/team/beta_target.go",
		"repos/team/gamma_target.go",
	} {
		if _, ok := got[want]; !ok {
			t.Fatalf("query target %q missing from top 3: %#v", want, traceRankedCodeHits(ranked))
		}
	}
	if _, duplicate := got["repos/team/alpha_target_helper.go"]; duplicate {
		t.Fatalf("duplicate target displaced uncovered query term: %#v", traceRankedCodeHits(ranked))
	}
}

func TestRankCodeHitsFallsBackToBackendOrderWithoutLexicalEvidence(t *testing.T) {
	hits := []semantic.Hit{
		codeRankTestHit(0.91, "repos/team/first.go", "func first() {}"),
		codeRankTestHit(0.83, "repos/team/second.go", "func second() {}"),
	}

	ranked := rankCodeHits("完全不同的查询内容", hits, 2)
	if got := payloadString(ranked[0].hit.Metadata, "path"); got != "repos/team/first.go" {
		t.Fatalf("top path = %q, want backend order; ranking = %#v", got, traceRankedCodeHits(ranked))
	}
}

func TestRankCodeHitsDropsWeakSparseOnlyMatchForMultiTermQuery(t *testing.T) {
	hits := []semantic.Hit{
		{
			ID: "dense", Score: 0.75, FusionScore: 0.75, DenseScore: 0.7,
			DenseRank: 1, ScoreKind: semantic.ScoreFusion,
			Metadata: map[string]any{"path": "repos/team/semantic.go", "repo": "team", "text": "unrelated implementation"},
		},
		{
			ID: "weak-sparse", Score: 0.25, FusionScore: 0.25, SparseScore: 9,
			SparseRank: 1, ScoreKind: semantic.ScoreFusion,
			Metadata: map[string]any{"path": "repos/team/noise.go", "repo": "team", "text": "generic retry helper"},
		},
	}

	ranked := rankCodeHits("checkout timeout retry policy", hits, 5)
	if len(ranked) != 1 || ranked[0].hit.ID != "dense" {
		t.Fatalf("ranked hits = %#v, want weak sparse-only hit removed", traceRankedCodeHits(ranked))
	}
}

func TestRankCodeHitsKeepsSparseOnlyIdentityMatch(t *testing.T) {
	hits := []semantic.Hit{
		{
			ID: "identity", Score: 0.25, FusionScore: 0.25, SparseScore: 9,
			SparseRank: 1, ScoreKind: semantic.ScoreFusion,
			Metadata: map[string]any{
				"path": "repos/team/CheckoutCoordinator.java",
				"repo": "team",
				"text": "class CheckoutCoordinator {}",
			},
		},
	}

	ranked := rankCodeHits("find CheckoutCoordinator timeout behavior", hits, 5)
	if len(ranked) != 1 || ranked[0].hit.ID != "identity" {
		t.Fatalf("identity hit was removed: %#v", traceRankedCodeHits(ranked))
	}
}

func TestRankCodeHitsKeepsDenseCandidateWithoutLexicalOverlap(t *testing.T) {
	hit := semantic.Hit{
		ID: "semantic", Score: 0.75, FusionScore: 0.75, DenseScore: 0.84,
		DenseRank: 1, ScoreKind: semantic.ScoreFusion,
		Metadata: map[string]any{"path": "repos/team/semantic.go", "repo": "team", "text": "different vocabulary"},
	}

	ranked := rankCodeHits("checkout timeout retry policy", []semantic.Hit{hit}, 5)
	if len(ranked) != 1 || ranked[0].hit.ID != "semantic" {
		t.Fatalf("dense semantic hit was removed: %#v", traceRankedCodeHits(ranked))
	}
}

func codeRankTestHit(score float32, path, text string) semantic.Hit {
	return semantic.Hit{
		Score: score,
		Metadata: map[string]any{
			"path": path,
			"repo": "team",
			"text": text,
		},
	}
}
