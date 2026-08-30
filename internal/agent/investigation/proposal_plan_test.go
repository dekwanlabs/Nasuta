package investigation

import (
	"encoding/json"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestPlanCompilerCompileProposalPreservesGraphAndContract(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatalf("register templates: %v", err)
	}
	contract := InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-proposal", Question: "trace the entrypoint", MaxTasks: 4,
		EvidenceGoals: []EvidenceGoal{{
			ID: "entrypoint", Kind: GoalKindEntrypoint, Facets: []string{"entrypoint"},
			Sources:   []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
			Freshness: agentapi.FreshnessStable, Required: true,
		}},
	}
	proposal := agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{
			{ID: "inspect", Purpose: "inspect the admitted entrypoint", Capability: "knowledge.code.inspect",
				RequiredFacets: []string{"entrypoint"}, Optional: true, MaxAttempts: 2,
				InputRefs: []agentapi.EvidenceRef{{SourceKind: "code", Target: "svc.go", ContentHash: "h1"}}},
			{ID: "inspect_next", Purpose: "confirm the call chain", Capability: "knowledge.code.inspect",
				RequiredFacets: []string{"entrypoint"}, Optional: true, MaxAttempts: 2},
			{ID: "synthesize", Purpose: "compose", Capability: "evidence.synthesize"},
		},
		Edges: []agentapi.TaskEdge{{From: "inspect", To: "inspect_next", Required: true}, {From: "inspect", To: "synthesize"}},
		Stop:  agentapi.StopPolicy{MaxTasks: 3, MaxAttempts: 2},
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish([]agentapi.SchemaDefinition{
		{ID: DefaultTaskInputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
		{ID: DefaultTaskOutputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	plan, err := (PlanCompiler{Catalog: catalog, Schemas: schemas, MaxTasks: 4}).CompileProposal(contract, proposal)
	if err != nil {
		t.Fatalf("compile proposal: %v", err)
	}
	if plan.ProposalHash == "" {
		t.Fatal("proposal hash is empty")
	}
	if len(plan.Tasks) != 3 { // two investigators plus the server-owned verifier
		t.Fatalf("plan tasks = %d, want 3", len(plan.Tasks))
	}
	byID := make(map[string]ExecutableTask, len(plan.Tasks))
	for _, task := range plan.Tasks {
		byID[task.ID] = task
	}
	first := byID["inspect"]
	if first.Capability != "knowledge.code.inspect" || first.InputRefs[0].Target != "svc.go" {
		t.Fatalf("proposal task projection = %+v", first)
	}
	if first.EvidenceGoals[0].Freshness != agentapi.FreshnessStable || first.EvidenceGoals[0].Sources[0] != agentapi.EvidenceSourceInternal {
		t.Fatalf("evidence contract was lost: %+v", first.EvidenceGoals)
	}
	if len(byID["inspect_next"].Dependencies) != 1 || byID["inspect_next"].Dependencies[0] != "inspect" || !byID["inspect_next"].Optional {
		t.Fatalf("edge projection = %+v", byID["inspect_next"])
	}

	changed := proposal
	changed.Tasks = append([]agentapi.TaskSpec(nil), proposal.Tasks...)
	changed.Tasks[0] = proposal.Tasks[0]
	changed.Tasks[0].Purpose = "inspect a different admitted entrypoint"
	other, err := (PlanCompiler{Catalog: catalog, Schemas: schemas, MaxTasks: 4}).CompileProposal(contract, changed)
	if err != nil {
		t.Fatalf("compile changed proposal: %v", err)
	}
	if other.ProposalHash == plan.ProposalHash {
		t.Fatal("different proposals share the same persisted identity")
	}
}

func TestProposalHashIsStableForSameGraph(t *testing.T) {
	proposal := agentapi.TaskGraphProposal{Tasks: []agentapi.TaskSpec{{ID: "task", Purpose: "inspect", Capability: "knowledge.code.inspect"}}}
	first := proposalHash(proposal)
	data, err := json.Marshal(proposal)
	if err != nil || first == "" || len(data) == 0 {
		t.Fatalf("proposal hash setup failed: hash=%q err=%v", first, err)
	}
	if first != proposalHash(proposal) {
		t.Fatal("proposal hash is not deterministic")
	}
}

func TestCompileProposalBindsExplicitGoalsAndFreezesStopPolicy(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatalf("register templates: %v", err)
	}
	contract := InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-policy", Question: "inspect one goal",
		EvidenceGoals: []EvidenceGoal{
			{ID: "entry", Kind: GoalKindEntrypoint, Facets: []string{"entrypoint"}, Required: true},
			{ID: "runtime", Kind: GoalKindRuntimeOperations, Facets: []string{"runtime"}},
		},
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish([]agentapi.SchemaDefinition{
		{ID: DefaultTaskInputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
		{ID: DefaultTaskOutputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	proposal := agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{{
			ID: "inspect", Purpose: "inspect the entrypoint", Capability: "knowledge.code.inspect",
			EvidenceGoalIDs: []string{"entry"}, RequiredFacets: []string{"entrypoint"},
			MaxAttempts: 5,
		}},
		Stop: agentapi.StopPolicy{
			MaxParallelism: 2, MaxRounds: 3, MaxDepth: 2, MaxDuplicateRatio: .25,
			MaxOutputTokens: 100, MaxTotalTokens: 120, MaxToolCalls: 4,
			MaxCostMicros: 50, MaxRetries: 1,
		},
	}
	plan, err := (PlanCompiler{Catalog: catalog, Schemas: schemas, MaxTasks: 4}).CompileProposal(contract, proposal)
	if err != nil {
		t.Fatalf("compile proposal: %v", err)
	}
	if len(plan.Tasks[0].EvidenceGoalIDs) != 1 || plan.Tasks[0].EvidenceGoalIDs[0] != "entry" {
		t.Fatalf("explicit goal binding = %+v", plan.Tasks[0].EvidenceGoalIDs)
	}
	if plan.Tasks[0].Budget.MaxAttempts != 2 {
		t.Fatalf("max retries was not converted to attempts: %d", plan.Tasks[0].Budget.MaxAttempts)
	}
	if plan.Policy.MaxParallelism != 2 || plan.Policy.MaxRounds != 3 || plan.Policy.MaxDepth != 2 || plan.Policy.MaxRetries != 1 {
		t.Fatalf("plan policy = %+v", plan.Policy)
	}
	if plan.Policy.Budget.OutputTokens != 100 || plan.Policy.Budget.TotalTokens != 120 || plan.Policy.Budget.ToolCalls != 4 || plan.Policy.Budget.CostMicros != 50 {
		t.Fatalf("stop budget = %+v", plan.Policy.Budget)
	}
}

func TestCompileProposalCopiesContractEntitiesFromInvestigationGoals(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatalf("register templates: %v", err)
	}
	contract := InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-entities", Question: "explain selected businesses",
		Entities: []string{"checkout", "billing"},
		EntityDetails: []InvestigationEntity{
			{ID: "checkout", Label: "Checkout"},
			{ID: "billing", Label: "Billing"},
		},
		InvestigationGoals: []InvestigationGoal{
			{ID: "checkout", Objective: "Explain checkout.", IndependentlyUseful: true},
			{ID: "billing", Objective: "Explain billing.", IndependentlyUseful: true},
		},
		EvidenceGoals: []EvidenceGoal{{
			ID: "domain", Kind: GoalKindBusinessDomain, Facets: []string{"business_domain"}, Required: true,
		}},
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish([]agentapi.SchemaDefinition{
		{ID: DefaultTaskInputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
		{ID: DefaultTaskOutputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	plan, err := (PlanCompiler{Catalog: catalog, Schemas: schemas, MaxTasks: 4}).CompileProposal(contract, agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{{
			ID: "inspect_checkout", Purpose: "explain checkout", Capability: "knowledge.docs.verify",
			InvestigationGoalIDs: []string{"checkout"}, EvidenceGoalIDs: []string{"domain"},
		}},
	})
	if err != nil {
		t.Fatalf("compile proposal: %v", err)
	}
	byID := make(map[string]ExecutableTask, len(plan.Tasks))
	for _, task := range plan.Tasks {
		byID[task.ID] = task
	}
	if got := byID["inspect_checkout"].Entities; len(got) != 1 || got[0] != "checkout" {
		t.Fatalf("compiled entities = %#v, want the bound subject", got)
	}
	details := byID["inspect_checkout"].EntityDetails
	if len(details) != 1 || details[0].ID != "checkout" || details[0].Label != "Checkout" {
		t.Fatalf("compiled entity details = %#v, want labeled subject", details)
	}
}

func TestCompileProposalRejectsUnknownSourceToSynthesize(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatalf("register templates: %v", err)
	}
	contract := InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-edge", Question: "inspect", EvidenceGoals: []EvidenceGoal{{
			ID: "entry", Kind: GoalKindEntrypoint, Required: true,
		}}}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish([]agentapi.SchemaDefinition{
		{ID: DefaultTaskInputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
		{ID: DefaultTaskOutputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	_, err := (PlanCompiler{Catalog: catalog, Schemas: schemas}).CompileProposal(contract, agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{{ID: "inspect", Purpose: "inspect", Capability: "knowledge.code.inspect"}},
		Edges: []agentapi.TaskEdge{{From: "missing", To: "synthesize"}},
	})
	if err == nil {
		t.Fatal("unknown edge source was accepted")
	}
}

func TestCompileProposalRejectsPlannerOutputSchemaOverride(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatalf("register templates: %v", err)
	}
	contract := InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-schema", Question: "inspect", EvidenceGoals: []EvidenceGoal{{
			ID: "entry", Kind: GoalKindEntrypoint, Facets: []string{"entrypoint"}, Required: true,
		}}}
	_, err := (PlanCompiler{Catalog: catalog, Schemas: testSchemas()}).CompileProposal(contract, agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{{
			ID: "inspect", Purpose: "inspect", Capability: "knowledge.code.inspect",
			OutputSchema: agentapi.SchemaRef{ID: "planner.answer", Version: 1},
		}},
	})
	if err == nil {
		t.Fatal("planner-selected output schema was accepted")
	}
}

func TestValidateProposalOutputSchemaAcceptsCurrentServerOwnedSchema(t *testing.T) {
	task := agentapi.TaskSpec{
		ID:           "inspect",
		OutputSchema: agentapi.InvestigationReportSchemaRef(),
	}
	if err := validateProposalOutputSchema(task); err != nil {
		t.Fatalf("current server-owned schema rejected: %v", err)
	}

	task.OutputSchema.Version--
	if err := validateProposalOutputSchema(task); err == nil {
		t.Fatal("obsolete server-owned schema was accepted")
	}
}

func TestCompileProposalRejectsDuplicateEdges(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatalf("register templates: %v", err)
	}
	contract := InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-duplicate-edge", Question: "inspect", EvidenceGoals: []EvidenceGoal{{
			ID: "entry", Kind: GoalKindEntrypoint, Facets: []string{"entrypoint"}, Required: true,
		}}}
	_, err := (PlanCompiler{Catalog: catalog, Schemas: testSchemas()}).CompileProposal(contract, agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{
			{ID: "inspect", Purpose: "inspect", Capability: "knowledge.code.inspect"},
			{ID: "inspect_next", Purpose: "confirm", Capability: "knowledge.code.inspect"},
		},
		Edges: []agentapi.TaskEdge{
			{From: "inspect", To: "inspect_next"},
			{From: "inspect", To: "inspect_next", Required: true},
		},
	})
	if err == nil {
		t.Fatal("duplicate proposal edges were accepted")
	}
}

