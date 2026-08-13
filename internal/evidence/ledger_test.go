package evidence

import (
	"testing"

	"github.com/dekwanlabs/nasuta/tool"
)

func TestLedgerExpandsAndPrefersCompleteCoverage(t *testing.T) {
	ledger := New([]tool.EvidenceUnit{{
		SourceKind: "runbook", Target: "doc-a", Sections: []string{"overview", "failure"},
		ContentHash: "source-v1", Coverage: tool.EvidenceCoverage{Partial: true, Included: 1},
	}}, "retrieval")
	conflicts := ledger.Add([]tool.EvidenceUnit{{
		SourceKind: "runbook", Target: "doc-a", Sections: []string{"overview"},
		ContentHash: "source-v1", Coverage: tool.EvidenceCoverage{Complete: true, Included: 2},
	}}, "tool")
	if len(conflicts) != 0 {
		t.Fatalf("conflicts = %#v", conflicts)
	}
	units := ledger.Units()
	if len(units) != 2 || units[0].Sections[0] != "overview" ||
		!units[0].Coverage.Complete || units[0].ContentHash != "source-v1" {
		t.Fatalf("units = %#v", units)
	}
}

func TestLedgerReportsOnlyComparableHashConflicts(t *testing.T) {
	ledger := New([]tool.EvidenceUnit{{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "v1",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}}, "seed")
	if conflicts := ledger.Add([]tool.EvidenceUnit{{
		SourceKind: "runtime", Target: "trace-1",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}}, "unknown"); len(conflicts) != 0 {
		t.Fatalf("empty hash produced conflict: %#v", conflicts)
	}
	conflicts := ledger.Add([]tool.EvidenceUnit{{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "v2",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}}, "tool")
	if len(conflicts) != 1 || conflicts[0].Key.Target != "trace-1" ||
		conflicts[0].Current.ContentHash != "v1" || conflicts[0].Incoming.ContentHash != "v2" {
		t.Fatalf("conflicts = %#v", conflicts)
	}
	if repeated := ledger.Add([]tool.EvidenceUnit{{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "v2",
	}}, "tool"); len(repeated) != 0 {
		t.Fatalf("repeated conflicts = %#v", repeated)
	}
}

func TestLedgerRemembersConflictRegardlessOfVersionOrder(t *testing.T) {
	ledger := New([]tool.EvidenceUnit{{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "v1",
	}}, "seed")
	ledger.RememberConflicts([]Conflict{{
		Key: Key{SourceKind: "runtime", Target: "trace-1"},
		Current: tool.EvidenceUnit{
			SourceKind: "runtime", Target: "trace-1", ContentHash: "v2",
		},
		Incoming: tool.EvidenceUnit{
			SourceKind: "runtime", Target: "trace-1", ContentHash: "v1",
		},
	}})
	if conflicts := ledger.Add([]tool.EvidenceUnit{{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "v2",
	}}, "tool"); len(conflicts) != 0 {
		t.Fatalf("remembered reverse conflict was reported again: %#v", conflicts)
	}
}

func TestLedgerFullyCoversOnlyCompleteUnits(t *testing.T) {
	ledger := New([]tool.EvidenceUnit{{
		SourceKind: "runbook", Target: "doc-a", Sections: []string{"overview", "failure"},
		ContentHash: "v1", Coverage: tool.EvidenceCoverage{Complete: true},
	}}, "seed")
	keys, covered := ledger.FullyCovers(tool.EvidenceScope{
		SourceKind: "runbook", Target: "doc-a", Sections: []string{"overview", "failure"},
	})
	if !covered || len(keys) != 2 {
		t.Fatalf("keys = %#v covered=%t", keys, covered)
	}
	ledger.Add([]tool.EvidenceUnit{{
		SourceKind: "runbook", Target: "doc-b", Sections: []string{"overview"},
		Coverage: tool.EvidenceCoverage{Partial: true},
	}}, "seed")
	if _, covered := ledger.FullyCovers(tool.EvidenceScope{
		SourceKind: "runbook", Target: "doc-b", Sections: []string{"overview"},
	}); covered {
		t.Fatal("partial evidence reported complete coverage")
	}
}
