package investigation

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDeliveryFallsBackWhenComposerReturnsEmptyText(t *testing.T) {
	contract, report := supportedReport()
	result := (DeliveryGate{}).Deliver(context.Background(), contract, report, ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
		return AnswerDraft{Status: DeliverySucceeded, ClaimIDs: []string{"claim-1"}}, nil
	}))
	if result.Status != DeliverySucceeded {
		t.Fatalf("status = %q", result.Status)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("empty composer output leaked into delivery")
	}
	if result.Failure == nil || result.Failure.Code != FailureComposer {
		t.Fatalf("failure = %#v, want composer failure", result.Failure)
	}
}

func TestDeliveryFallsBackWhenComposerFails(t *testing.T) {
	contract, report := supportedReport()
	result := (DeliveryGate{}).Deliver(context.Background(), contract, report, ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
		return AnswerDraft{}, errors.New("model unavailable")
	}))
	if strings.TrimSpace(result.Text) == "" || result.Failure == nil || result.Failure.Code != FailureComposer {
		t.Fatalf("delivery = %#v, want non-empty fallback and composer failure", result)
	}
}

func TestDeliveryRejectsComposerUnknownClaimAndFallsBack(t *testing.T) {
	contract, report := supportedReport()
	result := (DeliveryGate{}).Deliver(context.Background(), contract, report, ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
		return AnswerDraft{Text: "invented", Status: DeliverySucceeded, ClaimIDs: []string{"unknown"}}, nil
	}))
	if strings.TrimSpace(result.Text) == "" || result.Failure == nil || result.Failure.Code != FailureComposer {
		t.Fatalf("delivery = %#v, want fallback after invalid claim reference", result)
	}
	if strings.Contains(result.Text, "invented") {
		t.Fatalf("invalid composer text was delivered: %q", result.Text)
	}
}

func TestDeliverySkipsComposerDuringDiscoveryPhase(t *testing.T) {
	contract, report := supportedReport()
	contract.DiscoveryPhase = true
	called := false
	result := (DeliveryGate{}).Deliver(context.Background(), contract, report, ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
		called = true
		return AnswerDraft{
			Text:     "should not compose a final answer during discovery",
			Status:   DeliverySucceeded,
			ClaimIDs: []string{"claim-1"},
		}, nil
	}))
	if called {
		t.Fatal("composer ran during discovery")
	}
	if strings.Contains(result.Text, "should not compose") {
		t.Fatalf("discovery delivery used composer text: %q", result.Text)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("discovery delivery is empty")
	}
}

func TestDeliveryWithoutEvidenceStillReturnsExplicitInsufficiency(t *testing.T) {
	contract := InvestigationContract{
		Version:       InvestigationContractVersion,
		ID:            "contract-1",
		Question:      "what happened?",
		EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: "flow", Required: true}},
	}
	result := (DeliveryGate{}).Deliver(context.Background(), contract, InvestigationReport{
		Coverage: []GoalCoverage{{GoalID: "g1", Required: true, Status: GoalUnresolved}},
		Gaps:     []EvidenceGap{{GoalID: "g1", Reason: "no verified claim covers this goal"}},
	}, nil)
	if result.Status != DeliveryEvidenceInsufficient {
		t.Fatalf("status = %q", result.Status)
	}
	if strings.TrimSpace(result.Text) == "" {
		t.Fatal("insufficiency result is empty")
	}
}

func supportedReport() (InvestigationContract, InvestigationReport) {
	return InvestigationContract{
			Version:       InvestigationContractVersion,
			ID:            "contract-1",
			Question:      "how is AI integrated?",
			EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: "integration", Required: true}},
		}, InvestigationReport{
			Claims: []VerifiedClaim{{ID: "claim-1", GoalID: "g1", Text: "the service calls the model", Status: ClaimSupported}},
			Coverage: []GoalCoverage{{
				GoalID: "g1", Required: true, Status: GoalCovered, ClaimIDs: []string{"claim-1"},
			}},
		}
}

func TestDeterministicRendererDoesNotExposeInternalGapIdentifiers(t *testing.T) {
	report := InvestigationReport{
		Gaps: []EvidenceGap{
			{GoalID: "business_domain", Reason: "no verified claim covers this goal"},
			{GoalID: "core_flow", Reason: "no verified claim covers this goal"},
		},
	}

	text := (DeterministicRenderer{}).Render(report)
	for _, internal := range []string{"business_domain", "core_flow", "no verified claim covers this goal", "Investigation limits"} {
		if strings.Contains(text, internal) {
			t.Fatalf("renderer leaked %q in %q", internal, text)
		}
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("renderer returned empty insufficiency text")
	}
}

