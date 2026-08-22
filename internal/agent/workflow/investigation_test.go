package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	agentdefinition "github.com/dekwanlabs/nasuta/internal/agent/definition"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestDefaultFlowPinsStableReadOnlyDAG(t *testing.T) {
	schemas, agents := investigationCatalogs(t, 6)
	nodeTimeout := 40 * time.Second
	budgets := investigationBudgetPolicy()
	definition, err := DefaultFlow(6, nodeTimeout, budgets)
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog(schemas, agents)
	if err := catalog.Publish([]Definition{definition}); err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.Resolve(DefinitionRef{ID: FlowID, Version: 6})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != FlowID || resolved.Version != 6 ||
		resolved.ContentHash == "" || resolved.Budget.MaxNodes != 7 ||
		resolved.Budget.MaxParallelism != 3 || resolved.Budget.Timeout != 3*nodeTimeout ||
		resolved.FailurePolicy.Mode != CollectAvailable {
		t.Fatalf("workflow = %+v", resolved)
	}
	wantNodes := []string{
		"investigate.code", "investigate.docs", "investigate.runtime",
		"evidence.join", "evidence.verify", "evidence.risk", "synthesize",
	}
	order, err := TopologicalOrder(resolved, schemas)
	if err != nil {
		t.Fatal(err)
	}
	gotNodes := make([]string, 0, len(order))
	for _, node := range order {
		gotNodes = append(gotNodes, node.ID)
		if !reflect.DeepEqual(node.Permissions.Scopes, []string{"knowledge.read"}) ||
			node.Timeout != nodeTimeout {
			t.Fatalf("node %q policy = %+v", node.ID, node)
		}
		if strings.HasPrefix(node.ID, "investigate.") && !node.Optional {
			t.Fatalf("investigator node %q is not optional", node.ID)
		}
		if node.Kind == NodeAgent && node.Retry.MaxAttempts != 2 {
			t.Fatalf("agent node %q max attempts = %d, want 2", node.ID, node.Retry.MaxAttempts)
		}
	}
	if !reflect.DeepEqual(gotNodes, wantNodes) {
		t.Fatalf("topological order = %v, want %v", gotNodes, wantNodes)
	}
	if !reflect.DeepEqual(resolved.Permissions.Scopes, []string{"knowledge.read"}) {
		t.Fatalf("workflow permissions = %v", resolved.Permissions.Scopes)
	}
	if resolved.InputSchema.ID != "task.contract" {
		t.Fatalf("workflow input schema = %+v", resolved.InputSchema)
	}
	if len(resolved.Edges) != 6 {
		t.Fatalf("edges = %v", resolved.Edges)
	}
	for _, edge := range resolved.Edges[:3] {
		if edge.Required {
			t.Fatalf("investigator edge is required: %+v", edge)
		}
	}
	for _, edge := range resolved.Edges[3:] {
		if !edge.Required {
			t.Fatalf("verification edge is optional: %+v", edge)
		}
	}
	if resolved.Nodes[6].Budget.MaxToolCalls != 0 {
		t.Fatalf("synthesizer tool budget = %d, want zero", resolved.Nodes[6].Budget.MaxToolCalls)
	}
	wantBudget := Budget{
		MaxNodes: 7, MaxParallelism: 3, MaxRounds: 1, MaxDepth: 5,
		Timeout:           3 * nodeTimeout,
		MaxHandoffBytes:   1 << 20,
		MaxDuplicateRatio: maxDuplicateRatio,
		MaxInputTokens:    80,
		MaxOutputTokens:   40,
		MaxTotalTokens:    120,
		MaxToolCalls:      18,
		MaxRetries:        4,
	}
	if resolved.Budget != wantBudget {
		t.Fatalf("workflow budget = %+v, want %+v", resolved.Budget, wantBudget)
	}
}