func TestCompileProposalRejectsUnknownGoalSelector(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatalf("register templates: %v", err)
	}
	contract := InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-goal", Question: "inspect", EvidenceGoals: []EvidenceGoal{{
			ID: "entry", Kind: GoalKindEntrypoint, Facets: []string{"entrypoint"}, Required: true,
		}}}
	_, err := (PlanCompiler{Catalog: catalog, Schemas: testSchemas()}).CompileProposal(contract, agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{{
			ID: "inspect", Purpose: "inspect", Capability: "knowledge.code.inspect",
			EvidenceGoalIDs: []string{"missing"},
		}},
	})
	if err == nil {
		t.Fatal("unknown goal selector was accepted")
	}
}

func TestCompileProposalReservesServerVerifierOutsideEvidenceTaskLimit(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatalf("register templates: %v", err)
	}
	contract := InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-three-investigators", Question: "describe the architecture",
		EvidenceGoals: []EvidenceGoal{
			{ID: "boundary", Kind: GoalKindSystemBoundary, Required: true},
			{ID: "domain", Kind: GoalKindBusinessDomain, Required: true},
			{ID: "flow", Kind: GoalKindCoreFlow, Required: true},
		},
	}
	proposal := agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{
			{ID: "inspect_boundary", Purpose: "inspect the system boundary", Capability: "knowledge.code.inspect", EvidenceGoalIDs: []string{"boundary"}},
			{ID: "inspect_domain", Purpose: "inspect the business domain", Capability: "knowledge.docs.verify", EvidenceGoalIDs: []string{"domain"}},
			{ID: "inspect_flow", Purpose: "inspect the core flow", Capability: "knowledge.service.trace", EvidenceGoalIDs: []string{"flow"}},
			{ID: "synthesize", Purpose: "compose the answer", Capability: "evidence.synthesize"},
		},
		Stop: agentapi.StopPolicy{MaxTasks: 3, MaxParallelism: 3, MaxRounds: 1},
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish([]agentapi.SchemaDefinition{
		{ID: DefaultTaskInputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
		{ID: DefaultTaskOutputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	plan, err := (PlanCompiler{Catalog: catalog, Schemas: schemas}).CompileProposal(contract, proposal)
	if err != nil {
		t.Fatalf("compile proposal: %v", err)
	}
	if len(plan.Tasks) != 4 {
		t.Fatalf("plan task count = %d, want 4 (three evidence tasks plus verifier)", len(plan.Tasks))
	}
	verifierCount := 0
	var verifier ExecutableTask
	for _, task := range plan.Tasks {
		if task.Executor == ExecutorVerifier {
			verifierCount++
			verifier = task
		}
	}
	if verifierCount != 1 || verifier.ID != "evidence.verify" {
		t.Fatalf("verifier tasks = %d, task = %#v", verifierCount, verifier)
	}
	if len(verifier.Dependencies) != 3 {
		t.Fatalf("verifier dependencies = %#v, want all evidence tasks", verifier.Dependencies)
	}
	for _, id := range []string{"inspect_boundary", "inspect_domain", "inspect_flow"} {
		found := false
		for _, dependency := range verifier.Dependencies {
			if dependency == id {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("verifier dependencies = %#v, missing %q", verifier.Dependencies, id)
		}
	}
}

func TestCompileProposalProjectsOnlyTheTaskEntityIdentityBinding(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatalf("register templates: %v", err)
	}
	contract := InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-identity-projection", Question: "trace two businesses",
		Entities: []string{"checkout", "catalog"},
		EntityDetails: []InvestigationEntity{
			{ID: "checkout", Label: "Checkout", Role: "core_business"},
			{ID: "catalog", Label: "Catalog", Role: "core_business"},
		},
		IdentityBindings: []EntityIdentityBinding{
			{
				EntityID:     "checkout",
				Services:     []ServiceRef{{ID: "service.checkout"}},
				Repositories: []RepositoryRef{{ID: "repository.checkout"}},
				Documents:    []DocumentRef{{ID: "document.checkout"}},
			},
			{
				EntityID: "catalog",
				Services: []ServiceRef{{ID: "service.catalog"}},
			},
		},
		InvestigationGoals: []InvestigationGoal{
			{ID: "checkout", Objective: "trace checkout", IndependentlyUseful: true},
			{ID: "catalog", Objective: "trace catalog", IndependentlyUseful: true},
		},
		EvidenceGoals: []EvidenceGoal{{
			ID: "core_flow", Kind: GoalKindCoreFlow, Facets: []string{"core_flow"}, Required: true,
		}},
	}
	proposal := agentapi.TaskGraphProposal{Tasks: []agentapi.TaskSpec{
		{
			ID: "inspect_checkout", Purpose: "trace checkout", Capability: "knowledge.code.inspect",
			InvestigationGoalIDs: []string{"checkout"}, EvidenceGoalIDs: []string{"core_flow"},
		},
		{
			ID: "inspect_catalog", Purpose: "trace catalog", Capability: "knowledge.code.inspect",
			InvestigationGoalIDs: []string{"catalog"}, EvidenceGoalIDs: []string{"core_flow"},
		},
	}}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish([]agentapi.SchemaDefinition{
		{ID: DefaultTaskInputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
		{ID: DefaultTaskOutputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	plan, err := (PlanCompiler{Catalog: catalog, Schemas: schemas}).CompileProposal(contract, proposal)
	if err != nil {
		t.Fatalf("compile proposal: %v", err)
	}
	for _, task := range plan.Tasks {
		if task.Executor != ExecutorInvestigator {
			continue
		}
		if len(task.Entities) != 1 || len(task.IdentityBindings) != 1 {
			t.Fatalf("task %q projection = entities:%+v bindings:%+v", task.ID, task.Entities, task.IdentityBindings)
		}
		if task.IdentityBindings[0].EntityID != task.Entities[0] {
			t.Fatalf("task %q received another entity binding: %+v", task.ID, task.IdentityBindings)
		}
		if task.IdentityBindings[0].Services[0].ID == task.IdentityBindings[0].EntityID {
			t.Fatalf("task %q reused business id as service id: %+v", task.ID, task.IdentityBindings[0])
		}
	}
}
