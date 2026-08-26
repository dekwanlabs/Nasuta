package qa

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
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
			QueryPlan: domain.QueryPlan{
				Kind: domain.QueryRuntimeDiagnosis, Entities: []string{"Checkout.Place"},
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
		contract.Objective != "Trace the checkout failure" {
		t.Fatalf("contract identity = %+v", contract)
	}
	if !reflect.DeepEqual(contract.Entities, []EntityRef{{ID: "checkout.place"}}) {
		t.Fatalf("entities = %+v", contract.Entities)
	}
	wantInvestigationGoals := []InvestigationGoal{
		{
			ID: "failure_path", Objective: "Trace the failure path.",
			IndependentlyUseful: true, DependsOn: []string{},
		},
		{
			ID: "runtime_impact", Objective: "Assess the runtime impact.",
			IndependentlyUseful: true, DependsOn: []string{},
		},
	}
	if !reflect.DeepEqual(contract.InvestigationGoals, wantInvestigationGoals) {
		t.Fatalf("investigation goals = %+v", contract.InvestigationGoals)
	}
	wantGoals := make([]EvidenceGoal, 0, len(domain.RequiredFacetsFor(domain.QueryRuntimeDiagnosis)))
	for _, facet := range domain.RequiredFacetsFor(domain.QueryRuntimeDiagnosis) {
		value := string(facet)
		wantGoals = append(wantGoals, EvidenceGoal{
			ID: value, Facet: value, Facets: []string{value}, Required: true,
			Sources: []agentapi.EvidenceSource{
				agentapi.EvidenceSourceInternal,
				agentapi.EvidenceSourceWeb,
				agentapi.EvidenceSourceRuntime,
			},
			Freshness: agentapi.FreshnessBoundedLive, MinimumCoverage: 1,
			HighRisk: true,
		})
	}
	if !reflect.DeepEqual(contract.EvidenceGoals, wantGoals) {
		t.Fatalf("evidence goals = %+v", contract.EvidenceGoals)
	}
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatal(err)
	}
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Validate(agentapi.TaskContractSchemaRef(), raw); err != nil {
		t.Fatalf("QA task contract does not match current schema: %v", err)
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

func TestTaskContractObjectiveBoundsParentQuestionFallback(t *testing.T) {
	question := strings.Repeat("investigate the checkout failure path ", 1000)
	objective := taskContractObjective(&preparation{
		request: Request{Question: question},
	})
	if objective == question ||
		tooloutput.EstimateTokens(objective) > investigation.MaxTaskSummaryTokens {
		t.Fatalf("task objective was not bounded: %d tokens", tooloutput.EstimateTokens(objective))
	}
}

func TestCanonicalEntityIdentityMatchesRetrievalMemoryAndTaskContract(t *testing.T) {
	question := "继续检查 PaymentHandler.handle()"
	resolution := domain.ResolveQueryPlan(
		question,
		nil,
		[]string{"PaymentHandler.handle()"},
	)
	_, remembered, _ := memory.CanonicalQuestionMetadata(question)
	contract := contractFromPreparation(&preparation{
		request:  Request{RunID: "qa_1", Question: question},
		analysis: queryAnalysisOutput{QueryPlan: resolution.Plan},
	}, nil)

	want := "paymenthandler.handle"
	if !reflect.DeepEqual(resolution.Plan.Entities, []string{want}) {
		t.Fatalf("query entities = %v", resolution.Plan.Entities)
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

func TestComparisonContractCarriesEntityRolesMinimumCoverageAndRequiredInternalSource(t *testing.T) {
	prepared := &preparation{
		request: Request{RunID: "qa_compare", Question: "Compare the systems."},
		planning: evidencePlanningOutput{Effective: domain.PlanDecision{
			Plan: domain.EvidencePlan{Sources: domain.Internal | domain.Web},
		}},
		analysis: queryAnalysisOutput{QueryPlan: domain.QueryPlan{
			Kind: domain.QueryComparison,
			EntitySpecs: []domain.EntitySpec{
				{ID: "our_agent", Label: "Our Agent", Role: "first_party_agent"},
				{ID: "google", Label: "Google", Role: "external_adapter"},
				{ID: "alexa", Label: "Alexa", Role: "external_adapter"},
			},
		}},
	}

	contract := contractFromPreparation(prepared, nil)
	if len(contract.Entities) != 3 || contract.Entities[0].Role != "first_party_agent" {
		t.Fatalf("entities = %+v", contract.Entities)
	}
	for _, goal := range contract.EvidenceGoals {
		if goal.MinimumCoverage != 3 {
			t.Fatalf("goal %q minimum coverage = %d, want 3", goal.ID, goal.MinimumCoverage)
		}
		if !reflect.DeepEqual(goal.RequiredSources, []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}) {
			t.Fatalf("goal %q required sources = %v", goal.ID, goal.RequiredSources)
		}
		if !reflect.DeepEqual(goal.Sources, []agentapi.EvidenceSource{
			agentapi.EvidenceSourceInternal, agentapi.EvidenceSourceWeb,
		}) {
			t.Fatalf("goal %q sources = %v", goal.ID, goal.Sources)
		}
	}
}

func TestComparisonContractUsesWorkflowSafeIDsForNonCanonicalEntities(t *testing.T) {
	prepared := &preparation{
		request: Request{RunID: "qa_compare", Question: "比较这些系统。"},
		planning: evidencePlanningOutput{Effective: domain.PlanDecision{
			Plan: domain.EvidencePlan{Sources: domain.Internal},
		}},
		analysis: queryAnalysisOutput{QueryPlan: domain.QueryPlan{
			Kind: domain.QueryComparison,
			EntitySpecs: []domain.EntitySpec{
				{ID: "本系统ai集成", Label: "我们AI", Role: "first_party_agent"},
				{ID: "多agent系统", Label: "多agent", Role: "orchestration"},
			},
		}},
	}

	contract := contractFromPreparation(prepared, nil)
	want := []EntityRef{
		{
			ID:    "entity_75cbe4e1e8cee1d5879f90a9f477396b94d02f27a61407c4230618ddd2d16869",
			Label: "我们AI", Role: "first_party_agent", Aliases: []string{"本系统ai集成"},
		},
		{
			ID:    "entity_3155bfec203beb057468cc60152f8af7e017d531cc734ad9f6ff0d93e410d667",
			Label: "多agent", Role: "orchestration", Aliases: []string{"多agent系统"},
		},
	}
	if !reflect.DeepEqual(contract.Entities, want) {
		t.Fatalf("entities = %#v, want %#v", contract.Entities, want)
	}
}

func TestComparisonContractWithoutTwoEntitiesIsExplicitlyInvalidForCoverage(t *testing.T) {
	contract := TaskContract{
		Entities: []EntityRef{{ID: "only-one"}},
		EvidenceGoals: []EvidenceGoal{{
			ID: "core_flow", Facet: "core_flow", Required: true, MinimumCoverage: 2,
		}},
	}
	if err := validateContractEntityCoverage(contract); err == nil || !strings.Contains(err.Error(), "requires 2 subjects") {
		t.Fatalf("coverage error = %v", err)
	}
}