func TestDefaultPlanCompilesCapabilityBindings(t *testing.T) {
	const version int64 = 7
	schemas, agents := investigationCatalogs(t, version)
	definitions, err := defaultInvestigationDefinitionsForCapabilities(agents, version)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values, err := catalog.DefaultCapabilities(definitions, version)
	if err != nil {
		t.Fatal(err)
	}
	if err := capabilities.Publish(values); err != nil {
		t.Fatal(err)
	}
	compiler, err := NewProposalCompiler(schemas, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DefaultPolicy(
		version,
		time.Second,
		investigationBudgetPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := compiler.Compile(
		DefaultPlan(),
		policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	wantCapabilities := map[string]string{
		"investigate.code":    "knowledge.code.inspect",
		"investigate.runtime": "knowledge.service.trace",
		"investigate.docs":    "knowledge.docs.verify",
		"synthesize":          "evidence.synthesize",
	}
	for nodeID, capabilityID := range wantCapabilities {
		node := nodes[nodeID]
		if node.Capability != (agentapi.CapabilityRef{
			ID: capabilityID, Version: version,
		}) || node.CapabilityMaxConcurrency != 3 ||
			!node.RestrictVisibleTools || !node.RetrySafe {
			t.Fatalf("node %q capability binding = %+v", nodeID, node)
		}
	}
	join := nodes["evidence.join"]
	if join.Kind != NodeJoin ||
		join.JoinMode != JoinEvidenceView ||
		join.InputSchema != (agentapi.SchemaRef{ID: "investigation.report", Version: 1}) ||
		join.OutputSchema != (agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}) {
		t.Fatalf("join = %+v", join)
	}
	verifier := nodes["evidence.verify"]
	if verifier.Kind != NodeVerifier ||
		verifier.InputSchema != (agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}) ||
		verifier.OutputSchema != (agentapi.SchemaRef{ID: "investigation.verified_bundle", Version: 2}) ||
		verifier.Verifier == nil ||
		verifier.Verifier.RejectEvidenceConflicts {
		t.Fatalf("verifier = %+v", verifier)
	}
	riskGate := nodes["evidence.risk"]
	if riskGate.Kind != NodeGate ||
		riskGate.InputSchema != verifier.OutputSchema ||
		riskGate.OutputSchema != verifier.OutputSchema ||
		riskGate.Gate == nil ||
		riskGate.Gate.ID != EvidenceRiskGateID ||
		!riskGate.Gate.ForwardInput {
		t.Fatalf("risk gate = %+v", riskGate)
	}
	if len(definition.Nodes) != 7 || len(definition.Edges) != 6 ||
		definition.Budget.MaxNodes != 7 ||
		definition.Budget.MaxParallelism != 3 ||
		definition.FailurePolicy.Mode != CollectAvailable {
		t.Fatalf("compiled workflow = %+v", definition)
	}
}

func TestGoalsCompileMinimalStableWorkflow(t *testing.T) {
	const version int64 = 7
	schemas, agents := investigationCatalogs(t, version)
	definitions, err := defaultInvestigationDefinitionsForCapabilities(agents, version)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values, err := catalog.DefaultCapabilities(definitions, version)
	if err != nil {
		t.Fatal(err)
	}
	if err := capabilities.Publish(values); err != nil {
		t.Fatal(err)
	}
	compiler, err := NewProposalCompiler(schemas, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	goals := []Goal{
		{Facet: "core_flow", Required: true},
		{Facet: "entrypoint", Required: true},
	}
	proposal, err := BuildPlan(goals)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := GoalPolicy(
		version,
		time.Second,
		investigationBudgetPolicy(),
		goals,
	)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := compiler.Compile(proposal, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.Nodes) != 5 || len(definition.Edges) != 4 {
		t.Fatalf("minimal workflow = %+v", definition)
	}
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	code := nodes["investigate.code"]
	if !code.Optional ||
		code.Capability != (agentapi.CapabilityRef{
			ID: "knowledge.code.inspect", Version: version,
		}) ||
		code.Task == nil ||
		!reflect.DeepEqual(
			code.Task.RequiredFacets,
			[]string{"core_flow", "entrypoint"},
		) {
		t.Fatalf("code node = %+v", code)
	}
	if _, ok := nodes["investigate.runtime"]; ok {
		t.Fatalf("minimal workflow unexpectedly contains runtime node")
	}
	if _, ok := nodes["investigate.docs"]; ok {
		t.Fatalf("minimal workflow unexpectedly contains docs node")
	}
	join := nodes["evidence.join"]
	if join.Kind != NodeJoin || join.JoinMode != JoinEvidenceView {
		t.Fatalf("single investigator workflow join = %+v", join)
	}
	verifier := nodes["evidence.verify"]
	if verifier.Kind != NodeVerifier ||
		!reflect.DeepEqual(
			verifier.Verifier.RequiredGoals,
			[]string{"core_flow", "entrypoint"},
		) {
		t.Fatalf("single investigator workflow verifier = %+v", verifier)
	}
	riskGate := nodes["evidence.risk"]
	if riskGate.Kind != NodeGate ||
		riskGate.Gate == nil ||
		riskGate.Gate.ID != EvidenceRiskGateID ||
		!riskGate.Gate.ForwardInput {
		t.Fatalf("single investigator workflow risk gate = %+v", riskGate)
	}
	if definition.Budget.MaxNodes != 5 ||
		definition.Budget.MaxParallelism != 1 ||
		definition.Budget.MaxToolCalls != 6 ||
		definition.Budget.MaxRetries != 2 {
		t.Fatalf("minimal workflow budget = %+v", definition.Budget)
	}

	reordered := []Goal{
		{Facet: "entrypoint", Required: true},
		{Facet: "core_flow", Required: true},
		{Facet: "entrypoint", Required: false},
	}
	reorderedProposal, err := BuildPlan(reordered)
	if err != nil {
		t.Fatal(err)
	}
	reorderedPolicy, err := GoalPolicy(
		version,
		time.Second,
		investigationBudgetPolicy(),
		reordered,
	)
	if err != nil {
		t.Fatal(err)
	}
	reorderedDefinition, err := compiler.Compile(
		reorderedProposal,
		reorderedPolicy,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reorderedDefinition.ID != definition.ID ||
		reorderedDefinition.ContentHash != definition.ContentHash {
		t.Fatalf(
			"equivalent goals produced different snapshots: %q/%q and %q/%q",
			definition.ID,
			definition.ContentHash,
			reorderedDefinition.ID,
			reorderedDefinition.ContentHash,
		)
	}
}

func TestGoalSetsPublishWithoutVersionConflict(t *testing.T) {
	const version int64 = 7
	schemas, agents := investigationCatalogs(t, version)
	definitions, err := defaultInvestigationDefinitionsForCapabilities(agents, version)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values, err := catalog.DefaultCapabilities(definitions, version)
	if err != nil {
		t.Fatal(err)
	}
	if err := capabilities.Publish(values); err != nil {
		t.Fatal(err)
	}
	compiler, err := NewProposalCompiler(schemas, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	compile := func(goals []Goal) Definition {
		t.Helper()
		proposal, err := BuildPlan(goals)
		if err != nil {
			t.Fatal(err)
		}
		policy, err := GoalPolicy(
			version,
			time.Second,
			investigationBudgetPolicy(),
			goals,
		)
		if err != nil {
			t.Fatal(err)
		}
		definition, err := compiler.Compile(proposal, policy)
		if err != nil {
			t.Fatal(err)
		}
		return definition
	}
	code := compile([]Goal{
		{Facet: "core_flow", Required: true},
	})
	runtime := compile([]Goal{
		{Facet: "runtime_and_operations", Required: true},
	})
	if code.ID == runtime.ID || code.ContentHash == runtime.ContentHash {
		t.Fatalf("distinct goal sets share snapshot identity: %+v / %+v", code, runtime)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]Definition{code, runtime}); err != nil {
		t.Fatal(err)
	}
	if err := workflows.Publish([]Definition{code}); err != nil {
		t.Fatalf("republish stable snapshot: %v", err)
	}
	for _, definition := range []Definition{code, runtime} {
		resolved, err := workflows.Resolve(DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.ContentHash != definition.ContentHash {
			t.Fatalf("resolved workflow %q hash = %q", definition.ID, resolved.ContentHash)
		}
	}
}

func TestPlanPolicyBudgetsRepeatedInvestigatorCapabilities(t *testing.T) {
	plan := repeatedCodeInvestigationPlan()
	policy, err := PlanPolicy(
		7,
		time.Second,
		investigationBudgetPolicy(),
		[]Goal{{Facet: "core_flow", Required: true}},
		plan,
	)
	if err != nil {
		t.Fatal(err)
	}
	if policy.MaxTasks != 3 || policy.MaxParallelism != 2 ||
		policy.Budget.MaxNodes != 6 || policy.Budget.MaxParallelism != 2 ||
		policy.Budget.MaxToolCalls != 12 {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestPlanPolicyCompilesRepeatedInvestigatorCapabilities(t *testing.T) {
	const version int64 = 7
	schemas, agents := investigationCatalogs(t, version)
	definitions, err := defaultInvestigationDefinitionsForCapabilities(agents, version)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values, err := catalog.DefaultCapabilities(definitions, version)
	if err != nil {
		t.Fatal(err)
	}
	if err := capabilities.Publish(values); err != nil {
		t.Fatal(err)
	}
	compiler, err := NewProposalCompiler(schemas, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	goals := []Goal{{Facet: "core_flow", Required: true}}
	plan := repeatedCodeInvestigationPlan()
	policy, err := PlanPolicy(
		version, time.Second, investigationBudgetPolicy(), goals, plan,
	)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := compiler.Compile(plan, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(definition.Nodes) != 6 || definition.Budget.MaxParallelism != 2 {
		t.Fatalf("definition = %+v", definition)
	}
}

func repeatedCodeInvestigationPlan() agentapi.TaskGraphProposal {
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	return agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{
			{
				ID: "first", Purpose: "Inspect the first behavior.",
				Capability: "knowledge.code.inspect", RequiredFacets: []string{"core_flow"},
				OutputSchema: report, Optional: true, MaxAttempts: 2,
			},
			{
				ID: "second", Purpose: "Inspect the second behavior.",
				Capability: "knowledge.code.inspect", RequiredFacets: []string{"core_flow"},
				OutputSchema: report, Optional: true, MaxAttempts: 2,
			},
			{
				ID: "synthesize", Purpose: "Synthesize evidence.",
				Capability:   "evidence.synthesize",
				OutputSchema: agentapi.SchemaRef{ID: "investigation.answer", Version: 3},
				MaxAttempts:  2,
			},
		},
		Edges: []agentapi.TaskEdge{
			{From: "first", To: "synthesize"},
			{From: "second", To: "synthesize"},
		},
	}
}

func TestGoalsRejectUnknownFacet(t *testing.T) {
	goals := []Goal{
		{Facet: "live_database_mutation", Required: true},
	}
	if _, err := BuildPlan(goals); err == nil ||
		!strings.Contains(err.Error(), "is not registered") {
		t.Fatalf("proposal error = %v", err)
	}
	if _, err := GoalPolicy(
		1,
		time.Second,
		investigationBudgetPolicy(),
		goals,
	); err == nil || !strings.Contains(err.Error(), "is not registered") {
		t.Fatalf("policy error = %v", err)
	}
}

func TestInvestigationFlowRunsFourIndependentAgentsAndSynthesizesJoin(t *testing.T) {
	const version int64 = 8
	const parentRunID = "qa_parent_1"
	schemas, agents := investigationCatalogs(t, version)
	workflow, err := DefaultFlow(version, time.Second, investigationBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]Definition{workflow}); err != nil {
		t.Fatal(err)
	}
	runtime := &investigationRuntime{}
	nodes, err := NewAgentExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	service, err := NewService(workflows, persistence, newInvestigationOrchestrator(schemas, nodes))
	if err != nil {
		t.Fatal(err)
	}
	var traces []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		traces = append(traces, event)
	})
	actor := agentapi.Actor{UserID: 23, TenantID: "tenant-a"}
	result, err := service.Execute(ctx, ExecuteRequest{
		ParentRunID: parentRunID,
		Workflow:    DefinitionRef{ID: FlowID, Version: version},
		Input:       investigationTaskContract(),
		Actor:       actor,
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		Scenario: "delegated.investigation",
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Output.Payload) != `{"answer":"grounded answer","citations":[],"limitations":["live logs unavailable"],"limitations_detail":{"artifact_id":"art_00000000-0000-0000-0000-000000000000","total_count":1,"displayed_count":1,"omitted_count":0,"normalization_version":"limitations-v1"}}` {
		t.Fatalf("output = %s", result.Output.Payload)
	}
	requests := runtime.snapshot()
	if len(requests) != 4 {
		t.Fatalf("agent runs = %d, want 4", len(requests))
	}
	wantNodes := map[string]string{
		"investigator.code":    "investigate.code",
		"investigator.docs":    "investigate.docs",
		"investigator.runtime": "investigate.runtime",
		"synthesizer":          "synthesize",
	}
	runIDs := make(map[string]struct{}, len(requests))
	for agentID, request := range requests {
		if request.Agent.Version != version || request.DefinitionHash == "" ||
			request.Actor != actor || request.Correlation.WorkflowRunID != result.RunID ||
			request.Correlation.ParentRunID != result.RunID ||
			request.Correlation.NodeID != wantNodes[agentID] || request.ToolScope.AllowWrite ||
			!reflect.DeepEqual(request.Permissions.Scopes, []string{"knowledge.read"}) {
			t.Fatalf("request for %q = %+v", agentID, request)
		}
		if _, duplicate := runIDs[request.RunID]; duplicate || request.RunID == "" {
			t.Fatalf("agent run id %q is empty or duplicated", request.RunID)
		}
		runIDs[request.RunID] = struct{}{}
		if strings.HasPrefix(agentID, "investigator.") &&
			!bytes.Equal(request.Input, investigationTaskContract()) {
			t.Fatalf("investigator %q input = %s", agentID, request.Input)
		}
	}
	join := result.NodeOutputs["evidence.join"]
	verified := result.NodeOutputs["evidence.verify"]
	if !bytes.Equal(requests["synthesizer"].Input, verified.Payload) {
		t.Fatalf("synthesizer input = %s, verified = %s", requests["synthesizer"].Input, verified.Payload)
	}
	assertSynthesisObjectiveContext(t, requests["synthesizer"])
	var ledger ledgerView
	if err := json.Unmarshal(join.Payload, &ledger); err != nil {
		t.Fatal(err)
	}
	if got := []string{
		reportFocus(t, ledger.Handoffs[0].Payload),
		reportFocus(t, ledger.Handoffs[1].Payload),
		reportFocus(t, ledger.Handoffs[2].Payload),
	}; !reflect.DeepEqual(got, []string{"code", "runtime", "docs"}) {
		t.Fatalf("join order = %v", got)
	}
	if len(ledger.EvidenceUnits) != 0 || len(ledger.EvidenceConflicts) != 0 ||
		ledger.Completeness != Complete {
		t.Fatalf("ledger = %+v", ledger)
	}
	var evidenceView verifiedEvidenceView
	if err := json.Unmarshal(verified.Payload, &evidenceView); err != nil {
		t.Fatal(err)
	}
	if evidenceView.Completeness != Complete ||
		evidenceView.Verification.Decision != Complete ||
		evidenceView.Verification.StopReason != StopRequiredGoalsCovered ||
		len(evidenceView.SupportedClaims) != 0 ||
		len(evidenceView.UnresolvedGoals) != 0 ||
		result.StopReason != StopRequiredGoalsCovered {
		t.Fatalf("verified evidence = %+v, result stop = %q", evidenceView, result.StopReason)
	}
	persistence.mu.Lock()
	startedRun := persistence.startedRun
	startedNodes := len(persistence.startedNodes)
	succeededNodes := len(persistence.succeededNodes)
	finishedStatus := persistence.finishedStatus
	finishedStopReason := persistence.finishedStopReason
	persistence.mu.Unlock()
	if startedRun.ParentRunID != parentRunID ||
		startedNodes != 7 || succeededNodes != 7 ||
		finishedStatus != RunSucceeded ||
		finishedStopReason != StopRequiredGoalsCovered {
		t.Fatalf(
			"persisted lifecycle = run:%+v started:%d succeeded:%d status:%s stop:%q",
			startedRun,
			startedNodes,
			succeededNodes,
			finishedStatus,
			finishedStopReason,
		)
	}

	workflowNodes := make(map[string]struct{}, 7)
	childRuns := make(map[string]struct{}, 4)
	verificationPasses := 0
	traceID := ""
	for _, event := range traces {
		if event.TraceID == "" || event.Sequence <= 0 {
			t.Fatalf("trace event missing identity = %+v", event)
		}
		if traceID == "" {
			traceID = event.TraceID
		} else if event.TraceID != traceID {
			t.Fatalf("trace IDs are not shared: got %q and %q", traceID, event.TraceID)
		}
		switch event.Node {
		case "workflow_node":
			if event.RunID != result.RunID || event.ParentRunID != parentRunID ||
				event.WorkflowRunID != result.RunID || event.WorkflowNodeID == "" {
				t.Fatalf("workflow node trace = %+v", event)
			}
			workflowNodes[event.WorkflowNodeID] = struct{}{}
		case "multi_agent_child_run":
			if event.RunID == "" || event.RunID != event.AgentRunID ||
				event.ParentRunID != result.RunID ||
				event.WorkflowRunID != result.RunID || event.WorkflowNodeID == "" {
				t.Fatalf("child trace = %+v", event)
			}
			childRuns[event.RunID] = struct{}{}
		case "verification.completed":
			if event.Status != "completed" ||
				event.WorkflowNodeID != "evidence.verify" ||
				event.Output["decision"] != string(Complete) ||
				event.Output["stop_reason"] != StopRequiredGoalsCovered ||
				event.Output["conflict_count"] != 0 {
				t.Fatalf("verification pass trace = %+v", event)
			}
			verificationPasses++
		}
	}
	if len(workflowNodes) != 7 || len(childRuns) != 4 || verificationPasses != 1 {
		t.Fatalf(
			"trace coverage = workflow_nodes:%v child_runs:%v verification_passes:%d",
			workflowNodes,
			childRuns,
			verificationPasses,
		)
	}
}

func TestInvestigationFlowSynthesizesAvailableReport(t *testing.T) {
	const version int64 = 9
	schemas, agents := investigationCatalogs(t, version)
	definition, err := DefaultFlow(version, time.Second, investigationBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]Definition{definition}); err != nil {
		t.Fatal(err)
	}
	runtime := &investigationRuntime{failures: map[string]error{
		"investigator.runtime": errors.New("runtime source unavailable"),
		"investigator.docs":    errors.New("documentation source unavailable"),
	}}
	nodes, err := NewAgentExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		workflows,
		&recordingWorkflowPersistence{},
		newInvestigationOrchestrator(schemas, nodes),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: FlowID, Version: version},
		Input:    investigationTaskContract(),
		Actor:    agentapi.Actor{UserID: 23},
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		Scenario: "delegated.investigation",
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ledger ledgerView
	if err := json.Unmarshal(result.NodeOutputs["evidence.join"].Payload, &ledger); err != nil {
		t.Fatal(err)
	}
	if len(ledger.Handoffs) != 1 || reportFocus(t, ledger.Handoffs[0].Payload) != "code" ||
		ledger.Completeness != Partial {
		t.Fatalf("available reports = %#v", ledger)
	}
	gotUnavailable := make([]string, 0, len(ledger.UnavailableTasks))
	for _, task := range ledger.UnavailableTasks {
		gotUnavailable = append(gotUnavailable, task.ProducerNodeID)
	}
	if !reflect.DeepEqual(gotUnavailable, []string{
		"investigate.runtime",
		"investigate.docs",
	}) {
		t.Fatalf("unavailable tasks = %v", gotUnavailable)
	}
	synthesizer, ok := runtime.snapshot()["synthesizer"]
	if !ok {
		t.Fatal("synthesizer did not run with the available report")
	}
	verified := result.NodeOutputs["evidence.verify"]
	if !bytes.Equal(synthesizer.Input, verified.Payload) {
		t.Fatalf(
			"synthesizer input = %s, verified = %s",
			synthesizer.Input,
			verified.Payload,
		)
	}
	assertSynthesisObjectiveContext(t, synthesizer)
	var evidenceView verifiedEvidenceView
	if err := json.Unmarshal(verified.Payload, &evidenceView); err != nil {
		t.Fatal(err)
	}
	if evidenceView.Completeness != Partial ||
		evidenceView.Verification.StopReason != StopCapabilityUnavailable ||
		len(evidenceView.Limitations) != 2 ||
		result.StopReason != StopCapabilityUnavailable {
		t.Fatalf("verified evidence = %+v, result stop = %q", evidenceView, result.StopReason)
	}
	var synthesis struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(result.Output.Payload, &synthesis); err != nil {
		t.Fatalf("decode synthesis answer: %v", err)
	}
	if synthesis.Answer != "grounded answer" {
		t.Fatalf("synthesis answer = %+v", synthesis)
	}
	for _, internalField := range []string{
		"supported_claims", "partial_claims", "unsupported_claims",
		"verification", "evidence_units", "unavailable_tasks",
	} {
		if strings.Contains(synthesis.Answer, internalField) {
			t.Fatalf("synthesis answer leaked %q: %q", internalField, synthesis.Answer)
		}
	}
}

func TestInvestigationFlowRecoversVerifiedPartialBundleAfterSynthesizerReasoningTruncation(t *testing.T) {
	const version int64 = 15
	var llmCalls int32
	server := reasoningOnlyCompletionServer(t, &llmCalls)
	defer server.Close()

	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	settings := &config.PlatformSettings{
		LLMBaseURL:           server.URL,
		LLMAPIKey:            "test-key",
		LLMProvider:          "openai",
		LLMModel:             "reasoning-only",
		LLMAnswerMaxTokens:   1024,
		LLMContextWindow:     16000,
		AgentAnswerReserve:   config.Duration(100 * time.Millisecond),
		AgentTimeout:         config.Duration(time.Minute),
		AgentMaxSteps:        3,
		AgentMaxToolCalls:    0,
		LLMMaxContinueRounds: 0,
	}
	definitions, err := catalog.DefaultInvestigators(settings, version)
	if err != nil {
		t.Fatalf("prepare agents: %v", err)
	}
	agents := catalog.New(schemas)
	if err := agents.Publish(definitions); err != nil {
		t.Fatalf("publish agents: %v", err)
	}
	synthesizerRuntime, err := agentdefinition.NewRuntime(
		agents,
		schemas,
		tool.NewRegistry(),
		settings,
		nil,
	)
	if err != nil {
		t.Fatalf("create definition runtime: %v", err)
	}

	flowDefinition, err := DefaultFlow(version, time.Second, investigationBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]Definition{flowDefinition}); err != nil {
		t.Fatal(err)
	}
	runtime := &investigationRuntime{
		failures: map[string]error{
			"investigator.runtime": errors.New("runtime source unavailable"),
			"investigator.docs":    errors.New("documentation source unavailable"),
		},
		reports: map[string]json.RawMessage{
			"investigator.code": json.RawMessage(`{
				"focus":"code",
				"summary":"code report",
				"findings":[{
					"claim":"The checkout route reaches the placement handler.",
					"goal_ids":["core_flow"],
					"evidence":[{"kind":"code","reference":"checkout.go","summary":"Route registration and handler"}],
					"confidence":0.9
				}],
				"gaps":[],
				"covered_goals":["core_flow"],
				"unresolved_goals":[]
			}`),
		},
		evidenceUnits: map[string][]tool.EvidenceUnit{
			"investigator.code": {{
				SourceKind: "code", Target: "checkout.go",
				Coverage: tool.EvidenceCoverage{Complete: true},
			}},
		},
		synthesizer: synthesizerRuntime,
	}
	nodes, err := NewAgentExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	service, err := NewService(
		workflows,
		persistence,
		newInvestigationOrchestrator(schemas, nodes),
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: FlowID, Version: version},
		Input:    investigationTaskContract(),
		Actor:    agentapi.Actor{UserID: 23},
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		Scenario: "delegated.investigation",
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err != nil {
		t.Fatalf("workflow unexpectedly failed: %v", err)
	}
	if result.StopReason != StopCapabilityUnavailable ||
		result.Output.Completeness != Partial {
		t.Fatalf("workflow result = stop:%q completeness:%q", result.StopReason, result.Output.Completeness)
	}
	if got := runtime.attemptCount("synthesizer"); got != 1 {
		t.Fatalf("synthesizer workflow attempts = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&llmCalls); got != 3 {
		t.Fatalf("synthesizer internal LLM calls = %d, want initial + conclusion + no-reasoning conclusion", got)
	}

	var answer struct {
		Answer            string                             `json:"answer"`
		Citations         []recoveredCitationTestView        `json:"citations"`
		Limitations       []string                           `json:"limitations"`
		LimitationsDetail recoveredLimitationsDetailTestView `json:"limitations_detail"`
	}
	if err := json.Unmarshal(result.Output.Payload, &answer); err != nil {
		t.Fatalf("decode recovered answer: %v", err)
	}
	if answer.Answer != "- The checkout route reaches the placement handler." ||
		len(answer.Citations) != 1 ||
		answer.Citations[0].Evidence[0].Reference != "checkout.go" ||
		len(answer.Limitations) == 0 ||
		answer.LimitationsDetail.ArtifactID == "" {
		t.Fatalf("recovered answer = %+v", answer)
	}

	verified := result.NodeOutputs["evidence.verify"]
	var evidenceView verifiedEvidenceView
	if err := json.Unmarshal(verified.Payload, &evidenceView); err != nil {
		t.Fatalf("decode verified bundle: %v", err)
	}
	if len(evidenceView.SupportedClaims) != 1 ||
		evidenceView.SupportedClaims[0].Claim != "The checkout route reaches the placement handler." ||
		evidenceView.Completeness != Partial ||
		evidenceView.Verification.StopReason != StopCapabilityUnavailable ||
		evidenceView.LimitationsDetail == nil {
		t.Fatalf("verified bundle = %+v", evidenceView)
	}

	persistence.mu.Lock()
	finishedStatus := persistence.finishedStatus
	finishedError := persistence.finishedError
	finishedStopReason := persistence.finishedStopReason
	finishedOutput := persistence.finishedOutput
	persistence.mu.Unlock()
	if finishedStatus != RunSucceeded || finishedError != "" ||
		finishedStopReason != StopCapabilityUnavailable || finishedOutput == nil {
		t.Fatalf(
			"persisted terminal result = status:%s error:%q stop:%q output:%v",
			finishedStatus,
			finishedError,
			finishedStopReason,
			finishedOutput != nil,
		)
	}
	if len(persistence.succeededNodes) != 5 {
		t.Fatalf("succeeded workflow nodes = %d, want 5 (code, join, verify, risk, synthesize)", len(persistence.succeededNodes))
	}

	synthesizerResult := runtime.result("synthesizer")
	if !synthesizerResult.Evidence.ForcedConclusion {
		t.Fatalf("synthesizer result did not record forced conclusion: %+v", synthesizerResult.Evidence)
	}
}

type recoveredCitationTestView struct {
	Claim    string `json:"claim"`
	Evidence []struct {
		Reference string `json:"reference"`
	} `json:"evidence"`
}

type recoveredLimitationsDetailTestView struct {
	ArtifactID string `json:"artifact_id"`
}

func reasoningOnlyCompletionServer(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
			_ = r.Body.Close()
		}
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		chunk := map[string]any{
			"choices": []any{map[string]any{
				"delta": map[string]string{
					"reasoning_content": "hidden reasoning",
				},
				"finish_reason": "length",
			}},
		}
		data, err := json.Marshal(chunk)
		if err != nil {
			t.Errorf("marshal reasoning-only chunk: %v", err)
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
}

func TestJoinEvidenceViewMergesEvidenceByIdentityAndPreservesEdgeOrder(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 11)
	reportSchema := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	bundleSchema := agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}
	inputs := []Handoff{
		investigationHandoff("run", "investigate.runtime", reportSchema, "runtime", "v1"),
		investigationHandoff("run", "investigate.code", reportSchema, "code", "v1"),
		investigationHandoff("run", "investigate.docs", reportSchema, "docs", "v2"),
	}
	joined, err := joinHandoffs(
		"run", "evidence.join", bundleSchema, JoinEvidenceView,
		inputs, nil, nil, 0.8, false, 1<<20, schemas,
	)
	if err != nil {
		t.Fatal(err)
	}
	var ledger ledgerView
	if err := json.Unmarshal(joined.Payload, &ledger); err != nil {
		t.Fatal(err)
	}
	if got := []string{
		ledger.Handoffs[0].ProducerNodeID,
		ledger.Handoffs[1].ProducerNodeID,
		ledger.Handoffs[2].ProducerNodeID,
	}; !reflect.DeepEqual(got, []string{
		"investigate.runtime", "investigate.code", "investigate.docs",
	}) {
		t.Fatalf("handoff order = %v", got)
	}
	if len(joined.EvidenceUnits) != 1 || len(ledger.EvidenceUnits) != 1 ||
		joined.EvidenceUnits[0].ContentHash != "v1" ||
		ledger.EvidenceUnits[0].ContentHash != "v1" {
		t.Fatalf("merged evidence = handoff:%#v payload:%#v", joined.EvidenceUnits, ledger.EvidenceUnits)
	}
	if len(joined.EvidenceConflicts) != 1 || len(ledger.EvidenceConflicts) != 1 ||
		joined.EvidenceConflicts[0].Current.ContentHash != "v1" ||
		joined.EvidenceConflicts[0].Incoming.ContentHash != "v2" ||
		ledger.EvidenceConflicts[0].Current.ContentHash != "v1" ||
		ledger.EvidenceConflicts[0].Incoming.ContentHash != "v2" {
		t.Fatalf(
			"merged conflicts = handoff:%#v payload:%#v",
			joined.EvidenceConflicts,
			ledger.EvidenceConflicts,
		)
	}
	if joined.Completeness != Complete || ledger.Completeness != Complete {
		t.Fatalf("completeness = handoff:%s payload:%s", joined.Completeness, ledger.Completeness)
	}
	if len(ledger.UnavailableTasks) != 0 {
		t.Fatalf("unavailable tasks = %#v", ledger.UnavailableTasks)
	}
}

func TestJoinEvidenceViewPreservesBaselineWhenInvestigatorsAreUnavailable(
	t *testing.T,
) {
	schemas, _ := investigationCatalogs(t, 11)
	baseline := tool.EvidenceUnit{
		SourceKind: "code", Target: "repo/seed.go",
		Sections: []string{"L1-L20"}, ContentHash: "baseline",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}
	joined, err := joinHandoffs(
		"run",
		"evidence.join",
		agentapi.SchemaRef{ID: "investigation.bundle", Version: 1},
		JoinEvidenceView,
		nil,
		[]unavailableTaskView{{
			ProducerNodeID: "investigate.code",
			StopReason:     StopCapabilityUnavailable,
		}},
		[]tool.EvidenceUnit{baseline},
		0.8,
		false,
		1<<20,
		schemas,
	)
	if err != nil {
		t.Fatal(err)
	}
	var ledger ledgerView
	if err := json.Unmarshal(joined.Payload, &ledger); err != nil {
		t.Fatal(err)
	}
	if len(joined.EvidenceUnits) != 1 ||
		joined.EvidenceUnits[0].Target != baseline.Target ||
		len(ledger.EvidenceUnits) != 1 ||
		len(ledger.BaselineEvidenceIdentities) != 1 ||
		ledger.BaselineEvidenceIdentities[0].Section != "L1-L20" {
		t.Fatalf("baseline ledger = handoff:%#v payload:%#v", joined, ledger)
	}
}

func TestJoinEvidenceViewDeduplicatesBaselineAndInvestigatorEvidence(
	t *testing.T,
) {
	schemas, _ := investigationCatalogs(t, 11)
	reportSchema := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	baseline := tool.EvidenceUnit{
		SourceKind: "service", Target: "checkout",
		Sections: []string{"flow"}, ContentHash: "v1",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}
	input := investigationHandoff(
		"run",
		"investigate.code",
		reportSchema,
		"code",
		"v1",
	)
	joined, err := joinHandoffs(
		"run",
		"evidence.join",
		agentapi.SchemaRef{ID: "investigation.bundle", Version: 1},
		JoinEvidenceView,
		[]Handoff{input},
		nil,
		[]tool.EvidenceUnit{baseline},
		0.8,
		false,
		1<<20,
		schemas,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(joined.EvidenceUnits) != 1 ||
		joined.EvidenceUnits[0].ContentHash != "v1" ||
		len(joined.EvidenceConflicts) != 0 {
		t.Fatalf("deduplicated baseline = %#v", joined)
	}
}

func TestRetainReportedEvidenceDropsUncitedExplorationUnits(t *testing.T) {
	units := make([]tool.EvidenceUnit, 250)
	for index := range units {
		units[index] = tool.EvidenceUnit{
			SourceKind: "code",
			Target:     fmt.Sprintf("service-%03d", index),
			Coverage:   tool.EvidenceCoverage{Complete: true},
		}
	}
	report := reportView{
		Findings: []findingView{{
			Claim:   "The selected services implement the command path.",
			GoalIDs: []string{"core_flow"},
			Evidence: []findingEvidenceView{
				{
					Kind: "code", Reference: "service-007", Summary: "entry point",
					Identity: &agentapi.EvidenceIdentity{SourceKind: "code", Target: "service-007"},
				},
				{
					Kind: "code", Reference: "service-219", Summary: "downstream call",
					Identity: &agentapi.EvidenceIdentity{SourceKind: "code", Target: "service-219"},
				},
			},
			Confidence: 0.9,
		}},
	}
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	retained, omitted, err := retainReportedEvidence([]Handoff{{
		ProducerNodeID: "investigate.code",
		Schema:         agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		Payload:        payload,
		EvidenceUnits:  units,
	}}, units)
	if err != nil {
		t.Fatal(err)
	}
	if omitted != 248 || len(retained) != 2 {
		t.Fatalf("retained=%d omitted=%d", len(retained), omitted)
	}
	if retained[0].Target != "service-007" || retained[1].Target != "service-219" {
		t.Fatalf("retained evidence = %#v", retained)
	}
}

func TestJoinEvidenceViewCompactsLargeBundlesFairly(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 13)
	reportSchema := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	bundleSchema := agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}
	inputs := []Handoff{
		largeInvestigationHandoff(t, "run", "investigate.code", reportSchema, "code", 0, 156),
		largeInvestigationHandoff(t, "run", "investigate.runtime", reportSchema, "runtime", 156, 155),
		largeInvestigationHandoff(t, "run", "investigate.docs", reportSchema, "docs", 311, 155),
	}
	joined, err := joinHandoffs(
		"run", "evidence.join", bundleSchema, JoinEvidenceView,
		inputs, nil, nil, 0.8, false, 16<<20, schemas,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(joined.EvidenceUnits) != 466 {
		t.Fatalf("joined evidence count = %d", len(joined.EvidenceUnits))
	}
	var ledger ledgerView
	if err := json.Unmarshal(joined.Payload, &ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.EvidenceUnitsTotal != 0 || ledger.EvidenceUnitsOmitted != 0 {
		t.Fatalf("compaction metadata = total:%d omitted:%d", ledger.EvidenceUnitsTotal, ledger.EvidenceUnitsOmitted)
	}
	counts := map[string]int{"code": 0, "runtime": 0, "docs": 0}
	for _, unit := range joined.EvidenceUnits {
		prefix := strings.SplitN(unit.Target, "-", 2)[0]
		counts[prefix]++
	}
	if counts["code"] != 156 || counts["runtime"] != 155 || counts["docs"] != 155 {
		t.Fatalf("producer retention changed unexpectedly: %#v", counts)
	}
}

func TestJoinEvidenceViewCompactsToHandoffByteBudget(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 13)
	reportSchema := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	bundleSchema := agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}
	inputs := []Handoff{
		largeInvestigationHandoff(t, "run", "investigate.code", reportSchema, "code", 0, 156),
		largeInvestigationHandoff(t, "run", "investigate.runtime", reportSchema, "runtime", 156, 155),
		largeInvestigationHandoff(t, "run", "investigate.docs", reportSchema, "docs", 311, 155),
	}
	units, conflicts := mergeEvidenceHandoffs(nil, inputs)
	convergence := measureConvergence(inputs, nil, 0.8)
	fullPayload, err := joinedPayload(
		JoinEvidenceView, inputs, nil, units, nil, conflicts,
		0, 0, &convergence, Complete,
	)
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(len(fullPayload) - 1)
	joined, err := joinHandoffs(
		"run", "evidence.join", bundleSchema, JoinEvidenceView,
		inputs, nil, nil, 0.8, false, budget, schemas,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(joined.EvidenceUnits) >= len(units) ||
		len(joined.EvidenceUnits) == 0 ||
		int64(len(joined.Payload)) > budget {
		t.Fatalf(
			"byte compaction retained=%d total=%d payload=%d budget=%d",
			len(joined.EvidenceUnits), len(units), len(joined.Payload), budget,
		)
	}
	var ledger ledgerView
	if err := json.Unmarshal(joined.Payload, &ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.EvidenceUnitsTotal != len(units) ||
		ledger.EvidenceUnitsOmitted != len(units)-len(joined.EvidenceUnits) {
		t.Fatalf("byte compaction metadata = %+v", ledger)
	}
}

func TestJoinEvidenceViewRejectsConflictsWhenConfigured(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 13)
	reportSchema := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	bundleSchema := agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}
	joined, err := joinHandoffs(
		"run", "evidence.join", bundleSchema, JoinEvidenceView,
		[]Handoff{
			investigationHandoff("run", "investigate.code", reportSchema, "code", "v1"),
			investigationHandoff("run", "investigate.runtime", reportSchema, "runtime", "v2"),
		},
		nil, nil, 0.8, true, 1<<20, schemas,
	)
	if err == nil || !strings.Contains(err.Error(), "rejected 1 evidence conflict") {
		t.Fatalf("conflict rejection = handoff:%#v error:%v", joined, err)
	}
	if !errors.Is(err, ErrEvidenceConflict) {
		t.Fatalf("conflict rejection classification = %v", err)
	}
	if len(joined.EvidenceConflicts) != 1 ||
		joined.EvidenceConflicts[0].Current.ContentHash != "v1" ||
		joined.EvidenceConflicts[0].Incoming.ContentHash != "v2" {
		t.Fatalf("rejected handoff = %#v", joined)
	}
}

