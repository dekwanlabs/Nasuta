package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestDefaultDelegatedInvestigationPinsStableReadOnlyDAG(t *testing.T) {
	schemas, agents := investigationCatalogs(t, 6)
	nodeTimeout := 40 * time.Second
	budgets := investigationBudgetPolicy()
	definition, err := DefaultDelegatedInvestigation(6, nodeTimeout, budgets)
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog(schemas, agents)
	if err := catalog.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	resolved, err := catalog.Resolve(DefinitionRef{ID: DelegatedInvestigationID, Version: 6})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != DelegatedInvestigationID || resolved.Version != 6 ||
		resolved.ContentHash == "" || resolved.Budget.MaxNodes != 5 ||
		resolved.Budget.MaxParallelism != 3 || resolved.Budget.Timeout != 3*nodeTimeout ||
		resolved.FailurePolicy.Mode != CollectAvailable {
		t.Fatalf("workflow = %+v", resolved)
	}
	wantNodes := []string{
		"investigate.code", "investigate.docs", "investigate.runtime", "evidence.join", "synthesize",
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
	if len(resolved.Edges) != 4 {
		t.Fatalf("edges = %v", resolved.Edges)
	}
	for _, edge := range resolved.Edges[:3] {
		if edge.Required {
			t.Fatalf("investigator edge is required: %+v", edge)
		}
	}
	if !resolved.Edges[3].Required {
		t.Fatalf("synthesis edge is optional: %+v", resolved.Edges[3])
	}
	if resolved.Nodes[4].Budget.MaxToolCalls != 0 {
		t.Fatalf("synthesizer tool budget = %d, want zero", resolved.Nodes[4].Budget.MaxToolCalls)
	}
	wantBudget := WorkflowBudget{
		MaxNodes: 5, MaxParallelism: 3, Timeout: 3 * nodeTimeout,
		MaxHandoffBytes: 1 << 20,
		MaxInputTokens:  80,
		MaxOutputTokens: 40,
		MaxTotalTokens:  120,
		MaxToolCalls:    18,
		MaxRetries:      4,
	}
	if resolved.Budget != wantBudget {
		t.Fatalf("workflow budget = %+v, want %+v", resolved.Budget, wantBudget)
	}
}

func TestDefaultDelegatedInvestigationProposalCompilesCapabilityBindings(t *testing.T) {
	const version int64 = 7
	schemas, agents := investigationCatalogs(t, version)
	definitions, err := defaultInvestigationDefinitionsForCapabilities(agents, version)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values, err := catalog.DefaultInvestigationCapabilities(definitions, version)
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
	policy, err := DelegatedInvestigationCompilationPolicy(
		version,
		time.Second,
		investigationBudgetPolicy(),
	)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := compiler.Compile(
		DefaultDelegatedInvestigationProposal(),
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
	if len(definition.Nodes) != 5 || len(definition.Edges) != 4 ||
		definition.Budget.MaxNodes != 5 ||
		definition.Budget.MaxParallelism != 3 ||
		definition.FailurePolicy.Mode != CollectAvailable {
		t.Fatalf("compiled workflow = %+v", definition)
	}
}

func TestDelegatedInvestigationGoalsCompileMinimalStableWorkflow(t *testing.T) {
	const version int64 = 7
	schemas, agents := investigationCatalogs(t, version)
	definitions, err := defaultInvestigationDefinitionsForCapabilities(agents, version)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values, err := catalog.DefaultInvestigationCapabilities(definitions, version)
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
	goals := []DelegatedInvestigationGoal{
		{Facet: "core_flow", Required: true},
		{Facet: "entrypoint", Required: true},
	}
	proposal, err := DelegatedInvestigationProposalForGoals(goals)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := DelegatedInvestigationCompilationPolicyForGoals(
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
	if len(definition.Nodes) != 3 || len(definition.Edges) != 2 {
		t.Fatalf("minimal workflow = %+v", definition)
	}
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	code := nodes["investigate.code"]
	if code.Optional ||
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
	if definition.Budget.MaxNodes != 3 ||
		definition.Budget.MaxParallelism != 1 ||
		definition.Budget.MaxToolCalls != 6 ||
		definition.Budget.MaxRetries != 2 {
		t.Fatalf("minimal workflow budget = %+v", definition.Budget)
	}

	reordered := []DelegatedInvestigationGoal{
		{Facet: "entrypoint", Required: true},
		{Facet: "core_flow", Required: true},
		{Facet: "entrypoint", Required: false},
	}
	reorderedProposal, err := DelegatedInvestigationProposalForGoals(reordered)
	if err != nil {
		t.Fatal(err)
	}
	reorderedPolicy, err := DelegatedInvestigationCompilationPolicyForGoals(
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

func TestDelegatedInvestigationGoalSetsPublishWithoutVersionConflict(t *testing.T) {
	const version int64 = 7
	schemas, agents := investigationCatalogs(t, version)
	definitions, err := defaultInvestigationDefinitionsForCapabilities(agents, version)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values, err := catalog.DefaultInvestigationCapabilities(definitions, version)
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
	compile := func(goals []DelegatedInvestigationGoal) WorkflowDefinition {
		t.Helper()
		proposal, err := DelegatedInvestigationProposalForGoals(goals)
		if err != nil {
			t.Fatal(err)
		}
		policy, err := DelegatedInvestigationCompilationPolicyForGoals(
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
	code := compile([]DelegatedInvestigationGoal{
		{Facet: "core_flow", Required: true},
	})
	runtime := compile([]DelegatedInvestigationGoal{
		{Facet: "runtime_and_operations", Required: true},
	})
	if code.ID == runtime.ID || code.ContentHash == runtime.ContentHash {
		t.Fatalf("distinct goal sets share snapshot identity: %+v / %+v", code, runtime)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]WorkflowDefinition{code, runtime}); err != nil {
		t.Fatal(err)
	}
	if err := workflows.Publish([]WorkflowDefinition{code}); err != nil {
		t.Fatalf("republish stable snapshot: %v", err)
	}
	for _, definition := range []WorkflowDefinition{code, runtime} {
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

func TestDelegatedInvestigationGoalsRejectUnknownFacet(t *testing.T) {
	goals := []DelegatedInvestigationGoal{
		{Facet: "live_database_mutation", Required: true},
	}
	if _, err := DelegatedInvestigationProposalForGoals(goals); err == nil ||
		!strings.Contains(err.Error(), "has no registered capability") {
		t.Fatalf("proposal error = %v", err)
	}
	if _, err := DelegatedInvestigationCompilationPolicyForGoals(
		1,
		time.Second,
		investigationBudgetPolicy(),
		goals,
	); err == nil || !strings.Contains(err.Error(), "has no registered capability") {
		t.Fatalf("policy error = %v", err)
	}
}

func TestDelegatedInvestigationRunsFourIndependentAgentsAndSynthesizesJoin(t *testing.T) {
	const version int64 = 8
	const parentRunID = "qa_parent_1"
	schemas, agents := investigationCatalogs(t, version)
	workflow, err := DefaultDelegatedInvestigation(version, time.Second, investigationBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]WorkflowDefinition{workflow}); err != nil {
		t.Fatal(err)
	}
	runtime := &investigationRuntime{}
	nodes, err := NewAgentNodeExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	service, err := NewService(workflows, persistence, NewOrchestrator(schemas, nodes, nil))
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
		Workflow:    DefinitionRef{ID: DelegatedInvestigationID, Version: version},
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
	if string(result.Output.Payload) != `{"answer":"grounded answer","citations":[],"limitations":["live logs unavailable"]}` {
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
	if !bytes.Equal(requests["synthesizer"].Input, join.Payload) {
		t.Fatalf("synthesizer input = %s, join = %s", requests["synthesizer"].Input, join.Payload)
	}
	var ledger workflowEvidenceLedgerView
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
	persistence.mu.Lock()
	startedRun := persistence.startedRun
	startedNodes := len(persistence.startedNodes)
	succeededNodes := len(persistence.succeededNodes)
	finishedStatus := persistence.finishedStatus
	persistence.mu.Unlock()
	if startedRun.ParentRunID != parentRunID ||
		startedNodes != 5 || succeededNodes != 5 ||
		finishedStatus != RunSucceeded {
		t.Fatalf("persisted lifecycle = run:%+v started:%d succeeded:%d status:%s", startedRun, startedNodes, succeededNodes, finishedStatus)
	}

	workflowNodes := make(map[string]struct{}, 5)
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
				event.WorkflowNodeID != "evidence.join" ||
				event.Output["decision"] != "pass" ||
				event.Output["conflict_count"] != 0 {
				t.Fatalf("verification pass trace = %+v", event)
			}
			verificationPasses++
		}
	}
	if len(workflowNodes) != 5 || len(childRuns) != 4 || verificationPasses != 1 {
		t.Fatalf(
			"trace coverage = workflow_nodes:%v child_runs:%v verification_passes:%d",
			workflowNodes,
			childRuns,
			verificationPasses,
		)
	}
}

func TestDelegatedInvestigationSynthesizesAvailableReport(t *testing.T) {
	const version int64 = 9
	schemas, agents := investigationCatalogs(t, version)
	definition, err := DefaultDelegatedInvestigation(version, time.Second, investigationBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	runtime := &investigationRuntime{failures: map[string]error{
		"investigator.runtime": errors.New("runtime source unavailable"),
		"investigator.docs":    errors.New("documentation source unavailable"),
	}}
	nodes, err := NewAgentNodeExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(workflows, &recordingWorkflowPersistence{}, NewOrchestrator(schemas, nodes, nil))
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: DelegatedInvestigationID, Version: version},
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
	var ledger workflowEvidenceLedgerView
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
	if !bytes.Equal(synthesizer.Input, result.NodeOutputs["evidence.join"].Payload) {
		t.Fatalf(
			"synthesizer input = %s, join = %s",
			synthesizer.Input,
			result.NodeOutputs["evidence.join"].Payload,
		)
	}
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
		inputs, nil, false, 1<<20, schemas,
	)
	if err != nil {
		t.Fatal(err)
	}
	var ledger workflowEvidenceLedgerView
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
		nil, true, 1<<20, schemas,
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
	definition := WorkflowDefinition{
		ID:           "investigation.trace.output",
		Version:      1,
		Purpose:      "Trace evidence delivered by a workflow terminal.",
		InputSchema:  reportSchema,
		OutputSchema: reportSchema,
		Budget: WorkflowBudget{
			MaxNodes: 1, MaxParallelism: 1, Timeout: time.Second,
			MaxHandoffBytes: 1 << 20,
		},
		FailurePolicy: WorkflowFailurePolicy{Mode: FailFast},
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

func TestDelegatedInvestigationRejectsEmptyBundle(t *testing.T) {
	const version int64 = 10
	schemas, agents := investigationCatalogs(t, version)
	definition, err := DefaultDelegatedInvestigation(version, time.Second, investigationBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("source unavailable")
	runtime := &investigationRuntime{failures: map[string]error{
		"investigator.code": failure, "investigator.runtime": failure, "investigator.docs": failure,
	}}
	nodes, err := NewAgentNodeExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(workflows, &recordingWorkflowPersistence{}, NewOrchestrator(schemas, nodes, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: DelegatedInvestigationID, Version: version},
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
	if err == nil || !strings.Contains(err.Error(), `node "evidence.join"`) {
		t.Fatalf("empty bundle error = %v", err)
	}
	if _, ok := runtime.snapshot()["synthesizer"]; ok {
		t.Fatal("synthesizer ran without investigation evidence")
	}
}

func TestDelegatedInvestigationRejectsEvidenceConflictsBeforeSynthesis(t *testing.T) {
	const version int64 = 12
	schemas, agents := investigationCatalogs(t, version)
	definition, err := DefaultDelegatedInvestigation(version, time.Second, investigationBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	workflows := NewCatalog(schemas, agents)
	if err := workflows.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	runtime := &investigationRuntime{
		evidenceHashes: map[string]string{
			"investigator.code":    "v1",
			"investigator.runtime": "v2",
			"investigator.docs":    "v1",
		},
	}
	nodes, err := NewAgentNodeExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		workflows,
		&recordingWorkflowPersistence{},
		NewOrchestrator(schemas, nodes, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	var traces []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		traces = append(traces, event)
	})
	_, err = service.Execute(ctx, ExecuteRequest{
		Workflow: DefinitionRef{ID: DelegatedInvestigationID, Version: version},
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
	if err == nil || !strings.Contains(err.Error(), `node "evidence.join"`) ||
		!strings.Contains(err.Error(), "rejected 1 evidence conflict") {
		t.Fatalf("conflict rejection error = %v", err)
	}
	requests := runtime.snapshot()
	if _, ok := requests["synthesizer"]; ok {
		t.Fatal("synthesizer ran after evidence conflict rejection")
	}
	var rejected, verification, converged *domain.EvaluationTrace
	for index := range traces {
		event := &traces[index]
		switch event.Node {
		case "evidence.rejected":
			rejected = event
		case "verification.completed":
			verification = event
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
		verification.Output["decision"] != "reject" ||
		verification.Output["conflict_count"] != 1 ||
		verification.Output["error"] == "" {
		t.Fatalf("verification trace = %#v", verification)
	}
	if converged == nil ||
		converged.Status != "failed" ||
		converged.Output["outcome"] != "evidence_conflict" ||
		converged.Output["error_code"] != "evidence_conflict" {
		t.Fatalf("workflow convergence trace = %#v", converged)
	}
}

type investigationRuntime struct {
	mu             sync.Mutex
	requests       map[string]agentapi.RunRequest
	failures       map[string]error
	evidenceHashes map[string]string
}

func (runtime *investigationRuntime) Run(
	_ context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	runtime.mu.Lock()
	if runtime.requests == nil {
		runtime.requests = make(map[string]agentapi.RunRequest, 4)
	}
	cloned := request
	cloned.Input = append(json.RawMessage(nil), request.Input...)
	cloned.Permissions.Scopes = append([]string(nil), request.Permissions.Scopes...)
	runtime.requests[request.Agent.ID] = cloned
	runtime.mu.Unlock()

	if err := runtime.failures[request.Agent.ID]; err != nil {
		return agentapi.RunResult{}, err
	}
	if request.Agent.ID == "synthesizer" {
		return agentapi.RunResult{
			Status: agentapi.RunSucceeded,
			Output: json.RawMessage(`{"answer":"grounded answer","citations":[],"limitations":["live logs unavailable"]}`),
		}, nil
	}
	focus := map[string]string{
		"investigator.code": "code", "investigator.docs": "docs", "investigator.runtime": "runtime",
	}[request.Agent.ID]
	if focus == "" {
		return agentapi.RunResult{}, fmt.Errorf("unexpected agent %q", request.Agent.ID)
	}
	payload, err := json.Marshal(map[string]any{
		"focus": focus, "summary": focus + " report", "findings": []any{}, "gaps": []any{},
	})
	if err != nil {
		return agentapi.RunResult{}, err
	}
	result := agentapi.RunResult{Status: agentapi.RunSucceeded, Output: payload}
	if contentHash := runtime.evidenceHashes[request.Agent.ID]; contentHash != "" {
		result.EvidenceUnits = []tool.EvidenceUnit{{
			SourceKind: "service", Target: "checkout", Sections: []string{"flow"},
			ContentHash: contentHash, Coverage: tool.EvidenceCoverage{Complete: true},
		}}
	}
	return result, nil
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
	definitions, err := catalog.DefaultInvestigatorsVersion(settings, version)
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
		"evidence_goals":[{"id":"core_flow","facet":"core_flow","required":true}],
		"context":{}
	}`)
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

func investigationBudgetPolicy() DelegatedInvestigationBudgetPolicy {
	investigator := NodeBudget{
		MaxInputTokens: 10, MaxOutputTokens: 5, MaxTotalTokens: 15,
		MaxToolCalls: 3,
	}
	return DelegatedInvestigationBudgetPolicy{
		Code: investigator, Runtime: investigator, Docs: investigator,
		Synthesizer: NodeBudget{
			MaxInputTokens: 10, MaxOutputTokens: 5, MaxTotalTokens: 15,
		},
	}
}

func defaultInvestigationDefinitionsForCapabilities(
	agents AgentDefinitionResolver,
	version int64,
) ([]agentapi.Definition, error) {
	ids := []string{
		"investigator.code",
		"investigator.runtime",
		"investigator.docs",
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
