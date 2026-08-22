package workflow

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

func TestProposalCompilerPinsCapabilitiesAndInsertsJoin(t *testing.T) {
	compiler, schemas, agents, _ := proposalTestCompiler(t)
	proposal := proposalTestGraph()
	proposal.Tasks[0].Budget.MaxOutputTokens = 40
	proposal.Tasks[0].Purpose = "Planner text must not become an agent prompt."
	proposal.Tasks[0].InvestigationGoalIDs = []string{"implementation_review"}
	definition, err := compiler.Compile(proposal, proposalTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if definition.ContentHash == "" || len(definition.Nodes) != 4 ||
		definition.Budget.MaxNodes != 4 || definition.Budget.MaxParallelism != 2 {
		t.Fatalf("compiled workflow = %+v", definition)
	}
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	code := nodes["inspect.code"]
	if code.Agent != (agentapi.DefinitionRef{ID: "proposal.code", Version: 1}) ||
		code.Capability != (agentapi.CapabilityRef{ID: "knowledge.code.inspect", Version: 1}) ||
		code.Task == nil ||
		code.Task.Purpose != "Planner text must not become an agent prompt." ||
		len(code.Task.InvestigationGoalIDs) != 1 ||
		code.Task.InvestigationGoalIDs[0] != "implementation_review" ||
		len(code.Task.RequiredFacets) != 1 ||
		code.Task.RequiredFacets[0] != "implementation" ||
		code.Task.ParallelGroup != "investigation" ||
		code.CapabilityMaxConcurrency != 2 || !code.RestrictVisibleTools ||
		len(code.VisibleToolIDs) != 1 || code.VisibleToolIDs[0] != "search_code" ||
		code.Budget.MaxOutputTokens != 40 || code.Retry.MaxAttempts != 2 ||
		code.Optional || !code.RetrySafe {
		t.Fatalf("compiled code node = %+v", code)
	}
	join := nodes["evidence.join"]
	if join.Kind != NodeJoin ||
		join.RejectEvidenceConflicts ||
		join.InputSchema != (agentapi.SchemaRef{ID: "review.report", Version: 1}) ||
		join.OutputSchema != (agentapi.SchemaRef{ID: "review.report.list", Version: 1}) {
		t.Fatalf("compiled join = %+v", join)
	}
	wantEdges := map[string]bool{
		"inspect.code\x00evidence.join": true,
		"inspect.docs\x00evidence.join": false,
		"evidence.join\x00synthesize":   true,
	}
	if len(definition.Edges) != len(wantEdges) {
		t.Fatalf("compiled edges = %+v", definition.Edges)
	}
	for _, edge := range definition.Edges {
		required, ok := wantEdges[edge.From+"\x00"+edge.To]
		if !ok || edge.Required != required {
			t.Fatalf("compiled edge = %+v", edge)
		}
	}
	if err := NewCatalog(schemas, agents).Publish([]Definition{definition}); err != nil {
		t.Fatalf("publish compiled workflow: %v", err)
	}
}

func TestProposalCompilerOwnsVerifierBoundary(t *testing.T) {
	compiler, _, _, _ := proposalTestCompiler(t)
	proposal := proposalTestGraph()
	proposal.Stop.MaxDepth = 4
	policy := proposalTestPolicy()
	policy.Budget.MaxNodes = 5
	policy.Budget.MaxDepth = 4
	policy.MaxDepth = 4
	policy.VerifierID = "evidence.verify"
	policy.VerifierInputSchema = agentapi.SchemaRef{
		ID: "review.report.list", Version: 1,
	}
	policy.VerifierOutputSchema = agentapi.SchemaRef{
		ID: "review.report.list", Version: 1,
	}

	definition, err := compiler.Compile(proposal, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.Nodes) != 5 || definition.Budget.MaxNodes != 5 {
		t.Fatalf("compiled workflow = %+v", definition)
	}
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	edges := make(map[string]bool, len(definition.Edges))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range definition.Edges {
		edges[edge.From+"\x00"+edge.To] = edge.Required
	}
	verifier := nodes["evidence.verify"]
	if verifier.Kind != NodeVerifier || verifier.Verifier == nil ||
		verifier.Verifier.RejectEvidenceConflicts ||
		!edges["evidence.join\x00evidence.verify"] ||
		!edges["evidence.verify\x00synthesize"] {
		t.Fatalf("server-owned verifier = node:%+v edges:%+v", verifier, definition.Edges)
	}

	tooShallow := policy
	tooShallow.MaxDepth = 3
	shallowProposal := proposal
	shallowProposal.Stop.MaxDepth = 3
	if _, err := compiler.Compile(shallowProposal, tooShallow); err == nil ||
		!strings.Contains(err.Error(), "depth 4 exceeds limit 3") {
		t.Fatalf("verifier depth error = %v", err)
	}

	tooSmall := policy
	tooSmall.Budget.MaxNodes = 4
	if _, err := compiler.Compile(proposal, tooSmall); err == nil ||
		!strings.Contains(err.Error(), "has 5 nodes") {
		t.Fatalf("verifier node budget error = %v", err)
	}

	conflicting := proposal
	conflicting.Tasks = append([]agentapi.TaskSpec(nil), proposal.Tasks...)
	conflicting.Edges = append([]agentapi.TaskEdge(nil), proposal.Edges...)
	conflicting.Tasks[0].ID = policy.VerifierID
	for index := range conflicting.Edges {
		if conflicting.Edges[index].From == "inspect.code" {
			conflicting.Edges[index].From = policy.VerifierID
		}
	}
	if _, err := compiler.Compile(conflicting, policy); err == nil ||
		!strings.Contains(err.Error(), `compiled verifier id "evidence.verify" conflicts`) {
		t.Fatalf("planner-owned verifier error = %v", err)
	}
}

func TestProposalCompilerTracesProposedAndAcceptedGraphWithoutPurposeText(t *testing.T) {
	compiler, _, _, _ := proposalTestCompiler(t)
	proposal := proposalTestGraph()
	proposal.Tasks[0].Purpose = "planner-private-purpose"
	var traces []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		traces = append(traces, event)
	})
	scope := runtrace.Begin(ctx)
	ctx = runtrace.WithScope(ctx, scope)
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{RunID: "qa-parent"})
	definition, err := compiler.CompileContext(
		ctx,
		proposal,
		proposalTestPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	scope.Close()
	if len(traces) != 2 ||
		traces[0].Node != "task_graph.proposed" ||
		traces[1].Node != "task_graph.accepted" {
		t.Fatalf("task graph traces = %#v", traces)
	}
	if traces[0].Output["accepted"] != true ||
		traces[1].Input["workflow_hash"] != definition.ContentHash {
		t.Fatalf("task graph trace outputs = %#v", traces)
	}
	encoded, err := json.Marshal(traces)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("planner-private-purpose")) {
		t.Fatalf("task graph trace leaked planner purpose: %s", encoded)
	}
}

