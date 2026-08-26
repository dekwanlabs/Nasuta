package qa

import (
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
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
				Round: 2, MaxRounds: 3,
				UnresolvedEvidenceGoals: []string{"core_flow"},
			},
			want: true,
		},
		{
			name: "last configured round is eligible",
			context: InvestigationRoundContext{
				Round: 3, MaxRounds: 3,
				UnresolvedEvidenceGoals: []string{"core_flow"},
			},
			want: true,
		},
		{
			name: "round limit",
			context: InvestigationRoundContext{
				Round: 4, MaxRounds: 3,
				UnresolvedEvidenceGoals: []string{"core_flow"},
			},
		},
		{
			name:    "goals covered",
			context: InvestigationRoundContext{Round: 2, MaxRounds: 3},
		},
		{
			name: "budget exhausted",
			context: InvestigationRoundContext{
				Round: 2, MaxRounds: 3, UnresolvedEvidenceGoals: []string{"core_flow"},
				BudgetLimit: InvestigationBudget{TotalTokens: 100},
				BudgetUsed:  InvestigationUsage{TotalTokens: 100},
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

func TestTightenContinuationBudgetUsesAggregateRemainingBudget(t *testing.T) {
	proposal := &agentapi.TaskGraphProposal{}
	ok := tightenContinuationBudget(proposal, InvestigationBudget{
		InputTokens: 100, OutputTokens: 50, TotalTokens: 150, ToolCalls: 10, CostMicros: 1_000,
	}, InvestigationUsage{
		InputTokens: 40, OutputTokens: 20, TotalTokens: 60, ToolCalls: 3, CostMicros: 250,
	})
	if !ok {
		t.Fatal("remaining budget was rejected")
	}
	stop := proposal.Stop
	if stop.MaxInputTokens != 60 || stop.MaxOutputTokens != 30 || stop.MaxTotalTokens != 90 ||
		stop.MaxToolCalls != 7 || stop.MaxCostMicros != 750 {
		t.Fatalf("remaining stop policy = %+v", stop)
	}
}

func TestTightenContinuationBudgetDoesNotWidenPlannerLimit(t *testing.T) {
	proposal := &agentapi.TaskGraphProposal{Stop: agentapi.StopPolicy{MaxTotalTokens: 20, MaxToolCalls: 2}}
	if !tightenContinuationBudget(proposal, InvestigationBudget{TotalTokens: 100, ToolCalls: 10}, InvestigationUsage{TotalTokens: 10, ToolCalls: 1}) {
		t.Fatal("remaining budget was rejected")
	}
	if proposal.Stop.MaxTotalTokens != 20 || proposal.Stop.MaxToolCalls != 2 {
		t.Fatalf("planner limit was widened: %+v", proposal.Stop)
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
			EvidenceGoalIDs: []string{"business_domain"},
		}},
		EvidenceUnits:           []tool.EvidenceUnit{unit},
		UnresolvedEvidenceGoals: []string{"core_flow"},
		PartialEvidenceGoals:    []string{"data_state"},
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
		UnresolvedEvidenceGoals: []string{"system_boundary"},
		Round:                   2,
		Verification:            InvestigationVerification{Decision: "partial"},
	}
	merged := MergeRoundResult(previous, current)
	if merged.Answer != "second answer" || len(merged.SupportedClaims) != 2 ||
		len(merged.EvidenceUnits) != 2 || merged.Round != 2 ||
		merged.UnresolvedEvidenceGoals[0] != "system_boundary" {
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
			Claim:           "The route reaches the handler.",
			EvidenceGoalIDs: []string{"core_flow"},
			Evidence: []InvestigationEvidence{{
				Kind: "code", Reference: "route.go", Summary: "route",
			}},
		}},
	}
	current := InvestigationResult{
		SupportedClaims: []InvestigationClaim{{
			ProducerNodeID: "investigate.code", FindingIndex: 1,
			Claim:           "The route reaches the handler and emits the response.",
			EvidenceGoalIDs: []string{"core_flow", "system_boundary"},
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
		len(claim.EvidenceGoalIDs) != 2 || len(claim.Evidence) != 2 {
		t.Fatalf("merged claim = %#v", claim)
	}
}

func TestMergeRoundResultTreatsExplicitEmptyGoalsAsResolved(t *testing.T) {
	previous := InvestigationResult{
		PartialEvidenceGoals:    []string{"data_state"},
		UnresolvedEvidenceGoals: []string{"core_flow"},
	}
	current := InvestigationResult{
		PartialEvidenceGoals:    []string{},
		UnresolvedEvidenceGoals: []string{},
	}

	merged := MergeRoundResult(previous, current)
	if merged.PartialEvidenceGoals == nil || len(merged.PartialEvidenceGoals) != 0 {
		t.Fatalf("partial goals = %#v, want explicit empty list", merged.PartialEvidenceGoals)
	}
	if merged.UnresolvedEvidenceGoals == nil || len(merged.UnresolvedEvidenceGoals) != 0 {
		t.Fatalf("unresolved goals = %#v, want explicit empty list", merged.UnresolvedEvidenceGoals)
	}
}

func TestCloneTaskContractDetachesEvidenceGoalFacets(t *testing.T) {
	original := TaskContract{EvidenceGoals: []EvidenceGoal{{
		ID: "implementation", Facet: "core_flow",
		Facets: []string{"core_flow", "data_and_state"},
	}}}
	cloned := cloneTaskContract(original)
	cloned.EvidenceGoals[0].Facets[0] = "changed"
	if original.EvidenceGoals[0].Facets[0] != "core_flow" {
		t.Fatalf("original facets were mutated: %+v", original.EvidenceGoals[0].Facets)
	}
}