func TestAggregateHandoffsTracesEvidenceLifecycleWithoutPayloads(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 11)
	reportSchema := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	bundleSchema := agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}
	inputs := []Handoff{
		investigationHandoff("run", "investigate.runtime", reportSchema, "runtime", "v1"),
		investigationHandoff("run", "investigate.code", reportSchema, "code", "v1"),
		investigationHandoff("run", "investigate.docs", reportSchema, "docs", "v2"),
	}
	var traces []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		traces = append(traces, event)
	})
	scope := runtrace.Begin(ctx)
	ctx = runtrace.WithScope(ctx, scope)
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: "run", WorkflowRunID: "run",
	})
	orchestrator := NewOrchestrator(schemas, nil, nil)
	_, err := orchestrator.aggregateHandoffs(
		ctx,
		"run",
		NodeDefinition{
			ID: "evidence.join", Kind: NodeJoin, OutputSchema: bundleSchema,
			JoinMode: JoinEvidenceView,
		},
		inputs,
		nil,
		nil,
		0.8,
		1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	scope.Close()

	byNode := make(map[string]domain.EvaluationTrace, len(traces))
	for _, event := range traces {
		byNode[event.Node] = event
		if encoded, marshalErr := json.Marshal(event); marshalErr != nil {
			t.Fatal(marshalErr)
		} else if bytes.Contains(encoded, []byte(`"summary"`)) ||
			bytes.Contains(encoded, []byte(`"findings"`)) {
			t.Fatalf("evidence trace leaked handoff payload: %s", encoded)
		}
	}
	candidate := byNode["evidence.candidate"]
	if candidate.Output["candidate_count"] != 3 ||
		candidate.WorkflowNodeID != "evidence.join" {
		t.Fatalf("candidate trace = %#v", candidate)
	}
	merged := byNode["evidence.merged"]
	if merged.Output["merged_count"] != 1 {
		t.Fatalf("merged trace = %#v", merged)
	}
	rejected := byNode["evidence.rejected"]
	if rejected.Output["conflict_count"] != 1 {
		t.Fatalf("rejected trace = %#v", rejected)
	}
	delivered := byNode["evidence.delivered"]
	if delivered.Input["phase"] != "aggregation_output" ||
		delivered.Output["evidence_count"] != 1 ||
		delivered.Output["conflict_count"] != 1 {
		t.Fatalf("delivered trace = %#v", delivered)
	}
}

