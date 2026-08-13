package qa

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestTaskContractFromPreparationCarriesCanonicalContext(t *testing.T) {
	from := time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	prepared := &qaPreparation{
		request: QARequest{
			RunID: "qa_1", Question: "Why is checkout failing?",
			PreloadedContext: []ContextBlock{
				{Evidence: []tool.EvidenceUnit{{
					SourceKind: "code", Target: "Checkout.Place",
					Sections: []string{"implementation"}, ContentHash: "source-v1",
				}}},
				{Evidence: []tool.EvidenceUnit{{
					SourceKind: "code", Target: "Checkout.Place",
					Sections: []string{"implementation"}, ContentHash: "source-v1",
				}}},
			},
		},
		planning: evidencePlanningOutput{CleanQuestion: "Trace the checkout failure"},
		analysis: queryAnalysisOutput{
			HasTimeRange: true,
			TimeRange: tool.TimeRange{
				From: from, To: to, ToExclusive: true, Raw: "yesterday",
			},
			RetrievalIntent: domain.RetrievalIntent{
				TargetEntities: []string{"Checkout.Place"},
				RequiredFacets: []domain.EvidenceFacet{
					domain.FacetEntrypoint, domain.FacetCoreFlow,
				},
			},
		},
		conversationRefs: []ConversationRef{
			{SessionID: "session-1", RunID: "qa_0"},
			{SessionID: "session-1", RunID: "qa_turn_2", Turn: 2},
		},
	}

	contract := taskContractFromPreparation(prepared, prepared.request.PreloadedContext)
	if contract.TaskID != "qa_1" ||
		contract.Question != "Why is checkout failing?" ||
		contract.Objective != "Trace the checkout failure" {
		t.Fatalf("contract identity = %+v", contract)
	}
	if !reflect.DeepEqual(contract.Entities, []EntityRef{{ID: "Checkout.Place"}}) {
		t.Fatalf("entities = %+v", contract.Entities)
	}
	wantGoals := []EvidenceGoal{
		{ID: "entrypoint", Facet: "entrypoint", Required: true},
		{ID: "core_flow", Facet: "core_flow", Required: true},
	}
	if !reflect.DeepEqual(contract.EvidenceGoals, wantGoals) {
		t.Fatalf("evidence goals = %+v", contract.EvidenceGoals)
	}
	if contract.Context.TimeRange == nil ||
		contract.Context.TimeRange.From != from ||
		contract.Context.TimeRange.To != to ||
		!contract.Context.TimeRange.ToExclusive ||
		contract.Context.TimeRange.Raw != "yesterday" {
		t.Fatalf("time range = %+v", contract.Context.TimeRange)
	}
	if !reflect.DeepEqual(contract.Context.ConversationRefs, prepared.conversationRefs) {
		t.Fatalf("conversation refs = %+v", contract.Context.ConversationRefs)
	}
	if len(contract.Context.SeedEvidence) != 1 ||
		contract.Context.SeedEvidence[0].Target != "Checkout.Place" {
		t.Fatalf("seed evidence = %+v", contract.Context.SeedEvidence)
	}
}

func TestInvestigationOutcomeMapsGroundedAnswer(t *testing.T) {
	result := InvestigationResult{
		Answer: "  grounded answer  ",
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
	}

	outcome, err := investigationOutcome(InvestigationTerminal{
		WorkflowRunID: "workflow_1",
		Status:        InvestigationSucceeded,
		Output:        &result,
		Usage:         InvestigationUsage{TotalTokens: 91, ToolCalls: 4},
	})
	if err != nil {
		t.Fatalf("investigationOutcome: %v", err)
	}
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
	result := InvestigationResult{
		Answer: "best available answer", Limitations: []string{"live logs unavailable"},
	}
	outcome, err := investigationOutcome(InvestigationTerminal{
		WorkflowRunID: "workflow_1",
		Status:        InvestigationSucceeded,
		Output:        &result,
	})
	if err != nil {
		t.Fatalf("investigationOutcome: %v", err)
	}
	if outcome.Status != RunStatusDone || outcome.Evidence.Status != EvidencePartial ||
		outcome.Evidence.PartialResultCount != 1 {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestInvestigationOutcomeMapsFailureAndRejectsEmptyAnswer(t *testing.T) {
	failed, err := investigationOutcome(InvestigationTerminal{
		WorkflowRunID: "workflow_1",
		Status:        InvestigationFailed,
		ErrorCode:     "provider_failed",
	})
	if err != nil {
		t.Fatalf("investigationOutcome failure: %v", err)
	}
	if failed.Status != RunStatusFailed || failed.ErrorCode != "provider_failed" {
		t.Fatalf("failed = %+v", failed)
	}
	result := InvestigationResult{Answer: " \n "}
	empty, err := investigationOutcome(InvestigationTerminal{
		WorkflowRunID: "workflow_2",
		Status:        InvestigationSucceeded,
		Output:        &result,
	})
	if err != nil {
		t.Fatalf("investigationOutcome empty: %v", err)
	}
	if empty.Status != RunStatusFailed || empty.ErrorCode != "empty_output" || !errors.Is(empty.Err, ErrEmptyAnswer) {
		t.Fatalf("empty = %+v", empty)
	}
}

func TestInvestigationOutcomeRejectsInvalidTerminalFacts(t *testing.T) {
	tests := []InvestigationTerminal{
		{WorkflowRunID: "workflow_1", Status: InvestigationSucceeded},
		{WorkflowRunID: "workflow_2", Status: InvestigationStatus("unknown")},
	}
	for _, terminal := range tests {
		if _, err := investigationOutcome(terminal); err == nil {
			t.Fatalf("terminal %+v was accepted", terminal)
		}
	}
}
