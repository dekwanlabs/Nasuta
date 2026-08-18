package qa

import (
	"reflect"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestBuildTaskGraphFallbackUsesEvidenceGoalCoverNotInvestigationGoalCount(t *testing.T) {
	contract := TaskContract{
		InvestigationGoals: []InvestigationGoal{
			{ID: "business", Objective: "Explain the business behavior."},
			{ID: "implementation", Objective: "Explain the implementation."},
		},
		EvidenceGoals: []EvidenceGoal{
			{ID: "core_flow", Facet: "core_flow", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "external_dependency", Facet: "external_dependency", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
			{ID: "business_domain", Facet: "business_domain", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		},
	}
	proposal, err := buildTaskGraphFallback(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Tasks) != 4 {
		t.Fatalf("tasks = %+v, want three investigators plus synthesizer", proposal.Tasks)
	}
	wantIDs := []string{"investigate.code.1", "investigate.service.1", "investigate.docs.1", "synthesize"}
	for index, want := range wantIDs {
		if proposal.Tasks[index].ID != want {
			t.Fatalf("task ids = %+v, want %v", proposal.Tasks, wantIDs)
		}
	}
	if proposal.Tasks[0].ID == contract.InvestigationGoals[0].ID || proposal.Tasks[1].ID == contract.InvestigationGoals[1].ID {
		t.Fatal("task identity reused investigation goal identity")
	}
}

func TestBuildTaskGraphFallbackTreatsSourcesAsAlternatives(t *testing.T) {
	contract := TaskContract{EvidenceGoals: []EvidenceGoal{{
		ID: "core_flow", Facet: "core_flow", Required: true,
		Sources: []agentapi.EvidenceSource{
			agentapi.EvidenceSourceInternal,
			agentapi.EvidenceSourceWeb,
			agentapi.EvidenceSourceRuntime,
		},
	}}}
	proposal, err := buildTaskGraphFallback(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Tasks) != 2 || proposal.Tasks[0].Capability != "knowledge.code.inspect" {
		t.Fatalf("proposal = %+v, want one internal investigator plus synthesizer", proposal)
	}
}

func TestSelectCapabilityCoverReturnsPartialAndUncoveredAtBudget(t *testing.T) {
	capabilities := []taskGraphCapability{
		{ID: "knowledge.code.inspect", EvidenceGoalIDs: []string{"entrypoint"}, RequiredFacets: []string{"entrypoint"}},
		{ID: "knowledge.service.trace", EvidenceGoalIDs: []string{"external_dependency"}, RequiredFacets: []string{"external_dependency"}},
		{ID: "knowledge.docs.verify", EvidenceGoalIDs: []string{"business_domain"}, RequiredFacets: []string{"business_domain"}},
	}
	selected, uncovered, err := selectCapabilityCover(
		capabilities,
		[]string{"entrypoint", "external_dependency", "business_domain"},
		2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) != 2 || !reflect.DeepEqual(uncovered, []string{"business_domain"}) {
		t.Fatalf("selected=%+v uncovered=%v", selected, uncovered)
	}
}

func TestTaskGraphCapabilitiesRejectsUncoveredRequiredGoal(t *testing.T) {
	_, err := taskGraphCapabilities(TaskContract{EvidenceGoals: []EvidenceGoal{{
		ID: "unknown", Facet: "unknown", Required: true,
		Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
	}}})
	if err == nil || !strings.Contains(err.Error(), "no investigation capability") {
		t.Fatalf("error = %v", err)
	}
}

func TestTaskGraphCapabilitiesSkipsWebForInternalFacet(t *testing.T) {
	capabilities, err := taskGraphCapabilities(TaskContract{EvidenceGoals: []EvidenceGoal{{
		ID: "entrypoint", Facet: "entrypoint", Required: true,
		Sources: []agentapi.EvidenceSource{
			agentapi.EvidenceSourceInternal,
			agentapi.EvidenceSourceWeb,
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].ID != "knowledge.code.inspect" {
		t.Fatalf("capabilities = %+v, want only code.inspect (web must not cover entrypoint)", capabilities)
	}
}

func TestTaskGraphCapabilitiesKeepsWebForPublicFacet(t *testing.T) {
	capabilities, err := taskGraphCapabilities(TaskContract{EvidenceGoals: []EvidenceGoal{{
		ID: "external_dependency", Facet: "external_dependency", Required: true,
		Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceWeb},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || capabilities[0].ID != "knowledge.web.research" {
		t.Fatalf("capabilities = %+v, want web.research for external_dependency", capabilities)
	}
}

func TestValidateTaskGraphCoverageKeepsValidPlannerSelection(t *testing.T) {
	allowed := []taskGraphCapability{
		{ID: "knowledge.code.inspect", EvidenceGoalIDs: []string{"entrypoint", "core_flow"}, RequiredFacets: []string{"entrypoint", "core_flow"}},
		{ID: "knowledge.web.research", EvidenceGoalIDs: []string{"entrypoint"}, RequiredFacets: []string{"entrypoint"}},
		{ID: "knowledge.service.trace", EvidenceGoalIDs: []string{"external_dependency"}, RequiredFacets: []string{"external_dependency"}},
	}
	draft := taskGraphDraft{Tasks: []taskGraphDraftTask{
		{Purpose: "Inspect code.", Capability: "knowledge.code.inspect", EvidenceGoalIDs: []string{"entrypoint", "core_flow"}},
		{Purpose: "Search the web.", Capability: "knowledge.web.research", EvidenceGoalIDs: []string{"entrypoint"}},
		{Purpose: "Trace dependencies.", Capability: "knowledge.service.trace", EvidenceGoalIDs: []string{"external_dependency"}},
	}}
	err := validateTaskGraphCoverage(
		draft,
		allowed,
		[]string{"entrypoint", "core_flow", "external_dependency"},
		3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.Tasks) != 3 ||
		draft.Tasks[1].Capability != "knowledge.web.research" {
		t.Fatalf("draft=%+v", draft)
	}
}

func TestValidateTaskGraphCoverageRejectsMissingRequiredGoal(t *testing.T) {
	allowed := []taskGraphCapability{
		{ID: "knowledge.code.inspect", EvidenceGoalIDs: []string{"entrypoint", "core_flow"}},
		{ID: "knowledge.service.trace", EvidenceGoalIDs: []string{"external_dependency"}},
	}
	draft := taskGraphDraft{Tasks: []taskGraphDraftTask{{
		Purpose: "Inspect code.", Capability: "knowledge.code.inspect",
		EvidenceGoalIDs: []string{"entrypoint", "core_flow"},
	}}}
	err := validateTaskGraphCoverage(
		draft,
		allowed,
		[]string{"entrypoint", "core_flow", "external_dependency"},
		3,
	)
	if err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("coverage error = %v", err)
	}
}

func TestValidateTaskGraphDraftRejectsCapabilityExpansionAndDependencies(t *testing.T) {
	allowed := []taskGraphCapability{{
		ID: "knowledge.code.inspect", EvidenceGoalIDs: []string{"core_flow"}, RequiredFacets: []string{"core_flow"},
	}}
	expanded := taskGraphDraft{Tasks: []taskGraphDraftTask{{
		Purpose: "Inspect code.", Capability: "knowledge.web.research", EvidenceGoalIDs: []string{"core_flow"},
	}}}
	if err := validateTaskGraphDraft(expanded, allowed, []string{"core_flow"}, 3); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expanded capability error = %v", err)
	}
	sequential := taskGraphDraft{Tasks: []taskGraphDraftTask{{
		Purpose: "Inspect code.", Capability: "knowledge.code.inspect",
		EvidenceGoalIDs: []string{"core_flow"}, DependsOn: []string{"another"},
	}}}
	if err := validateTaskGraphDraft(sequential, allowed, []string{"core_flow"}, 3); err == nil || !strings.Contains(err.Error(), "single-round") {
		t.Fatalf("dependency error = %v", err)
	}
}

func TestBuildTaskGraphFallbackKeepsRequiredInternalSourceSeparateFromWeb(t *testing.T) {
	contract := TaskContract{
		Entities: []EntityRef{{ID: "our_agent"}, {ID: "google"}},
		EvidenceGoals: []EvidenceGoal{
			{ID: "core_flow", Facet: "core_flow", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal, agentapi.EvidenceSourceWeb}, RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}, MinimumCoverage: 2},
			{ID: "external_dependency", Facet: "external_dependency", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal, agentapi.EvidenceSourceWeb}, RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}, MinimumCoverage: 2},
			{ID: "business_domain", Facet: "business_domain", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal, agentapi.EvidenceSourceWeb}, RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}, MinimumCoverage: 2},
		},
	}

	proposal, err := buildTaskGraphFallback(contract)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := make(map[string]struct{}, len(proposal.Tasks))
	for _, task := range proposal.Tasks {
		capabilities[task.Capability] = struct{}{}
	}
	for _, want := range []string{
		"knowledge.code.inspect", "knowledge.service.trace", "knowledge.docs.verify",
	} {
		if _, ok := capabilities[want]; !ok {
			t.Fatalf("fallback capabilities = %v, missing %q", capabilities, want)
		}
	}
	if _, ok := capabilities["knowledge.web.research"]; ok {
		t.Fatalf("fallback selected web as an internal substitute: %v", capabilities)
	}
}

func TestBindTaskGraphDraftRejectsMissingRequiredSource(t *testing.T) {
	contract := TaskContract{
		Entities: []EntityRef{{ID: "our_agent"}, {ID: "google"}},
		EvidenceGoals: []EvidenceGoal{{
			ID: "core_flow", Facet: "core_flow", Required: true,
			Sources:         []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal, agentapi.EvidenceSourceWeb},
			RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}, MinimumCoverage: 2,
		}},
	}
	allowed := []taskGraphCapability{
		{ID: "knowledge.code.inspect", EvidenceGoalIDs: []string{"core_flow"}, RequiredFacets: []string{"core_flow"}, EvidenceSources: map[string][]agentapi.EvidenceSource{
			"core_flow": {agentapi.EvidenceSourceInternal},
		}},
		{ID: "knowledge.web.research", EvidenceGoalIDs: []string{"core_flow"}, RequiredFacets: []string{"core_flow"}, EvidenceSources: map[string][]agentapi.EvidenceSource{
			"core_flow": {agentapi.EvidenceSourceWeb},
		}},
	}
	_, err := bindTaskGraphDraft(taskGraphDraft{Tasks: []taskGraphDraftTask{{
		Purpose: "Search external documentation.", Capability: "knowledge.web.research", EvidenceGoalIDs: []string{"core_flow"},
	}}}, allowed, contract, 3)
	if err == nil || !strings.Contains(err.Error(), "required source") {
		t.Fatalf("required source error = %v", err)
	}
}