func TestOrchestratorTracesEvidenceDeliveredAtWorkflowOutput(t *testing.T) {
	schemas, _ := investigationCatalogs(t, 12)
	reportSchema := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	definition := Definition{
		ID:           "investigation.trace.output",
		Version:      1,
		Purpose:      "Trace evidence delivered by a workflow terminal.",
		InputSchema:  reportSchema,
		OutputSchema: reportSchema,
		Budget: Budget{
			MaxNodes: 1, MaxParallelism: 1, MaxRounds: 1, MaxDepth: 1,
			Timeout:         time.Second,
			MaxHandoffBytes: 1 << 20,
		},
		FailurePolicy: FailurePolicy{Mode: FailFast},
		Nodes: []NodeDefinition{{
			ID: "investigate.code", Kind: NodeAgent,
			Agent:       agentapi.DefinitionRef{ID: "investigator.code", Version: 12},
			InputSchema: reportSchema, OutputSchema: reportSchema,
			Timeout: time.Second,
		}},
	}
	var traces []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		traces = append(traces, event)
	})
	executor := nodeExecutorFunc(func(
		_ context.Context,
		request NodeRequest,
	) (NodeResult, error) {
		return NodeResult{Handoff: investigationHandoff(
			request.WorkflowRunID,
			request.Node.ID,
			reportSchema,
			"code",
			"v1",
		)}, nil
	})
	result, err := NewOrchestrator(schemas, executor, nil).Run(
		ctx,
		definition,
		RunRequest{
			RunID: "trace-output-run",
			Input: investigationHandoffPayload("code"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output.EvidenceUnits) != 1 {
		t.Fatalf("workflow output evidence = %#v", result.Output.EvidenceUnits)
	}
	var delivered []domain.EvaluationTrace
	for _, event := range traces {
		if event.Node == "evidence.delivered" {
			delivered = append(delivered, event)
		}
	}
	if len(delivered) != 1 ||
		delivered[0].Input["phase"] != "workflow_output" ||
		delivered[0].WorkflowNodeID != "workflow.output" ||
		delivered[0].Output["evidence_count"] != 1 ||
		delivered[0].Output["conflict_count"] != 0 {
		t.Fatalf("workflow output delivered traces = %#v", delivered)
	}
}

func TestInvestigationFlowSynthesizesUnavailableBundle(t *testing.T) {
	const version int64 = 10
	schemas, agents := investigationCatalogs(t, version)
	definition, err := DefaultFlow(version, time.Second, investigationBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]Definition{definition}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("source unavailable")
	runtime := &investigationRuntime{failures: map[string]error{
		"investigator.code": failure, "investigator.runtime": failure, "investigator.docs": failure,
	}}
	nodes, err := NewAgentExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		workflows,
		&recordingWorkflowPersistence{},
		newInvestigationOrchestrator(schemas, nodes),
	)
	if err != nil {
		t.Fatal(err)
	}
	baseline := tool.EvidenceUnit{
		SourceKind: "code", Target: "repo/seed.go",
		Sections: []string{"L1-L20"}, ContentHash: "baseline",
		Coverage: tool.EvidenceCoverage{Complete: true},
		Facets:   []string{"core_flow"},
	}
	result, err := service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: FlowID, Version: version},
		Input:    investigationTaskContract(),
		SeedEvidence: []tool.EvidenceUnit{
			baseline,
		},
		Actor: agentapi.Actor{UserID: 23},
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		Scenario: "delegated.investigation",
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var ledger ledgerView
	if err := json.Unmarshal(result.NodeOutputs["evidence.join"].Payload, &ledger); err != nil {
		t.Fatal(err)
	}
	if len(ledger.Handoffs) != 0 ||
		len(ledger.UnavailableTasks) != 3 ||
		len(ledger.EvidenceUnits) != 1 ||
		ledger.EvidenceUnits[0].Target != baseline.Target ||
		len(ledger.BaselineEvidenceIdentities) != 1 ||
		ledger.Completeness != Unavailable {
		t.Fatalf("unavailable ledger = %#v", ledger)
	}
	synthesizer, ok := runtime.snapshot()["synthesizer"]
	if !ok {
		t.Fatal("synthesizer did not run with unavailable investigation evidence")
	}
	verified := result.NodeOutputs["evidence.verify"]
	if !bytes.Equal(synthesizer.Input, verified.Payload) ||
		result.Output.Completeness != Unavailable {
		t.Fatalf(
			"synthesis = input:%s output completeness:%s",
			synthesizer.Input,
			result.Output.Completeness,
		)
	}
	assertSynthesisObjectiveContext(t, synthesizer)
	var evidenceView verifiedEvidenceView
	if err := json.Unmarshal(verified.Payload, &evidenceView); err != nil {
		t.Fatal(err)
	}
	if evidenceView.Verification.StopReason != StopCapabilityUnavailable ||
		len(evidenceView.EvidenceUnits) != 1 ||
		evidenceView.EvidenceUnits[0].Target != baseline.Target ||
		result.StopReason != StopCapabilityUnavailable {
		t.Fatalf("verified evidence = %+v, result stop = %q", evidenceView, result.StopReason)
	}
}

