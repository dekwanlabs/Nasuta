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
