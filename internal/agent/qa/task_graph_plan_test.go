package qa

import (
	"reflect"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestBindTaskGraphDraftFixesServerOwnedFields(t *testing.T) {
	allowed := []taskGraphCapability{
		{
			ID: "knowledge.code.inspect",
			RequiredFacets: []string{
				"entrypoint",
				"core_flow",
				"data_and_state",
			},
		},
		{
			ID:             "knowledge.service.trace",
			RequiredFacets: []string{"external_dependency"},
		},
	}
	proposal, err := bindTaskGraphDraft(taskGraphDraft{
		Tasks: []taskGraphDraftTask{
			{
				ID: "inspect.flow", Purpose: "Inspect the exact entrypoint and state transitions.",
				Capability: "knowledge.code.inspect",
				RequiredFacets: []string{
					"data_and_state",
					"entrypoint",
					"core_flow",
				},
				DependsOn: []string{},
			},
			{
				ID: "trace.dependencies", Purpose: "Trace external service dependencies.",
				Capability:     "knowledge.service.trace",
				RequiredFacets: []string{"external_dependency"},
				DependsOn:      []string{},
			},
		},
	}, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Tasks) != 3 || len(proposal.Edges) != 2 {
		t.Fatalf("proposal = %+v", proposal)
	}
	code := proposal.Tasks[0]
	if code.ID != "inspect.flow" ||
		code.OutputSchema != (agentapi.SchemaRef{ID: "investigation.report", Version: 1}) ||
		!code.Optional || code.MaxAttempts != 2 ||
		code.ParallelGroup != "" || code.Budget != (agentapi.TaskBudget{}) ||
		!reflect.DeepEqual(code.RequiredFacets, allowed[0].RequiredFacets) {
		t.Fatalf("code task = %+v", code)
	}
	synthesizer := proposal.Tasks[2]
	if synthesizer.ID != "synthesize" ||
		synthesizer.Capability != "evidence.synthesize" ||
		synthesizer.OutputSchema != (agentapi.SchemaRef{
			ID: "investigation.answer", Version: 1,
		}) ||
		synthesizer.Optional || synthesizer.MaxAttempts != 2 {
		t.Fatalf("synthesizer = %+v", synthesizer)
	}
	for _, edge := range proposal.Edges {
		if edge.To != "synthesize" || edge.Required {
			t.Fatalf("edge = %+v", edge)
		}
	}
}

func TestValidateTaskGraphDraftRejectsCapabilityExpansionAndDependencies(t *testing.T) {
	allowed := []taskGraphCapability{
		{ID: "knowledge.code.inspect", RequiredFacets: []string{"core_flow"}},
		{ID: "knowledge.docs.verify", RequiredFacets: []string{"documentation"}},
	}
	valid := taskGraphDraft{Tasks: []taskGraphDraftTask{
		{
			ID: "inspect.code", Purpose: "Inspect code.",
			Capability: "knowledge.code.inspect", RequiredFacets: []string{"core_flow"},
		},
		{
			ID: "verify.docs", Purpose: "Verify docs.",
			Capability: "knowledge.docs.verify", RequiredFacets: []string{"documentation"},
		},
	}}
	expanded := valid
	expanded.Tasks = append([]taskGraphDraftTask(nil), valid.Tasks...)
	expanded.Tasks[1].Capability = "knowledge.web.research"
	if err := validateTaskGraphDraft(expanded, allowed); err == nil ||
		!strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("expanded capability error = %v", err)
	}

	sequential := valid
	sequential.Tasks = append([]taskGraphDraftTask(nil), valid.Tasks...)
	sequential.Tasks[1].DependsOn = []string{"inspect.code"}
	if err := validateTaskGraphDraft(sequential, allowed); err == nil ||
		!strings.Contains(err.Error(), "single-round") {
		t.Fatalf("dependency error = %v", err)
	}

	reserved := valid
	reserved.Tasks = append([]taskGraphDraftTask(nil), valid.Tasks...)
	reserved.Tasks[0].ID = "evidence.verify.custom"
	if err := validateTaskGraphDraft(reserved, allowed); err == nil ||
		!strings.Contains(err.Error(), "invalid or reserved") {
		t.Fatalf("reserved verifier id error = %v", err)
	}
}