func TestInvestigationFlowRequiresClarificationForEvidenceConflicts(t *testing.T) {
	const version int64 = 12
	schemas, agents := investigationCatalogs(t, version)
	definition, err := DefaultFlow(version, time.Second, investigationBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]Definition{definition}); err != nil {
		t.Fatal(err)
	}
	runtime := &investigationRuntime{
		evidenceHashes: map[string]string{
			"investigator.code":    "v1",
			"investigator.runtime": "v2",
			"investigator.docs":    "v1",
		},
	}
	nodes, err := NewAgentExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	service, err := NewService(
		workflows,
		persistence,
		newInvestigationOrchestrator(schemas, nodes),
	)
	if err != nil {
		t.Fatal(err)
	}
	var traces []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		traces = append(traces, event)
	})
	result, err := service.Execute(ctx, ExecuteRequest{
		Workflow: DefinitionRef{ID: FlowID, Version: version},
		Input:    investigationTaskContract(),
		Actor:    agentapi.Actor{UserID: 23},
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		Scenario: "delegated.investigation",
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err == nil || errorStopReason(err) != StopNeedsClarification ||
		result.StopReason != StopNeedsClarification {
		t.Fatalf("conflict clarification = result:%+v error:%v", result, err)
	}
	requests := runtime.snapshot()
	if _, ok := requests["synthesizer"]; ok {
		t.Fatal("synthesizer ran after evidence conflict rejection")
	}
	var rejected, verification, riskGate, converged *domain.EvaluationTrace
	for index := range traces {
		event := &traces[index]
		switch event.Node {
		case "evidence.rejected":
			rejected = event
		case "verification.completed":
			verification = event
		case "workflow_node":
			if event.WorkflowNodeID == "evidence.risk" {
				riskGate = event
			}
		case "workflow.converged":
			converged = event
		}
		encoded, marshalErr := json.Marshal(*event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if bytes.Contains(encoded, []byte(`"summary"`)) ||
			bytes.Contains(encoded, []byte(`"findings"`)) {
			t.Fatalf("conflict trace leaked handoff payload: %s", encoded)
		}
	}
	if rejected == nil ||
		rejected.Output["conflict_count"] != 1 ||
		rejected.Output["omitted_count"] != 0 {
		t.Fatalf("evidence rejection trace = %#v", rejected)
	}
	conflicts, ok := rejected.Output["conflicts"].([]map[string]any)
	if !ok || len(conflicts) != 1 {
		t.Fatalf("evidence rejection conflicts = %#v", rejected.Output["conflicts"])
	}
	conflict := conflicts[0]
	if conflict["current_hash"] != "v1" ||
		conflict["incoming_hash"] != "v2" {
		t.Fatalf("evidence rejection conflict = %#v", conflicts[0])
	}
	if verification == nil ||
		verification.Status != "completed" ||
		verification.Output["decision"] != string(Complete) ||
		verification.Output["conflict_count"] != 1 ||
		verification.Output["stop_reason"] != StopRequiredGoalsCovered {
		t.Fatalf("verification trace = %#v", verification)
	}
	if riskGate == nil ||
		riskGate.Output["gate_decision"] !=
			string(StopNeedsClarification) ||
		riskGate.Output["gate_finding_count"] != 0 ||
		!reflect.DeepEqual(
			riskGate.Output["gate_reason_codes"],
			[]string{riskReasonEvidenceConflict},
		) {
		t.Fatalf("risk gate trace = %#v", riskGate)
	}
	if converged == nil ||
		converged.Status != "failed" ||
		converged.Output["outcome"] != string(StopNeedsClarification) ||
		converged.Output["error_code"] != "needs_clarification" ||
		converged.Output["stop_reason"] != StopNeedsClarification {
		t.Fatalf("workflow convergence trace = %#v", converged)
	}
	persistence.mu.Lock()
	finishedStatus := persistence.finishedStatus
	finishedError := persistence.finishedError
	finishedStopReason := persistence.finishedStopReason
	decision := persistence.state.Gates["evidence.risk"]
	persistence.mu.Unlock()
	if finishedStatus != RunFailed ||
		finishedError != "needs_clarification" ||
		finishedStopReason != StopNeedsClarification ||
		decision.Decision != string(StopNeedsClarification) ||
		!reflect.DeepEqual(decision.ReasonCodes, []string{riskReasonEvidenceConflict}) {
		t.Fatalf(
			"persisted conflict terminal = %s error=%q stop=%q gate=%+v",
			finishedStatus,
			finishedError,
			finishedStopReason,
			decision,
		)
	}
}

func newInvestigationOrchestrator(
	schemas *agentapi.SchemaRegistry,
	nodes NodeExecutor,
) *Orchestrator {
	return NewOrchestrator(schemas, nodes, map[string]GateEvaluator{
		EvidenceRiskGateID: RiskGateEvaluator{},
	})
}

type investigationRuntime struct {
	mu             sync.Mutex
	requests       map[string]agentapi.RunRequest
	attempts       map[string]int
	failures       map[string]error
	evidenceHashes map[string]string
	evidenceUnits  map[string][]tool.EvidenceUnit
	reports        map[string]json.RawMessage
	results        map[string]agentapi.RunResult
	synthesizer    agentapi.Runtime
}

func (runtime *investigationRuntime) Run(
	_ context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	runtime.mu.Lock()
	if runtime.requests == nil {
		runtime.requests = make(map[string]agentapi.RunRequest, 4)
	}
	if runtime.attempts == nil {
		runtime.attempts = make(map[string]int, 4)
	}
	if runtime.results == nil {
		runtime.results = make(map[string]agentapi.RunResult, 4)
	}
	runtime.attempts[request.Agent.ID]++
	cloned := request
	cloned.Input = append(json.RawMessage(nil), request.Input...)
	cloned.Permissions.Scopes = append([]string(nil), request.Permissions.Scopes...)
	runtime.requests[request.Agent.ID] = cloned
	runtime.mu.Unlock()

	if err := runtime.failures[request.Agent.ID]; err != nil {
		return runtime.recordResult(request.Agent.ID, agentapi.RunResult{}, err)
	}
	if request.Agent.ID == "synthesizer" {
		if runtime.synthesizer != nil {
			result, err := runtime.synthesizer.Run(context.Background(), request)
			return runtime.recordResult(request.Agent.ID, result, err)
		}
		return runtime.recordResult(request.Agent.ID, agentapi.RunResult{
			Status: agentapi.RunSucceeded,
			Output: json.RawMessage(`{"answer":"grounded answer","citations":[],"limitations":["live logs unavailable"],"limitations_detail":{"artifact_id":"art_00000000-0000-0000-0000-000000000000","total_count":1,"displayed_count":1,"omitted_count":0,"normalization_version":"limitations-v1"}}`),
		}, nil)
	}
	focus := map[string]string{
		"investigator.code": "code", "investigator.docs": "docs", "investigator.runtime": "runtime",
	}[request.Agent.ID]
	if focus == "" {
		return runtime.recordResult(request.Agent.ID, agentapi.RunResult{}, fmt.Errorf("unexpected agent %q", request.Agent.ID))
	}
	payload := append(json.RawMessage(nil), runtime.reports[request.Agent.ID]...)
	if len(payload) == 0 {
		var err error
		payload, err = json.Marshal(map[string]any{
			"focus": focus, "summary": focus + " report", "findings": []any{}, "gaps": []any{},
			"covered_goals": []any{}, "unresolved_goals": []any{},
		})
		if err != nil {
			return runtime.recordResult(request.Agent.ID, agentapi.RunResult{}, err)
		}
	}
	result := agentapi.RunResult{Status: agentapi.RunSucceeded, Output: payload}
	result.EvidenceUnits = cloneEvidenceUnits(runtime.evidenceUnits[request.Agent.ID])
	if contentHash := runtime.evidenceHashes[request.Agent.ID]; contentHash != "" {
		result.EvidenceUnits = []tool.EvidenceUnit{{
			SourceKind: "service", Target: "checkout", Sections: []string{"flow"},
			ContentHash: contentHash, Coverage: tool.EvidenceCoverage{Complete: true},
		}}
	}
	return runtime.recordResult(request.Agent.ID, result, nil)
}

func (runtime *investigationRuntime) recordResult(
	agentID string,
	result agentapi.RunResult,
	err error,
) (agentapi.RunResult, error) {
	runtime.mu.Lock()
	runtime.results[agentID] = result
	runtime.mu.Unlock()
	return result, err
}

func (runtime *investigationRuntime) snapshot() map[string]agentapi.RunRequest {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	out := make(map[string]agentapi.RunRequest, len(runtime.requests))
	for id, request := range runtime.requests {
		out[id] = request
	}
	return out
}

func (runtime *investigationRuntime) result(agentID string) agentapi.RunResult {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.results[agentID]
}

func (runtime *investigationRuntime) attemptCount(agentID string) int {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.attempts[agentID]
}

func investigationCatalogs(t *testing.T, version int64) (*agentapi.SchemaRegistry, *catalog.Catalog) {
	t.Helper()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	settings := &config.PlatformSettings{
		LLMProvider: "openai", LLMModel: "test-model", LLMAnswerMaxTokens: 1024,
		LLMContextWindow: 16000, AgentTimeout: config.Duration(time.Minute), AgentMaxSteps: 3,
	}
	definitions, err := catalog.DefaultInvestigators(settings, version)
	if err != nil {
		t.Fatalf("prepare agents: %v", err)
	}
	agents := catalog.New(schemas)
	if err := agents.Publish(definitions); err != nil {
		t.Fatalf("publish agents: %v", err)
	}
	return schemas, agents
}

func investigationTaskContract() json.RawMessage {
	return json.RawMessage(`{
		"task_id":"qa_parent_1",
		"question":"Why is checkout failing?",
		"objective":"Trace the checkout failure",
		"entities":[{"id":"Checkout.Place"}],
		"investigation_goals":[
			{
				"id":"failure_path",
				"objective":"Explain the failure path",
				"independently_useful":true,
				"depends_on":[]
			},
			{
				"id":"runtime_impact",
				"objective":"Assess the runtime impact",
				"independently_useful":true,
				"depends_on":[]
			}
		],
		"evidence_goals":[{
			"id":"core_flow",
			"facet":"core_flow",
			"required":true,
			"sources":["internal"],
			"freshness":"stable",
			"minimum_coverage":1
		}],
		"context":{
			"seed_material":[{
				"source":"context.seed_material",
				"title":"Seed material",
				"content":"must-not-reach-synthesizer",
				"complete":true,
				"content_hash":"seed-v1"
			}]
		}
	}`)
}

func assertSynthesisObjectiveContext(
	t *testing.T,
	request agentapi.RunRequest,
) {
	t.Helper()
	var objectiveBlock *agentapi.ContextBlock
	for index := range request.Context {
		if request.Context[index].Source == "workflow.synthesis_objective" {
			objectiveBlock = &request.Context[index]
			break
		}
	}
	if objectiveBlock == nil || objectiveBlock.ContentHash == "" || !objectiveBlock.Complete {
		t.Fatalf("synthesis objective context = %+v", request.Context)
	}
	var objective synthesisObjectiveView
	if err := json.Unmarshal([]byte(objectiveBlock.Content), &objective); err != nil {
		t.Fatalf("decode synthesis objective: %v", err)
	}
	if objective.Objective != "Trace the checkout failure" ||
		len(objective.InvestigationGoals) != 2 ||
		objective.InvestigationGoals[0].ID != "failure_path" ||
		objective.InvestigationGoals[0].Objective != "Explain the failure path" ||
		objective.InvestigationGoals[1].ID != "runtime_impact" ||
		objective.InvestigationGoals[1].Objective != "Assess the runtime impact" {
		t.Fatalf("synthesis objective = %+v", objective)
	}
	for _, forbidden := range []string{
		"context.seed_material",
		"must-not-reach-synthesizer",
		"evidence_goals",
	} {
		if strings.Contains(objectiveBlock.Content, forbidden) {
			t.Fatalf(
				"synthesis objective leaked %q: %s",
				forbidden,
				objectiveBlock.Content,
			)
		}
	}
}

func largeInvestigationHandoff(
	t *testing.T,
	runID, nodeID string,
	schema agentapi.SchemaRef,
	prefix string,
	start, count int,
) Handoff {
	t.Helper()
	units := make([]tool.EvidenceUnit, 0, count)
	findings := make([]findingView, 0, (count+19)/20)
	for offset := 0; offset < count; offset += 20 {
		end := min(offset+20, count)
		items := make([]findingEvidenceView, 0, end-offset)
		for index := offset; index < end; index++ {
			target := fmt.Sprintf("%s-%03d", prefix, start+index)
			units = append(units, tool.EvidenceUnit{
				SourceKind: "code", Target: target,
				Coverage: tool.EvidenceCoverage{Complete: true},
			})
			items = append(items, findingEvidenceView{
				Kind: "code", Reference: target, Summary: "canonical evidence",
				Identity: &agentapi.EvidenceIdentity{SourceKind: "code", Target: target},
			})
		}
		findings = append(findings, findingView{
			Claim:   fmt.Sprintf("%s finding %d", prefix, offset/20),
			GoalIDs: []string{"core_flow"}, Evidence: items, Confidence: 0.9,
		})
	}
	payload, err := json.Marshal(reportView{
		Findings: findings, Gaps: []string{},
		CoveredGoals: []string{"core_flow"}, UnresolvedGoals: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	// reportView omits focus and summary, which are required by the public schema.
	var report map[string]any
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatal(err)
	}
	report["focus"] = "code"
	report["summary"] = prefix + " report"
	payload, err = json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return Handoff{
		WorkflowRunID: runID, ProducerNodeID: nodeID, Schema: schema,
		Payload: payload, EvidenceUnits: units, Completeness: Complete,
	}
}

func investigationHandoff(
	runID, nodeID string,
	schema agentapi.SchemaRef,
	focus, contentHash string,
) Handoff {
	return Handoff{
		WorkflowRunID: runID, ProducerNodeID: nodeID, Schema: schema,
		Payload: investigationHandoffPayload(focus),
		EvidenceUnits: []tool.EvidenceUnit{{
			SourceKind: "service", Target: "checkout", Sections: []string{"flow"},
			ContentHash: contentHash, Coverage: tool.EvidenceCoverage{Complete: true},
		}},
		Completeness: Complete,
	}
}

func investigationHandoffPayload(focus string) json.RawMessage {
	payload, err := json.Marshal(map[string]any{
		"focus": focus, "summary": focus + " report", "findings": []any{}, "gaps": []any{},
		"covered_goals": []any{}, "unresolved_goals": []any{},
	})
	if err != nil {
		panic(err)
	}
	return payload
}

type nodeExecutorFunc func(context.Context, NodeRequest) (NodeResult, error)

func (execute nodeExecutorFunc) Execute(
	ctx context.Context,
	request NodeRequest,
) (NodeResult, error) {
	return execute(ctx, request)
}

func reportFocus(t *testing.T, payload json.RawMessage) string {
	t.Helper()
	var report struct {
		Focus string `json:"focus"`
	}
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatal(err)
	}
	return report.Focus
}

func investigationBudgetPolicy() Budgets {
	investigator := NodeBudget{
		MaxInputTokens: 10, MaxOutputTokens: 5, MaxTotalTokens: 15,
		MaxToolCalls: 3,
	}
	return Budgets{
		Code: investigator, Runtime: investigator, Docs: investigator, Web: investigator,
		Memory: NodeBudget{
			MaxInputTokens: 10, MaxOutputTokens: 5, MaxTotalTokens: 15,
		},
		Synthesizer: NodeBudget{
			MaxInputTokens: 10, MaxOutputTokens: 5, MaxTotalTokens: 15,
		},
	}
}

func defaultInvestigationDefinitionsForCapabilities(
	agents AgentResolver,
	version int64,
) ([]agentapi.Definition, error) {
	ids := []string{
		"investigator.code",
		"investigator.runtime",
		"investigator.docs",
		"investigator.web",
		"investigator.memory",
		"delegation.verifier",
		"synthesizer",
	}
	definitions := make([]agentapi.Definition, 0, len(ids))
	for _, id := range ids {
		definition, err := agents.Resolve(agentapi.DefinitionRef{
			ID: id, Version: version,
		})
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func TestBuildPlanTreatsEvidenceSourcesAsAlternatives(t *testing.T) {
	proposal, err := BuildPlan([]Goal{{
		Facet: "core_flow", Required: true,
		Sources: []agentapi.EvidenceSource{
			agentapi.EvidenceSourceInternal,
			agentapi.EvidenceSourceWeb,
			agentapi.EvidenceSourceRuntime,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Tasks) != 2 || proposal.Tasks[0].Capability != "knowledge.code.inspect" {
		t.Fatalf("proposal = %+v, want one internal investigator plus synthesizer", proposal)
	}
}

func TestBuildPlanRejectsSourceThatCannotCoverFacet(t *testing.T) {
	_, err := BuildPlan([]Goal{{
		Facet: "entrypoint", Required: true,
		Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceWeb},
	}})
	if err == nil || !strings.Contains(err.Error(), "no investigation capability") {
		t.Fatalf("proposal error = %v", err)
	}
}

func TestBuildPlanCapsComplementaryEvidenceGoals(t *testing.T) {
	goals := []Goal{
		{Facet: "entrypoint", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		{Facet: "external_dependency", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		{Facet: "business_domain", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}},
		{Facet: "runtime_and_operations", Required: true, Sources: []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime}},
	}
	proposal, err := BuildPlan(goals)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Tasks) != maxGoalInvestigationTasks+1 {
		t.Fatalf("tasks = %+v, want bounded investigators plus synthesizer", proposal.Tasks)
	}
	policy, err := GoalPolicy(1, time.Second, investigationBudgetPolicy(), goals)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.RequiredGoals) != len(goals) {
		t.Fatalf("required goals = %v, want all requested goals retained for completeness", policy.RequiredGoals)
	}
}
