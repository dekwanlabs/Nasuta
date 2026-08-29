package qa

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestApplyDiscoverThenSelectKeepsOnlyBusinessDomain(t *testing.T) {
	contract := applyDiscoverThenSelect(TaskContract{
		Objective: "我们的架构是什么样的，有哪些业务",
		EvidenceGoals: []EvidenceGoal{
			{ID: "system_boundary", Facet: "system_boundary", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "business_domain", Facet: "business_domain", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "core_flow", Facet: "core_flow", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		},
		InvestigationGoals: []InvestigationGoal{{
			ID: "details", Objective: "Explain the business areas.", IndependentlyUseful: true,
		}},
	})
	if !contract.DiscoveryPhase || contract.SelectCount != maxInvestigationTasks {
		t.Fatalf("phase = %+v", contract)
	}
	if len(contract.EvidenceGoals) != 1 || contract.EvidenceGoals[0].ID != "business_domain" {
		t.Fatalf("discovery goals = %+v", contract.EvidenceGoals)
	}
	if len(contract.DeferredEvidenceGoals) != 2 {
		t.Fatalf("deferred = %+v", contract.DeferredEvidenceGoals)
	}
	if len(contract.InvestigationGoals) != 1 || contract.InvestigationGoals[0].ID != discoverBusinessesGoalID {
		t.Fatalf("investigation goals = %+v", contract.InvestigationGoals)
	}
}

func TestApplyDiscoverThenSelectIsNoOpWithoutBusinessDomain(t *testing.T) {
	contract := TaskContract{
		Objective: "我们的架构边界是什么",
		EvidenceGoals: []EvidenceGoal{
			{ID: "system_boundary", Facet: "system_boundary", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		},
	}
	next := applyDiscoverThenSelect(contract)
	if next.DiscoveryPhase || next.SelectCount != 0 {
		t.Fatalf("must not invent a discovery wave from wording: %+v", next)
	}
}

func TestApplyDiscoverThenSelectIsNoOpWhenEntitiesNamed(t *testing.T) {
	contract := TaskContract{
		Entities: []EntityRef{{ID: "checkout", Label: "checkout"}},
		EvidenceGoals: []EvidenceGoal{
			{ID: "business_domain", Facet: "business_domain", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		},
	}
	next := applyDiscoverThenSelect(contract)
	if next.DiscoveryPhase {
		t.Fatalf("named subjects must not restart discovery: %+v", next)
	}
}

func TestContinuationContractBindsDiscoveredEntities(t *testing.T) {
	previous := applyDiscoverThenSelect(TaskContract{
		Objective: "我们的架构是什么样的，有哪些业务",
		EvidenceGoals: []EvidenceGoal{
			{ID: "system_boundary", Facet: "system_boundary", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "business_domain", Facet: "business_domain", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "core_flow", Facet: "core_flow", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		},
	})
	next, ok := continuationContract(previous, InvestigationResult{
		DiscoveredEntities: []string{"订单", "支付", "库存", "营销"},
	})
	if !ok {
		t.Fatal("expected bound continuation contract")
	}
	if next.DiscoveryPhase || len(next.Entities) != 3 {
		t.Fatalf("entities = %+v", next.Entities)
	}
	if len(next.InvestigationGoals) != 3 {
		t.Fatalf("investigation goals = %+v", next.InvestigationGoals)
	}
	required := 0
	requiredID := ""
	for _, goal := range next.EvidenceGoals {
		if goal.Required {
			required++
			requiredID = goal.ID
		}
	}
	if required != 1 || requiredID != "business_domain" {
		t.Fatalf("restored required goals = %d id=%q, want business_domain only", required, requiredID)
	}
	if _, ok := continuationContract(next, InvestigationResult{
		UnresolvedEvidenceGoals: []string{"core_flow"},
	}); ok {
		t.Fatal("subject explain wave must not open another facet-fill round")
	}
}

func TestResultHasValidReportRejectsToolJSONClaims(t *testing.T) {
	if resultHasValidReport(InvestigationResult{
		PartialClaims: []InvestigationClaim{{
			Claim: `{"matches":[{"docId":"doc-2015a2bba8c6e812","title":"hsds-product"`,
		}},
	}) {
		t.Fatal("truncated tool JSON must not count as a valid report")
	}
	if !resultHasValidReport(InvestigationResult{
		DiscoveredEntities: []string{"checkout"},
	}) {
		t.Fatal("discovered businesses must count as a valid report")
	}
}

func TestShouldContinuePendingEntitySelection(t *testing.T) {
	if !ShouldContinue(InvestigationRoundContext{
		Round: 2, MaxRounds: 3, PendingEntitySelection: true,
	}) {
		t.Fatal("selected entities must continue even when discovery goals are covered")
	}
	if ShouldContinue(InvestigationRoundContext{
		Round: 2, MaxRounds: 3, UnresolvedEvidenceGoals: []string{"core_flow"},
	}) {
		t.Fatal("unresolved facets without a valid report must not continue")
	}
}

func TestBuildTaskGraphFallbackDiscoveryRoundIsOneDocsTask(t *testing.T) {
	contract := applyDiscoverThenSelect(TaskContract{
		Objective: "我们的架构是什么样的，有哪些业务",
		EvidenceGoals: []EvidenceGoal{
			{ID: "system_boundary", Facet: "system_boundary", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "business_domain", Facet: "business_domain", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "core_flow", Facet: "core_flow", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		},
	})
	proposal, err := buildTaskGraphFallback(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Tasks) != 2 || proposal.Tasks[0].Capability != "knowledge.docs.verify" {
		t.Fatalf("tasks = %+v, want one docs investigator plus synthesizer", proposal.Tasks)
	}
	if len(proposal.Tasks[0].InvestigationGoalIDs) != 1 ||
		proposal.Tasks[0].InvestigationGoalIDs[0] != discoverBusinessesGoalID {
		t.Fatalf("discovery task = %+v", proposal.Tasks[0])
	}
}
