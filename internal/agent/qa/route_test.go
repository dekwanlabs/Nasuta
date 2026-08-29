package qa

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

func TestAssessExecutionKeepsFocusedOverviewWithoutBusinessDomainSingleAgent(t *testing.T) {
	contract := TaskContract{
		Objective: "我们的架构边界是什么",
		EvidenceGoals: []EvidenceGoal{
			{Facet: "system_boundary", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{Facet: "core_flow", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		},
	}
	suggestion := retrieval.ExecutionSuggestion{
		Strategy: retrieval.ExecutionSingleAgent,
		Tasks: []retrieval.ExecutionTask{{
			ID:                  "overview",
			Objective:           "Explain the architecture boundary.",
			IndependentlyUseful: true,
		}},
	}

	assessment := assessExecution(suggestion, contract)
	if assessment.EvidenceDecomposable || assessment.DiscoverThenSelect {
		t.Fatalf("assessment = %+v, overview without unnamed businesses must not split", assessment)
	}
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion:             suggestion,
		Assessment:             assessment,
		Policy:                 ExecutionPolicy{AllowMultiAgent: true},
		InvestigationAvailable: true,
	})
	if decision.Path != executionPathSingle || decision.RouteReason != "insufficient_independent_tasks" {
		t.Fatalf("decision = %+v, want single-agent route", decision)
	}
}

func TestAssessExecutionRoutesUnnamedBusinessDomainToDiscovery(t *testing.T) {
	contract := applyDiscoverThenSelect(TaskContract{
		Objective: "我们的架构是什么样的，有哪些业务",
		EvidenceGoals: []EvidenceGoal{
			{ID: "system_boundary", Facet: "system_boundary", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "business_domain", Facet: "business_domain", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "core_flow", Facet: "core_flow", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		},
		InvestigationGoals: []InvestigationGoal{{
			ID: "details", Objective: "Explain the business areas.",
			IndependentlyUseful: true,
		}},
	})
	if !contract.DiscoveryPhase || len(contract.Entities) != 0 || len(contract.EvidenceGoals) != 1 {
		t.Fatalf("discovery contract = %+v", contract)
	}
	suggestion := retrieval.ExecutionSuggestion{
		Strategy: retrieval.ExecutionSingleAgent,
		Tasks: []retrieval.ExecutionTask{
			{ID: "overview", Objective: "Explain the system overview.", IndependentlyUseful: true},
			{ID: "details", Objective: "Explain selected core business areas.", IndependentlyUseful: true, DependsOn: []string{"overview"}},
		},
	}

	assessment := assessExecution(suggestion, contract)
	if assessment.EvidenceDecomposable || !assessment.DiscoverThenSelect {
		t.Fatalf("assessment = %+v, want discovery routing without capability split", assessment)
	}
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion: suggestion, Assessment: assessment,
		Policy: ExecutionPolicy{AllowMultiAgent: true}, InvestigationAvailable: true,
	})
	if decision.Path != executionPathWorkflow || decision.RouteReason != "discover_then_select" {
		t.Fatalf("decision = %+v, want discover-then-select workflow", decision)
	}
}

func TestAssessExecutionDecomposesWhenEntitiesAreNamed(t *testing.T) {
	contract := TaskContract{
		Entities: []EntityRef{{ID: "checkout"}, {ID: "billing"}},
		EvidenceGoals: []EvidenceGoal{
			{Facet: "system_boundary", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{Facet: "business_domain", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{Facet: "core_flow", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		},
	}
	suggestion := retrieval.ExecutionSuggestion{
		Strategy: retrieval.ExecutionSingleAgent,
		Tasks: []retrieval.ExecutionTask{{
			ID: "overview", Objective: "Explain checkout and billing.", IndependentlyUseful: true,
		}},
	}

	assessment := assessExecution(suggestion, contract)
	if !assessment.EvidenceDecomposable {
		t.Fatalf("assessment = %+v, want evidence decomposition after entities are bound", assessment)
	}
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion:             suggestion,
		Assessment:             assessment,
		Policy:                 ExecutionPolicy{AllowMultiAgent: true},
		InvestigationAvailable: true,
	})
	if decision.Path != executionPathWorkflow || decision.RouteReason != "evidence_goal_decomposition" {
		t.Fatalf("decision = %+v, want durable multi-agent workflow", decision)
	}
}

func TestAssessExecutionKeepsFocusedEvidenceSingleAgent(t *testing.T) {
	contract := TaskContract{EvidenceGoals: []EvidenceGoal{{
		Facet: "entrypoint", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
	}}}
	suggestion := retrieval.ExecutionSuggestion{
		Strategy: retrieval.ExecutionSingleAgent,
		Tasks: []retrieval.ExecutionTask{{
			ID:                  "fact",
			Objective:           "Find the entrypoint.",
			IndependentlyUseful: true,
		}},
	}

	assessment := assessExecution(suggestion, contract)
	if assessment.EvidenceDecomposable {
		t.Fatalf("assessment = %+v, focused request must not be decomposed", assessment)
	}
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion:             suggestion,
		Assessment:             assessment,
		Policy:                 ExecutionPolicy{AllowMultiAgent: true},
		InvestigationAvailable: true,
	})
	if decision.Path != executionPathSingle || decision.RouteReason != "insufficient_independent_tasks" {
		t.Fatalf("decision = %+v, want single-agent route", decision)
	}
}