func TestDeterministicRendererKeepsReadableClaimsAndHidesGapDetails(t *testing.T) {
	report := InvestigationReport{
		Claims: []VerifiedClaim{{ID: "claim-1", GoalID: "core_flow", Text: "请求先经过路由层，再进入问答协调器。", Status: ClaimSupported}},
		Gaps:   []EvidenceGap{{GoalID: "runtime_and_operations", Reason: "no verified claim covers this goal"}},
	}

	text := (DeterministicRenderer{}).Render(report)
	if !strings.Contains(text, "请求先经过路由层") {
		t.Fatalf("renderer omitted readable claim: %q", text)
	}
	if strings.Contains(text, "runtime_and_operations") || strings.Contains(text, "no verified claim covers this goal") {
		t.Fatalf("renderer leaked internal gap details: %q", text)
	}
}

func TestDeliveryStatusRequiresEveryEntityRequiredFacet(t *testing.T) {
	contract := InvestigationContract{
		EntityDetails: []InvestigationEntity{{ID: "checkout"}, {ID: "billing"}},
		EvidenceGoals: []EvidenceGoal{{
			ID: "core_flow", Kind: "core_flow", Facets: []string{"entrypoint", "core_flow"}, Required: true,
		}},
	}
	evidence := EvidenceUnit{ID: "evidence-1", SourceKind: "code", Target: "service", Content: "supported"}
	ref := EvidenceRef{EvidenceID: evidence.ID, SourceKind: evidence.SourceKind, Target: evidence.Target}
	base := InvestigationReport{
		Evidence: []EvidenceUnit{evidence},
		Coverage: []GoalCoverage{{GoalID: "core_flow", Required: true, Status: GoalCovered}},
	}
	tests := []struct {
		name   string
		claims []VerifiedClaim
		want   DeliveryStatus
	}{
		{
			name: "one entity missing",
			claims: []VerifiedClaim{{
				ID: "claim-checkout", GoalID: "core_flow", Status: ClaimSupported,
				EntityIDs: []string{"checkout"}, EvidenceRefs: []EvidenceRef{ref},
			}},
			want: DeliveryPartial,
		},
		{
			name: "claim has no entity binding",
			claims: []VerifiedClaim{{
				ID: "claim-unscoped", GoalID: "core_flow", Status: ClaimSupported,
				EvidenceRefs: []EvidenceRef{ref},
			}},
			want: DeliveryPartial,
		},
		{
			name: "claim has no evidence",
			claims: []VerifiedClaim{{
				ID: "claim-checkout", GoalID: "core_flow", Status: ClaimSupported,
				EntityIDs: []string{"checkout", "billing"},
			}},
			want: DeliveryPartial,
		},
		{
			name: "conflict blocks supported pair",
			claims: []VerifiedClaim{
				{ID: "claim-supported", GoalID: "core_flow", Status: ClaimSupported, EntityIDs: []string{"checkout", "billing"}, EvidenceRefs: []EvidenceRef{ref}},
				{ID: "claim-conflict", GoalID: "core_flow", Status: ClaimConflicting, EntityIDs: []string{"billing"}, EvidenceRefs: []EvidenceRef{ref}},
			},
			want: DeliveryPartial,
		},
		{
			name: "all entity facets covered",
			claims: []VerifiedClaim{
				{ID: "claim-checkout", GoalID: "core_flow", Status: ClaimSupported, EntityIDs: []string{"checkout"}, EvidenceRefs: []EvidenceRef{ref}},
				{ID: "claim-billing", GoalID: "core_flow", Status: ClaimSupported, EntityIDs: []string{"billing"}, EvidenceRefs: []EvidenceRef{ref}},
			},
			want: DeliverySucceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := base
			report.Claims = test.claims
			if got := deliveryStatus(contract, report); got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDeliveryStatusSkipsEntityFacetGateDuringDiscovery(t *testing.T) {
	contract := InvestigationContract{
		DiscoveryPhase: true,
		EntityDetails:  []InvestigationEntity{{ID: "candidate"}},
		EvidenceGoals:  []EvidenceGoal{{ID: "business_domain", Kind: "business_domain", Required: true}},
	}
	report := InvestigationReport{
		Claims:   []VerifiedClaim{{ID: "claim-1", GoalID: "business_domain", Status: ClaimSupported}},
		Coverage: []GoalCoverage{{GoalID: "business_domain", Required: true, Status: GoalCovered}},
	}
	if got := deliveryStatus(contract, report); got != DeliverySucceeded {
		t.Fatalf("status = %q, want succeeded", got)
	}
}
