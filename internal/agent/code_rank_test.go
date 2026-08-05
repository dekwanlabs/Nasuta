package agent

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