func TestTaskGraphCapabilitiesRequireExactContractSources(t *testing.T) {
	capabilities, err := taskGraphCapabilities(TaskContract{
		EvidenceGoals: []EvidenceGoal{
			{
				Facet: "core_flow",
				Sources: []agentapi.EvidenceSource{
					agentapi.EvidenceSourceInternal,
					agentapi.EvidenceSourceWeb,
				},
			},
			{
				Facet: "external_dependency",
				Sources: []agentapi.EvidenceSource{
					agentapi.EvidenceSourceInternal,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []taskGraphCapability{
		{ID: "knowledge.code.inspect", RequiredFacets: []string{"core_flow"}},
		{ID: "knowledge.web.research", RequiredFacets: []string{"core_flow"}},
		{
			ID:             "knowledge.service.trace",
			RequiredFacets: []string{"external_dependency"},
		},
	}
	if !reflect.DeepEqual(capabilities, want) {
		t.Fatalf("capabilities = %+v", capabilities)
	}
}

func TestTaskGraphAllowsMultipleGoalsToShareCapability(t *testing.T) {
	allowed := []taskGraphCapability{
		{ID: "knowledge.code.inspect", RequiredFacets: []string{"core_flow", "data_and_state"}},
	}
	goals := []InvestigationGoal{
		{ID: "business", Objective: "Verify the business behavior."},
		{ID: "implementation", Objective: "Verify the implementation behavior."},
	}
	proposal, err := bindTaskGraphDraft(taskGraphDraft{Tasks: []taskGraphDraftTask{
		{ID: "business", Purpose: "Business", Capability: "knowledge.code.inspect", RequiredFacets: []string{"core_flow"}},
		{ID: "implementation", Purpose: "Implementation", Capability: "knowledge.code.inspect", RequiredFacets: []string{"data_and_state"}},
	}}, allowed, goals...)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Tasks) != 3 ||
		proposal.Tasks[0].Capability != proposal.Tasks[1].Capability ||
		proposal.Tasks[0].Purpose != goals[0].Objective ||
		proposal.Tasks[1].Purpose != goals[1].Objective {
		t.Fatalf("proposal = %+v", proposal)
	}
}

func TestBuildTaskGraphFallbackPreservesGoalsWithSharedCapability(t *testing.T) {
	contract := TaskContract{
		InvestigationGoals: []InvestigationGoal{
			{ID: "first", Objective: "Inspect the first behavior."},
			{ID: "second", Objective: "Inspect the second behavior."},
		},
		EvidenceGoals: []EvidenceGoal{{
			Facet:   "core_flow",
			Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
		}},
	}
	proposal, err := buildTaskGraphFallback(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Tasks) != 3 ||
		proposal.Tasks[0].ID != "first" || proposal.Tasks[1].ID != "second" ||
		proposal.Tasks[0].Capability != "knowledge.code.inspect" ||
		proposal.Tasks[1].Capability != "knowledge.code.inspect" {
		t.Fatalf("proposal = %+v", proposal)
	}
}

func TestBuildTaskGraphFallbackIgnoresRedundantSourceCapabilities(t *testing.T) {
	contract := TaskContract{
		InvestigationGoals: []InvestigationGoal{
			{ID: "flow", Objective: "Inspect the flow."},
			{ID: "dependency", Objective: "Inspect the dependency."},
		},
		EvidenceGoals: []EvidenceGoal{
			{
				Facet: "core_flow",
				Sources: []agentapi.EvidenceSource{
					agentapi.EvidenceSourceInternal,
					agentapi.EvidenceSourceWeb,
				},
			},
			{
				Facet:   "external_dependency",
				Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
			},
		},
	}
	proposal, err := buildTaskGraphFallback(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Tasks) != 3 ||
		proposal.Tasks[0].Capability != "knowledge.code.inspect" ||
		proposal.Tasks[1].Capability != "knowledge.service.trace" {
		t.Fatalf("proposal = %+v", proposal)
	}
}
