package qa

import (
	"errors"
	"reflect"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestTaskContractFromPreparationCarriesCanonicalContext(t *testing.T) {
	from := time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)
	prepared := &preparation{
		request: Request{
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
		planning: evidencePlanningOutput{
			CleanQuestion: "Trace the checkout failure",
			Effective: domain.PlanDecision{
				Plan: domain.EvidencePlan{Sources: domain.Internal | domain.Web},
			},
			Execution: retrieval.ExecutionSuggestion{
				Strategy: retrieval.ExecutionMultiAgent,
				Tasks: []retrieval.ExecutionTask{
					{
						ID: "failure_path", Objective: "Trace the failure path.",
						IndependentlyUseful: true,
					},
					{
						ID: "runtime_impact", Objective: "Assess the runtime impact.",
						IndependentlyUseful: true,
					},
				},
			},
			RoutedToolIDs: []string{"observe_logs"},
		},
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
		toolCandidates: []retrieval.ToolRouteCandidate{{
			ID: "observe_logs", Temporal: true,
			EvidenceSource: string(tool.RoutingEvidenceRuntime),
		}},
	}

	seedMaterial := []agentapi.ContextBlock{{
		Source: "qa.evidence", Title: "QA Evidence", Content: "checkout evidence",
		Evidence: prepared.request.PreloadedContext[0].Evidence,
		Complete: false, ContentHash: "context-v1",
	}, {
		Source: "qa.duplicate", Title: "Duplicate Evidence", Content: "same evidence",
		Evidence: prepared.request.PreloadedContext[1].Evidence,
		Complete: false, ContentHash: "context-v2",
	}}
	contract := contractFromPreparation(prepared, seedMaterial)
	if contract.TaskID != "qa_1" ||
		contract.Question != "Why is checkout failing?" ||
		contract.Objective != "Trace the checkout failure" {
		t.Fatalf("contract identity = %+v", contract)
	}
	if !reflect.DeepEqual(contract.Entities, []EntityRef{{ID: "checkout.place"}}) {
		t.Fatalf("entities = %+v", contract.Entities)
	}
	wantInvestigationGoals := []InvestigationGoal{
		{
			ID: "failure_path", Objective: "Trace the failure path.",
			IndependentlyUseful: true,
		},
		{
			ID: "runtime_impact", Objective: "Assess the runtime impact.",
			IndependentlyUseful: true,
		},
	}
	if !reflect.DeepEqual(contract.InvestigationGoals, wantInvestigationGoals) {
		t.Fatalf("investigation goals = %+v", contract.InvestigationGoals)
	}
	wantGoals := []EvidenceGoal{
		{
			ID: "entrypoint", Facet: "entrypoint", Required: true,
			Sources: []agentapi.EvidenceSource{
				agentapi.EvidenceSourceInternal,
				agentapi.EvidenceSourceWeb,
				agentapi.EvidenceSourceRuntime,
			},
			Freshness: agentapi.FreshnessBoundedLive, MinimumCoverage: 1,
			HighRisk: true,
		},
		{
			ID: "core_flow", Facet: "core_flow", Required: true,
			Sources: []agentapi.EvidenceSource{
				agentapi.EvidenceSourceInternal,
				agentapi.EvidenceSourceWeb,
				agentapi.EvidenceSourceRuntime,
			},
			Freshness: agentapi.FreshnessBoundedLive, MinimumCoverage: 1,
			HighRisk: true,
		},
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
	if !reflect.DeepEqual(contract.Context.SeedMaterial, seedMaterial) {
		t.Fatalf("seed material = %+v", contract.Context.SeedMaterial)
	}
}

func TestCanonicalEntityIdentityMatchesRetrievalMemoryAndTaskContract(t *testing.T) {
	question := "继续检查 PaymentHandler.handle()"
	resolution := domain.ResolveRetrievalIntent(
		question,
		domain.RetrievalIntentSignals{
			Identifiers: []string{"PaymentHandler.handle()"},
		},
	)
	_, remembered, _ := memory.CanonicalQuestionMetadata(question)
	contract := contractFromPreparation(&preparation{
		request:  Request{RunID: "qa_1", Question: question},
		analysis: queryAnalysisOutput{RetrievalIntent: resolution.Intent},
	}, nil)

	want := "paymenthandler.handle"
	if !reflect.DeepEqual(resolution.Intent.TargetEntities, []string{want}) {
		t.Fatalf("retrieval entities = %v", resolution.Intent.TargetEntities)
	}
	if !reflect.DeepEqual(remembered, []string{want}) {
		t.Fatalf("memory entities = %v", remembered)
	}
	if !reflect.DeepEqual(contract.Entities, []EntityRef{{ID: want}}) {
		t.Fatalf("task contract entities = %+v", contract.Entities)
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
		Completeness:  InvestigationComplete,
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

func TestInvestigationOutcomeUsesWorkflowCompleteness(t *testing.T) {
	result := InvestigationResult{
		Answer: "best available answer", Limitations: []string{"live logs unavailable"},
	}
	outcome, err := investigationOutcome(InvestigationTerminal{
		WorkflowRunID: "workflow_1",
		Status:        InvestigationSucceeded,
		Output:        &result,
		Completeness:  InvestigationPartial,
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
		Completeness:  InvestigationComplete,
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
