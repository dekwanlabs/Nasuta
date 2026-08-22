package qa

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/tool"
)

func TestShouldContinueBoundsRoundsGoalsAndBudget(t *testing.T) {
	tests := []struct {
		name    string
		context InvestigationRoundContext
		want    bool
	}{
		{
			name: "eligible",
			context: InvestigationRoundContext{
				Round: 1, MaxRounds: 3,
				UnresolvedGoals: []string{"core_flow"}, RemainingBudget: 1,
			},
			want: true,
		},
		{
			name: "round limit",
			context: InvestigationRoundContext{
				Round: 3, MaxRounds: 3,
				UnresolvedGoals: []string{"core_flow"}, RemainingBudget: 1,
			},
		},
		{
			name: "goals covered",
			context: InvestigationRoundContext{
				Round: 1, MaxRounds: 3, RemainingBudget: 1,
			},
		},
		{
			name: "budget exhausted",
			context: InvestigationRoundContext{
				Round: 1, MaxRounds: 3,
				UnresolvedGoals: []string{"core_flow"}, RemainingBudget: -1,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ShouldContinue(test.context); got != test.want {
				t.Fatalf("ShouldContinue(%+v) = %v, want %v", test.context, got, test.want)
			}
		})
	}
}

func TestStableRoundWorkflowIDIsDeterministicAndCanonical(t *testing.T) {
	first := StableRoundWorkflowID(" QA/run:42 ", 2)
	second := StableRoundWorkflowID(" QA/run:42 ", 2)
	if first != second {
		t.Fatalf("IDs differ: %q and %q", first, second)
	}
	if first != "workflow_qa_run_42_round_2" {
		t.Fatalf("ID = %q", first)
	}
	if strings.ContainsAny(first, " :/") {
		t.Fatalf("ID contains non-canonical characters: %q", first)
	}
}

func TestEvidenceIdentityExpandsOnlySingleSectionUnits(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "code", Target: "Checkout.Place",
		Sections: []string{"implementation"}, Version: "v1", TimeRange: "today",
	}
	identity, ok := EvidenceIdentity(unit)
	if !ok || identity.SourceKind != "code" || identity.Target != "Checkout.Place" ||
		identity.Section != "implementation" || identity.Version != "v1" ||
		identity.TimeRange != "today" {
		t.Fatalf("identity = %#v, ok=%v", identity, ok)
	}
	if _, ok := EvidenceIdentity(tool.EvidenceUnit{
		SourceKind: "code", Target: "Checkout.Place",
		Sections: []string{"implementation", "calls"},
	}); ok {
		t.Fatal("multi-section evidence unexpectedly has one identity")
	}
}

func TestNewEvidenceRatioUsesCanonicalUnitsAndDeduplicates(t *testing.T) {
	previous := []tool.EvidenceUnit{{
		SourceKind: "code", Target: "Checkout.Place", Sections: []string{"implementation"},
	}}
	current := []tool.EvidenceUnit{
		{SourceKind: "code", Target: "Checkout.Place", Sections: []string{"implementation"}},
		{SourceKind: "code", Target: "Checkout.Place", Sections: []string{"calls", "implementation"}},
		{SourceKind: "runtime", Target: "checkout", Sections: []string{"logs"}},
	}
	if got := NewEvidenceRatio(previous, current); got != 2.0/3.0 {
		t.Fatalf("NewEvidenceRatio = %v, want %v", got, 2.0/3.0)
	}
	if got := NewEvidenceRatio(current, current); got != 0 {
		t.Fatalf("duplicate round ratio = %v, want 0", got)
	}
}

func TestMergeRoundResultDeduplicatesClaimsEvidenceAndConflicts(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "code", Target: "Checkout.Place", Sections: []string{"implementation"},
		ContentHash: "v1",
	}
	previous := InvestigationResult{
		Answer: "first answer",
		SupportedClaims: []InvestigationClaim{{
			ProducerNodeID: "code", FindingIndex: 1, Claim: "same",
			GoalIDs: []string{"business_domain"},
		}},
		EvidenceUnits:   []tool.EvidenceUnit{unit},
		UnresolvedGoals: []string{"core_flow"},
		PartialGoals:    []string{"data_state"},
	}
	current := InvestigationResult{
		Answer: "second answer",
		SupportedClaims: []InvestigationClaim{
			{ProducerNodeID: "code", FindingIndex: 1, Claim: "same"},
			{ProducerNodeID: "runtime", FindingIndex: 2, Claim: "new"},
		},
		EvidenceUnits: []tool.EvidenceUnit{
			unit,
			{SourceKind: "runtime", Target: "checkout", Sections: []string{"logs"}, ContentHash: "v2"},
		},
		UnresolvedGoals: []string{"system_boundary"},
		Round:           2,
		Verification:    InvestigationVerification{Decision: "partial"},
	}
	merged := MergeRoundResult(previous, current)
	if merged.Answer != "second answer" || len(merged.SupportedClaims) != 2 ||
		len(merged.EvidenceUnits) != 2 || merged.Round != 2 ||
		merged.UnresolvedGoals[0] != "system_boundary" {
		t.Fatalf("merged result = %+v", merged)
	}
	if merged.Verification.Decision != "partial" {
		t.Fatalf("verification = %+v", merged.Verification)
	}
	if len(merged.EvidenceConflicts) != 0 {
		t.Fatalf("unexpected conflicts = %+v", merged.EvidenceConflicts)
	}
}

func TestMergeRoundResultUpdatesDuplicateClaimAndRetainsEvidence(t *testing.T) {
	previous := InvestigationResult{
		SupportedClaims: []InvestigationClaim{{
			ProducerNodeID: "investigate.code", FindingIndex: 1,
			Claim:   "The route reaches the handler.",
			GoalIDs: []string{"core_flow"},
			Evidence: []InvestigationEvidence{{
				Kind: "code", Reference: "route.go", Summary: "route",
			}},
		}},
	}
	current := InvestigationResult{
		SupportedClaims: []InvestigationClaim{{
			ProducerNodeID: "investigate.code", FindingIndex: 1,
			Claim:   "The route reaches the handler and emits the response.",
			GoalIDs: []string{"core_flow", "system_boundary"},
			Evidence: []InvestigationEvidence{{
				Kind: "runtime", Reference: "trace-42", Summary: "trace",
			}},
		}},
	}

	merged := MergeRoundResult(previous, current)
	if len(merged.SupportedClaims) != 1 {
		t.Fatalf("supported claims = %#v", merged.SupportedClaims)
	}
	claim := merged.SupportedClaims[0]
	if claim.Claim != current.SupportedClaims[0].Claim ||
		len(claim.GoalIDs) != 2 || len(claim.Evidence) != 2 {
		t.Fatalf("merged claim = %#v", claim)
	}
}

func TestMergeRoundResultTreatsExplicitEmptyGoalsAsResolved(t *testing.T) {
	previous := InvestigationResult{
		PartialGoals:    []string{"data_state"},
		UnresolvedGoals: []string{"core_flow"},
	}
	current := InvestigationResult{
		PartialGoals:    []string{},
		UnresolvedGoals: []string{},
	}

	merged := MergeRoundResult(previous, current)
	if merged.PartialGoals == nil || len(merged.PartialGoals) != 0 {
		t.Fatalf("partial goals = %#v, want explicit empty list", merged.PartialGoals)
	}
	if merged.UnresolvedGoals == nil || len(merged.UnresolvedGoals) != 0 {
		t.Fatalf("unresolved goals = %#v, want explicit empty list", merged.UnresolvedGoals)
	}
}
