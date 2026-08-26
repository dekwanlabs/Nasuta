package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestAgentNodeExecutorPinsRunAndIntersectsDefinitionPermissions(t *testing.T) {
	runtime := &capturingAgentRuntime{result: agentapi.RunResult{
		Status:     agentapi.RunSucceeded,
		Output:     json.RawMessage(`{"node":"review.a"}`),
		References: []agentapi.Reference{{Type: "code", Target: "repo/file.go"}},
		Usage: agentapi.Usage{
			InputTokens: 31, OutputTokens: 7, ReasoningTokens: 2,
			TotalTokens: 38, CostMicros: 9,
		},
		Evidence: agentapi.EvidenceSummary{ToolCallCount: 3},
	}}
	executor, err := NewAgentExecutor(
		testSchemaRegistry(t),
		testAgentDefinitions(t),
		runtime,
	)
	if err != nil {
		t.Fatal(err)
	}
	input, err := PrepareHandoff(Handoff{
		WorkflowRunID: "workflow_run_1", ProducerNodeID: "workflow.input",
		Schema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		Payload: json.RawMessage(`{"subject":"x"}`), Completeness: Complete,
	}, 4096, testSchemaRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	node := singleNodeWorkflow().Nodes[0]
	node.Budget.MaxToolCalls = 4
	node.Task = &TaskDirective{
		Purpose:              "Review the implementation evidence.",
		InvestigationGoalIDs: []string{"implementation_review"},
		RequiredFacets:       []string{"implementation"},
		InputRefs: []agentapi.EvidenceRef{{
			SourceKind: "code", Target: "repo/file.go", Section: "implementation",
		}},
		ParallelGroup: "review",
	}
	result, err := executor.Execute(t.Context(), NodeRequest{
		WorkflowRunID: "workflow_run_1", ParentRunID: "qa_parent_1",
		Node: node, Inputs: []Handoff{input},
		Actor: agentapi.Actor{UserID: 7, TenantID: "tenant-a"},
		EffectivePermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read", "knowledge.write"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AgentRunID == "" || result.Handoff.ProducerRunID != result.AgentRunID {
		t.Fatalf("agent run linkage = %+v", result)
	}
	if runtime.request.RunID != result.AgentRunID ||
		runtime.request.DefinitionHash == "" ||
		runtime.request.Correlation.WorkflowRunID != "workflow_run_1" ||
		runtime.request.Correlation.NodeID != "review.a" ||
		runtime.request.Actor.UserID != 7 {
		t.Fatalf("runtime request = %+v", runtime.request)
	}
	if runtime.projectedChildRunID != result.AgentRunID ||
		runtime.projectedParentRunID != "qa_parent_1" ||
		runtime.projectedWorkflowRunID != "workflow_run_1" ||
		runtime.projectedNodeID != "review.a" ||
		!runtime.projectionStopped {
		t.Fatalf("tool projection = %+v", runtime)
	}
	if len(runtime.request.Permissions.Scopes) != 1 ||
		runtime.request.Permissions.Scopes[0] != "knowledge.read" ||
		runtime.request.ToolScope.AllowWrite {
		t.Fatalf("effective runtime permissions = %+v", runtime.request)
	}
	if len(runtime.request.Context) != 2 ||
		runtime.request.Context[0].Source != "workflow.handoff" ||
		runtime.request.Context[1].Source != "workflow.task" ||
		runtime.request.Context[1].ContentHash == "" {
		t.Fatalf("runtime context = %+v", runtime.request.Context)
	}
	if len(runtime.request.Context[0].Evidence) != 0 ||
		len(runtime.request.Context[0].EvidenceConflicts) != 0 ||
		len(runtime.request.Context[0].References) != 0 {
		t.Fatalf(
			"handoff ledger leaked into model context = %+v",
			runtime.request.Context[0],
		)
	}
	var directive TaskDirective
	if err := json.Unmarshal([]byte(runtime.request.Context[1].Content), &directive); err != nil {
		t.Fatalf("decode task directive: %v", err)
	}
	if directive.Purpose != node.Task.Purpose ||
		len(directive.InvestigationGoalIDs) != 1 ||
		directive.InvestigationGoalIDs[0] != "implementation_review" ||
		len(directive.RequiredFacets) != 1 ||
		directive.RequiredFacets[0] != "implementation" ||
		len(directive.InputRefs) != 1 ||
		directive.InputRefs[0].Target != "repo/file.go" ||
		directive.ParallelGroup != "review" {
		t.Fatalf("task directive = %+v", directive)
	}
	if len(result.Handoff.References) != 1 || result.Handoff.References[0].Target != "repo/file.go" {
		t.Fatalf("handoff references = %+v", result.Handoff.References)
	}
	if runtime.request.Policy.MaxToolCalls != 4 ||
		result.Usage != (Usage{
			InputTokens: 31, OutputTokens: 7, ReasoningTokens: 2,
			TotalTokens: 38, ToolCalls: 3, CostMicros: 9,
		}) {
		t.Fatalf("runtime policy=%+v node usage=%+v", runtime.request.Policy, result.Usage)
	}
}

func TestAgentNodeExecutorRequiresExplicitJoinForMultipleInputs(t *testing.T) {
	runtime := &capturingAgentRuntime{}
	executor, err := NewAgentExecutor(testSchemaRegistry(t), testAgentDefinitions(t), runtime)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(t.Context(), NodeRequest{
		WorkflowRunID: "workflow_run_1", Node: singleNodeWorkflow().Nodes[0],
		Inputs: []Handoff{{}, {}},
	})
	if err == nil || !strings.Contains(err.Error(), "exactly one handoff") {
		t.Fatalf("Execute error = %v", err)
	}
	if runtime.called {
		t.Fatal("runtime was called for an ambiguous multi-input node")
	}
}

func TestAgentNodeExecutorRetainsAgentRunIDOnRuntimeFailure(t *testing.T) {
	runtime := &capturingAgentRuntime{
		result: agentapi.RunResult{
			Usage: agentapi.Usage{
				InputTokens: 17, OutputTokens: 4, ReasoningTokens: 1,
				TotalTokens: 21, CostMicros: 6,
			},
			Evidence: agentapi.EvidenceSummary{ToolCallCount: 2},
		},
		err: context.DeadlineExceeded,
	}
	executor, err := NewAgentExecutor(testSchemaRegistry(t), testAgentDefinitions(t), runtime)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(t.Context(), NodeRequest{
		WorkflowRunID: "workflow_run_1", Node: singleNodeWorkflow().Nodes[0],
		Inputs:               []Handoff{{Payload: json.RawMessage(`{"subject":"x"}`)}},
		EffectivePermissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err == nil || result.AgentRunID == "" ||
		result.Usage != (Usage{
			InputTokens: 17, OutputTokens: 4, ReasoningTokens: 1,
			TotalTokens: 21, ToolCalls: 2, CostMicros: 6,
		}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAgentNodeExecutorMapsRetryableRuntimeFailure(t *testing.T) {
	runtime := &capturingAgentRuntime{result: agentapi.RunResult{
		Status: agentapi.RunFailed,
		Error: &agentapi.RunError{
			Code: "provider_unavailable", Message: "provider is temporarily unavailable",
			Retryable: true,
		},
	}}
	executor, err := NewAgentExecutor(testSchemaRegistry(t), testAgentDefinitions(t), runtime)
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(t.Context(), NodeRequest{
		WorkflowRunID: "workflow_run_1",
		Node:          singleNodeWorkflow().Nodes[0],
		Inputs: []Handoff{{
			Payload: json.RawMessage(`{"subject":"x"}`),
		}},
		EffectivePermissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err == nil {
		t.Fatal("Execute succeeded after runtime failure")
	}
	var classified interface{ Retryable() bool }
	if !errors.As(err, &classified) || !classified.Retryable() {
		t.Fatalf("Execute error = %v, want explicit retryable classification", err)
	}
}

func TestAgentNodeExecutorTreatsEvidenceBackedEmptyReportAsPartial(t *testing.T) {
	const version int64 = 41
	schemas, agents := investigationCatalogs(t, version)
	runtimeEvidence := tool.EvidenceUnit{
		SourceKind: "log",
		Target:     "checkout-runtime",
		Coverage:   tool.EvidenceCoverage{Complete: true},
	}
	runtime := &capturingAgentRuntime{result: agentapi.RunResult{
		Status: agentapi.RunSucceeded,
		Output: json.RawMessage(`{
			"focus":"runtime",
			"summary":"Evidence collection completed, but report generation was incomplete.",
			"findings":[],
			"gaps":["The collected runtime evidence could not be converted into a schema-backed finding."],
			"covered_evidence_goals":[],
			"unresolved_evidence_goals":["runtime_and_operations"]
		}`),
		Evidence: agentapi.EvidenceSummary{ForcedConclusion: true},
		EvidenceUnits: []tool.EvidenceUnit{
			runtimeEvidence,
		},
	}}
	executor, err := NewAgentExecutor(schemas, agents, runtime)
	if err != nil {
		t.Fatal(err)
	}
	contract := json.RawMessage(`{
		"task_id":"task-1",
		"question":"Why is checkout failing?",
		"objective":"Trace the checkout failure",
		"entities":[],
		"investigation_goals":[],
		"evidence_goals":[
			{
				"id":"core_flow",
				"facet":"core_flow",
				"facets":["core_flow"],
				"required":true,
				"sources":["internal"],
				"freshness":"stable",
				"minimum_coverage":1
			},
			{
				"id":"runtime_and_operations",
				"facet":"runtime_and_operations",
				"facets":["runtime_and_operations"],
				"required":true,
				"sources":["internal"],
				"freshness":"current",
				"minimum_coverage":1
			}
		],
		"context":{}
	}`)
	input, err := PrepareHandoff(Handoff{
		WorkflowRunID:  "workflow-run",
		ProducerNodeID: "workflow.input",
		Schema:         agentapi.TaskContractSchemaRef(),
		Payload:        contract,
		Completeness:   Complete,
	}, 1<<20, schemas)
	if err != nil {
		t.Fatal(err)
	}
	nodeResult, err := executor.Execute(t.Context(), NodeRequest{
		WorkflowRunID: "workflow-run",
		Node: NodeDefinition{
			ID:   "investigate.runtime",
			Kind: NodeAgent,
			Agent: agentapi.DefinitionRef{
				ID: "investigator.runtime", Version: version,
			},
			InputSchema:  agentapi.TaskContractSchemaRef(),
			OutputSchema: agentapi.InvestigationReportSchemaRef(),
			Task: &TaskDirective{
				Purpose:        "Inspect runtime evidence.",
				RequiredFacets: []string{"runtime_and_operations"},
			},
			Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		},
		Inputs:        []Handoff{input},
		WorkflowInput: input,
		EffectivePermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.callCount != 1 ||
		nodeResult.Handoff.Completeness != Partial ||
		len(nodeResult.Handoff.EvidenceUnits) != 1 ||
		nodeResult.Handoff.EvidenceUnits[0].Target != runtimeEvidence.Target {
		t.Fatalf("node result=%+v runtime calls=%d", nodeResult, runtime.callCount)
	}

	codeEvidence := tool.EvidenceUnit{
		SourceKind: "code",
		Target:     "checkout.go",
		Coverage:   tool.EvidenceCoverage{Complete: true},
	}
	codeReport := Handoff{
		WorkflowRunID:  "workflow-run",
		ProducerNodeID: "investigate.code",
		Schema:         agentapi.InvestigationReportSchemaRef(),
		Payload: json.RawMessage(`{
			"focus":"code",
			"summary":"code report",
			"findings":[{
				"claim":"The checkout route reaches the placement handler.",
				"evidence_goal_ids":["core_flow"],
				"evidence":[{"kind":"code","reference":"checkout.go","summary":"Route registration"}],
				"confidence":0.9
			}],
			"gaps":[],
			"covered_evidence_goals":["core_flow"],
			"unresolved_evidence_goals":[]
		}`),
		EvidenceUnits: []tool.EvidenceUnit{codeEvidence},
		Completeness:  Complete,
	}
	joined, err := joinHandoffs(
		"workflow-run",
		"evidence.join",
		agentapi.InvestigationBundleSchemaRef(),
		JoinEvidenceView,
		[]Handoff{codeReport, nodeResult.Handoff},
		nil,
		nil,
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
	if joined.Completeness != Partial ||
		len(ledger.UnavailableTasks) != 0 ||
		len(ledger.EvidenceUnits) != 2 {
		t.Fatalf("joined evidence = %+v", ledger)
	}

	verifiedOutput, err := verifyBundle(verificationRunInput{
		workflowRunID: "workflow-run",
		node: NodeDefinition{
			ID:           "evidence.verify",
			Kind:         NodeVerifier,
			OutputSchema: agentapi.InvestigationVerifiedBundleSchemaRef(),
			Verifier: &VerifierSpec{
				RequiredGoals: []string{"core_flow", "runtime_and_operations"},
			},
		},
		inputs:   []Handoff{joined},
		maxBytes: 1 << 20,
		schemas:  schemas,
	})
	if err != nil {
		t.Fatal(err)
	}
	var verified verifiedEvidenceView
	if err := json.Unmarshal(verifiedOutput.handoff.Payload, &verified); err != nil {
		t.Fatal(err)
	}
	if verified.Completeness != Partial ||
		verified.Verification.StopReason != StopEvidenceInsufficient ||
		len(verified.SupportedClaims) != 1 ||
		verified.SupportedClaims[0].Claim != "The checkout route reaches the placement handler." ||
		len(verified.EvidenceUnits) != 2 {
		t.Fatalf("verified evidence = %+v", verified)
	}
}

type capturingAgentRuntime struct {
	request                agentapi.RunRequest
	result                 agentapi.RunResult
	err                    error
	called                 bool
	callCount              int
	projectedChildRunID    string
	projectedParentRunID   string
	projectedWorkflowRunID string
	projectedNodeID        string
	projectionStopped      bool
}

func (runtime *capturingAgentRuntime) Run(
	_ context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	runtime.called = true
	runtime.callCount++
	runtime.request = request
	return runtime.result, runtime.err
}

// ProjectToolEvents records the child-to-parent projection requested by the executor.
func (runtime *capturingAgentRuntime) ProjectToolEvents(
	childRunID string,
	parentRunID string,
	workflowRunID string,
	nodeID string,
) func() {
	runtime.projectedChildRunID = childRunID
	runtime.projectedParentRunID = parentRunID
	runtime.projectedWorkflowRunID = workflowRunID
	runtime.projectedNodeID = nodeID
	return func() {
		runtime.projectionStopped = true
	}
}
