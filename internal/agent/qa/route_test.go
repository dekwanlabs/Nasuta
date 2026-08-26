package qa

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

func TestAssessExecutionDecomposesBroadEvidenceContract(t *testing.T) {
	contract := TaskContract{EvidenceGoals: []EvidenceGoal{
		{Facet: "system_boundary", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		{Facet: "business_domain", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		{Facet: "core_flow", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
	}}
	suggestion := retrieval.ExecutionSuggestion{
		Strategy: retrieval.ExecutionSingleAgent,
		Tasks: []retrieval.ExecutionTask{{
			ID:                  "overview",
			Objective:           "Explain the architecture and the business areas.",
			IndependentlyUseful: true,
		}},
	}

	assessment := assessExecution(suggestion, contract)
	if !assessment.EvidenceDecomposable {
		t.Fatalf("assessment = %+v, want evidence decomposition", assessment)
	}
	if assessment.IndependentTaskCount != 1 || assessment.Parallelizable {
		t.Fatalf("assessment = %+v, want one model task but server-side decomposition", assessment)
	}
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion:             suggestion,
		Assessment:             assessment,
		Policy:                 ExecutionPolicy{AllowMultiAgent: true},
		InvestigationAvailable: true,
	})
	if decision.Path != executionPathWorkflow || decision.Strategy != retrieval.ExecutionMultiAgent {
		t.Fatalf("decision = %+v, want durable multi-agent workflow", decision)
	}
	if decision.RouteReason != "evidence_goal_decomposition" || decision.PromotionReason != "evidence_goal_decomposition" {
		t.Fatalf("decision = %+v, want evidence decomposition reason", decision)
	}
}

func TestAssessExecutionIgnoresModelCompositionDependencyForEvidenceSplit(t *testing.T) {
	contract := TaskContract{EvidenceGoals: []EvidenceGoal{
		{Facet: "system_boundary", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		{Facet: "business_domain", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		{Facet: "core_flow", Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
	}}
	suggestion := retrieval.ExecutionSuggestion{
		Strategy: retrieval.ExecutionSingleAgent,
		Tasks: []retrieval.ExecutionTask{
			{ID: "overview", Objective: "Explain the system overview.", IndependentlyUseful: true},
			{ID: "details", Objective: "Explain selected core business areas.", IndependentlyUseful: true, DependsOn: []string{"overview"}},
		},
	}

	assessment := assessExecution(suggestion, contract)
	if !assessment.EvidenceDecomposable {
		t.Fatalf("assessment = %+v, model composition dependency must not block evidence split", assessment)
	}
	decision := decideExecutionRoute(executionRouteInput{
		Suggestion: suggestion, Assessment: assessment,
		Policy: ExecutionPolicy{AllowMultiAgent: true}, InvestigationAvailable: true,
	})
	if decision.Path != executionPathWorkflow || decision.RouteReason != "evidence_goal_decomposition" {
		t.Fatalf("decision = %+v, want evidence-decomposed workflow", decision)
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
