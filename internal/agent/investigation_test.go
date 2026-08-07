package agent

import (
	"errors"
	"testing"
)

func TestInvestigationOutcomeMapsGroundedAnswer(t *testing.T) {
	result := InvestigationResult{
		WorkflowRunID: "workflow_1",
		Answer:        "  grounded answer  ",
		Citations: []InvestigationCitation{
			{Claim: "claim one", Evidence: []InvestigationEvidence{
				{Kind: "call", Reference: "Checkout.Place", Summary: "calls inventory"},
				{Kind: "call", Reference: "Checkout.Place", Summary: "duplicate"},
			}},
			{Claim: "claim two", Evidence: []InvestigationEvidence{
				{Kind: "doc", Reference: "runbooks/checkout.md", Summary: "checkout runbook"},
				{Kind: "", Reference: "ignored", Summary: "invalid"},
			}},
		},
		Usage: InvestigationUsage{TotalTokens: 91, ToolCalls: 4},
	}

	outcome := investigationOutcome(result, nil)
	if outcome.Status != RunStatusDone || outcome.Answer != "grounded answer" || outcome.TokenUsed != 91 {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.HitCount != 2 || len(outcome.References) != 2 ||
		outcome.References[0].Type != "call" || outcome.References[0].Label != "calls inventory" ||
		outcome.References[0].Target != "Checkout.Place" {
		t.Fatalf("references = %+v", outcome.References)
	}
	if outcome.Evidence.Status != EvidenceComplete || outcome.Evidence.ResultCount != 2 ||
		outcome.Evidence.ToolCallCount != 4 || outcome.Evidence.PartialResultCount != 0 {
		t.Fatalf("evidence = %+v", outcome.Evidence)
	}
}

func TestInvestigationOutcomeMarksLimitationsPartial(t *testing.T) {
	outcome := investigationOutcome(InvestigationResult{
		Answer: "best available answer", Limitations: []string{"live logs unavailable"},
	}, nil)
	if outcome.Status != RunStatusDone || outcome.Evidence.Status != EvidencePartial ||
		outcome.Evidence.PartialResultCount != 1 {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestInvestigationOutcomeRejectsFailureAndEmptyAnswer(t *testing.T) {
	runErr := errors.New("workflow failed")
	failed := investigationOutcome(InvestigationResult{}, runErr)
	if failed.Status != RunStatusFailed || failed.ErrorCode != "investigation_failed" || !errors.Is(failed.Err, runErr) {
		t.Fatalf("failed = %+v", failed)
	}
	empty := investigationOutcome(InvestigationResult{Answer: " \n "}, nil)
	if empty.Status != RunStatusFailed || empty.ErrorCode != "empty_output" || !errors.Is(empty.Err, ErrEmptyAnswer) {
		t.Fatalf("empty = %+v", empty)
	}
}