func TestProposalCompilerTracesRejectedGraph(t *testing.T) {
	compiler, _, _, _ := proposalTestCompiler(t)
	proposal := proposalTestGraph()
	proposal.Tasks[0].Capability = "unknown.capability"
	var traces []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		traces = append(traces, event)
	})
	scope := runtrace.Begin(ctx)
	ctx = runtrace.WithScope(ctx, scope)
	_, err := compiler.CompileContext(ctx, proposal, proposalTestPolicy())
	scope.Close()
	if err == nil {
		t.Fatal("rejected proposal unexpectedly compiled")
	}
	if len(traces) != 1 ||
		traces[0].Node != "task_graph.proposed" ||
		traces[0].Status != "failed" ||
		traces[0].Output["accepted"] != false {
		t.Fatalf("rejected task graph traces = %#v", traces)
	}
}

func TestProposalCompilerRejectsPlannerExpansion(t *testing.T) {
	tests := []struct {
		name   string
		change func(*agentapi.TaskGraphProposal, *CompilationPolicy)
		want   string
	}{
		{
			name: "stop policy",
			change: func(proposal *agentapi.TaskGraphProposal, _ *CompilationPolicy) {
				proposal.Stop.MaxParallelism = 3
			},
			want: "exceeds server limit",
		},
		{
			name: "node budget",
			change: func(proposal *agentapi.TaskGraphProposal, _ *CompilationPolicy) {
				proposal.Tasks[0].Budget.MaxToolCalls = 3
			},
			want: "exceeds server limit",
		},
		{
			name: "capability schema",
			change: func(proposal *agentapi.TaskGraphProposal, _ *CompilationPolicy) {
				proposal.Tasks[0].OutputSchema = agentapi.SchemaRef{
					ID: "review.report.list", Version: 1,
				}
			},
			want: "must match capability",
		},
		{
			name: "dependent parallel tasks",
			change: func(proposal *agentapi.TaskGraphProposal, _ *CompilationPolicy) {
				proposal.Tasks[2].ParallelGroup = "investigation"
			},
			want: "contains dependent tasks",
		},
		{
			name: "caller permission",
			change: func(_ *agentapi.TaskGraphProposal, policy *CompilationPolicy) {
				policy.CallerPermissions.Scopes = nil
			},
			want: "proposal workflow permissions",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			compiler, _, _, _ := proposalTestCompiler(t)
			proposal := proposalTestGraph()
			policy := proposalTestPolicy()
			test.change(&proposal, &policy)
			if _, err := compiler.Compile(proposal, policy); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("Compile error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestProposalCompilerAllowsDuplicateRatioTightening(t *testing.T) {
	compiler, _, _, _ := proposalTestCompiler(t)
	proposal := proposalTestGraph()
	proposal.Stop.MaxDuplicateRatio = 0.6
	policy := proposalTestPolicy()
	policy.Budget.MaxDuplicateRatio = 0.8

	definition, err := compiler.Compile(proposal, policy)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Budget.MaxDuplicateRatio != 0.6 {
		t.Fatalf(
			"compiled duplicate ratio = %v, want 0.6",
			definition.Budget.MaxDuplicateRatio,
		)
	}

	proposal.Stop.MaxDuplicateRatio = 0.9
	if _, err := compiler.Compile(proposal, policy); err == nil ||
		!strings.Contains(err.Error(), "exceeds server limit") {
		t.Fatalf("expanded duplicate ratio error = %v", err)
	}
}

func TestProposalCompilerAllowsOptionalTaskForRequiredGoal(t *testing.T) {
	compiler, _, _, _ := proposalTestCompiler(t)
	proposal := proposalTestGraph()
	proposal.Tasks[0].Optional = true
	if _, err := compiler.Compile(proposal, proposalTestPolicy()); err != nil {
		t.Fatalf("Compile optional required-goal task: %v", err)
	}
}

func TestProposalCompilerRejectsDisabledCapability(t *testing.T) {
	compiler, _, _, capabilities := proposalTestCompiler(t)
	disabled := proposalCodeCapability()
	disabled.Version = 2
	disabled.Enabled = false
	if err := capabilities.Publish([]agentapi.Capability{disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.Compile(
		proposalTestGraph(),
		proposalTestPolicy(),
	); err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("Compile error = %v, want disabled capability rejection", err)
	}
}

func TestProposalCompilerRequiresRetrySafeWriteCapability(t *testing.T) {
	compiler, _, agents, capabilities := proposalTestCompiler(t)
	writeAgent, err := agentapi.Prepare(agentapi.Definition{
		ID: "proposal.writer", Version: 1, Purpose: "Write one proposal artifact.",
		Prompt:       agentapi.PromptSpec{System: "Write the artifact.", Version: "1"},
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "review.report", Version: 1},
		Model: agentapi.ModelPolicy{
			Provider: "openai", Model: "test", MaxOutputTokens: 100,
		},
		Tools: agentapi.ToolPolicy{
			VisibleToolIDs: []string{"write_artifact"}, RestrictVisible: true,
			AllowWrite: true,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout: time.Second, MaxSteps: 1, ContextTokens: 1000,
		},
		Permissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read", "knowledge.write"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	agents.definitions[agentapi.DefinitionRef{
		ID: writeAgent.ID, Version: writeAgent.Version,
	}] = writeAgent
	writeCapability := agentapi.Capability{
		ID: "change.write", Version: 1, Purpose: "Write one artifact.",
		Role:         agentapi.RoleInvestigator,
		InputFacets:  []string{"implementation"},
		InputSchema:  writeAgent.InputSchema,
		OutputSchema: writeAgent.OutputSchema,
		ToolIDs:      []string{"write_artifact"},
		PermissionScope: []string{
			"knowledge.read", "knowledge.write",
		},
		Freshness:      agentapi.FreshnessStable,
		SideEffects:    agentapi.SideEffectWrite,
		RetrySafe:      false,
		MaxConcurrency: 1,
		Enabled:        true,
		Agent:          agentapi.DefinitionRef{ID: writeAgent.ID, Version: 1},
		WriteSet:       []string{"artifact"},
	}
	if err := capabilities.Publish([]agentapi.Capability{writeCapability}); err != nil {
		t.Fatal(err)
	}
	proposal := agentapi.TaskGraphProposal{Tasks: []agentapi.TaskSpec{{
		ID: "write", Purpose: "Write the artifact.",
		RequiredFacets: []string{"implementation"},
		Capability:     writeCapability.ID,
		OutputSchema:   writeCapability.OutputSchema,
		MaxAttempts:    2,
	}}}
	policy := proposalTestPolicy()
	policy.OutputSchema = writeCapability.OutputSchema
	policy.Permissions.Scopes = []string{"knowledge.read", "knowledge.write"}
	policy.CallerPermissions.Scopes = []string{"knowledge.read", "knowledge.write"}
	policy.RequiredGoals = []string{"implementation"}
	policy.CapabilityBudgets[writeCapability.ID] = NodeBudget{
		MaxInputTokens: 100, MaxOutputTokens: 100, MaxTotalTokens: 200,
		MaxToolCalls: 1,
	}
	if _, err := compiler.Compile(proposal, policy); err == nil ||
		!strings.Contains(err.Error(), "is not retry-safe") {
		t.Fatalf("Compile error = %v, want retry safety rejection", err)
	}
}

func proposalTestCompiler(
	t *testing.T,
) (
	*ProposalCompiler,
	*agentapi.SchemaRegistry,
	*testAgentResolver,
	*agentapi.CapabilityRegistry,
) {
	t.Helper()
	schemas := testSchemaRegistry(t)
	agents := testAgentDefinitions(t)
	for _, specification := range []struct {
		id     string
		input  agentapi.SchemaRef
		output agentapi.SchemaRef
		tools  []string
	}{
		{
			id:     "proposal.code",
			input:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
			output: agentapi.SchemaRef{ID: "review.report", Version: 1},
			tools:  []string{"search_code"},
		},
		{
			id:     "proposal.docs",
			input:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
			output: agentapi.SchemaRef{ID: "review.report", Version: 1},
			tools:  []string{"search_docs"},
		},
		{
			id:     "proposal.synthesizer",
			input:  agentapi.SchemaRef{ID: "review.report.list", Version: 1},
			output: agentapi.SchemaRef{ID: "review.report", Version: 1},
		},
	} {
		definition, err := agentapi.Prepare(agentapi.Definition{
			ID: specification.id, Version: 1, Purpose: "Proposal test agent.",
			Prompt: agentapi.PromptSpec{
				System: "Execute only the server-owned contract.", Version: "1",
			},
			InputSchema: specification.input, OutputSchema: specification.output,
			Model: agentapi.ModelPolicy{
				Provider: "openai", Model: "test", MaxOutputTokens: 100,
			},
			Tools: agentapi.ToolPolicy{
				VisibleToolIDs:  append([]string(nil), specification.tools...),
				RestrictVisible: true,
			},
			Budget: agentapi.BudgetPolicy{
				Timeout: time.Second, MaxSteps: 1, ContextTokens: 1000,
			},
			Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		agents.definitions[agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		}] = definition
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values := []agentapi.Capability{
		proposalCodeCapability(),
		{
			ID: "knowledge.docs.verify", Version: 1,
			Role:            agentapi.RoleInvestigator,
			Purpose:         "Verify documentation.",
			InputFacets:     []string{"documentation"},
			InputSchema:     agentapi.SchemaRef{ID: "review.subject", Version: 1},
			OutputSchema:    agentapi.SchemaRef{ID: "review.report", Version: 1},
			ToolIDs:         []string{"search_docs"},
			PermissionScope: []string{"knowledge.read"},
			Freshness:       agentapi.FreshnessStable,
			SideEffects:     agentapi.SideEffectNone, RetrySafe: true,
			MaxConcurrency: 1, Enabled: true,
			Agent: agentapi.DefinitionRef{ID: "proposal.docs", Version: 1},
		},
		{
			ID: "evidence.synthesize", Version: 1,
			Role:            agentapi.RoleSynthesizer,
			Purpose:         "Synthesize evidence.",
			InputSchema:     agentapi.SchemaRef{ID: "review.report.list", Version: 1},
			OutputSchema:    agentapi.SchemaRef{ID: "review.report", Version: 1},
			PermissionScope: []string{"knowledge.read"},
			Freshness:       agentapi.FreshnessStable,
			SideEffects:     agentapi.SideEffectNone, RetrySafe: true,
			MaxConcurrency: 1, Enabled: true,
			Agent: agentapi.DefinitionRef{ID: "proposal.synthesizer", Version: 1},
		},
	}
	if err := capabilities.Publish(values); err != nil {
		t.Fatal(err)
	}
	compiler, err := NewProposalCompiler(schemas, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	return compiler, schemas, agents, capabilities
}

func proposalCodeCapability() agentapi.Capability {
	return agentapi.Capability{
		ID: "knowledge.code.inspect", Version: 1,
		Role:            agentapi.RoleInvestigator,
		Purpose:         "Inspect code.",
		InputFacets:     []string{"implementation"},
		InputSchema:     agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema:    agentapi.SchemaRef{ID: "review.report", Version: 1},
		ToolIDs:         []string{"search_code"},
		PermissionScope: []string{"knowledge.read"},
		Freshness:       agentapi.FreshnessStable,
		SideEffects:     agentapi.SideEffectNone, RetrySafe: true,
		MaxConcurrency: 2, Enabled: true,
		Agent: agentapi.DefinitionRef{ID: "proposal.code", Version: 1},
	}
}

func proposalTestGraph() agentapi.TaskGraphProposal {
	return agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{
			{
				ID: "inspect.code", Purpose: "Inspect implementation.",
				RequiredFacets: []string{"implementation"},
				Capability:     "knowledge.code.inspect",
				OutputSchema:   agentapi.SchemaRef{ID: "review.report", Version: 1},
				ParallelGroup:  "investigation",
			},
			{
				ID: "inspect.docs", Purpose: "Verify documentation.",
				RequiredFacets: []string{"documentation"},
				Capability:     "knowledge.docs.verify",
				OutputSchema:   agentapi.SchemaRef{ID: "review.report", Version: 1},
				ParallelGroup:  "investigation", Optional: true, MaxAttempts: 1,
			},
			{
				ID: "synthesize", Purpose: "Synthesize evidence.",
				Capability:   "evidence.synthesize",
				OutputSchema: agentapi.SchemaRef{ID: "review.report", Version: 1},
				MaxAttempts:  1,
			},
		},
		Edges: []agentapi.TaskEdge{
			{From: "inspect.code", To: "synthesize"},
			{From: "inspect.docs", To: "synthesize"},
		},
		Stop: agentapi.StopPolicy{
			MaxTasks: 3, MaxParallelism: 2, MaxAttempts: 2,
			MaxRounds: 1, MaxDepth: 3,
			MaxInputTokens: 500, MaxOutputTokens: 500, MaxTotalTokens: 1000,
			MaxToolCalls: 4, MaxRetries: 2,
		},
	}
}

func proposalTestPolicy() CompilationPolicy {
	return CompilationPolicy{
		WorkflowID: "proposal.workflow", WorkflowVersion: 1,
		Purpose:      "Compile a server-owned proposal workflow.",
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "review.report", Version: 1},
		Permissions:  agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		CallerPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		Budget: Budget{
			MaxNodes: 4, MaxParallelism: 2, MaxRounds: 1, MaxDepth: 3,
			Timeout:         time.Second,
			MaxHandoffBytes: 4096,
			MaxInputTokens:  1000, MaxOutputTokens: 1000, MaxTotalTokens: 2000,
			MaxToolCalls: 10, MaxRetries: 3,
		},
		NodeTimeout: time.Second,
		CapabilityBudgets: map[string]NodeBudget{
			"knowledge.code.inspect": {
				MaxInputTokens: 100, MaxOutputTokens: 100, MaxTotalTokens: 200,
				MaxToolCalls: 2,
			},
			"knowledge.docs.verify": {
				MaxInputTokens: 100, MaxOutputTokens: 100, MaxTotalTokens: 200,
				MaxToolCalls: 2,
			},
			"evidence.synthesize": {
				MaxInputTokens: 200, MaxOutputTokens: 100, MaxTotalTokens: 300,
			},
		},
		MaxTasks: 3, MaxParallelism: 2, MaxAttempts: 2,
		MaxRounds: 1, MaxDepth: 3,
		RequiredGoals:    []string{"implementation"},
		JoinID:           "evidence.join",
		JoinInputSchema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		JoinOutputSchema: agentapi.SchemaRef{ID: "review.report.list", Version: 1},
		FailureMode:      CollectAvailable,
	}
}
