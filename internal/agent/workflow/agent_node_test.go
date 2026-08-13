package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
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
	executor, err := NewAgentNodeExecutor(
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
		Purpose:        "Review the implementation evidence.",
		RequiredFacets: []string{"implementation"},
		InputRefs: []agentapi.EvidenceRef{{
			SourceKind: "code", Target: "repo/file.go", Section: "implementation",
		}},
		ParallelGroup: "review",
	}
	result, err := executor.Execute(t.Context(), NodeRequest{
		WorkflowRunID: "workflow_run_1", Node: node, Inputs: []Handoff{input},
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
	var directive TaskDirective
	if err := json.Unmarshal([]byte(runtime.request.Context[1].Content), &directive); err != nil {
		t.Fatalf("decode task directive: %v", err)
	}
	if directive.Purpose != node.Task.Purpose ||
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
		result.Usage != (WorkflowUsage{
			InputTokens: 31, OutputTokens: 7, ReasoningTokens: 2,
			TotalTokens: 38, ToolCalls: 3, CostMicros: 9,
		}) {
		t.Fatalf("runtime policy=%+v node usage=%+v", runtime.request.Policy, result.Usage)
	}
}

func TestAgentNodeExecutorRequiresExplicitJoinForMultipleInputs(t *testing.T) {
	runtime := &capturingAgentRuntime{}
	executor, err := NewAgentNodeExecutor(testSchemaRegistry(t), testAgentDefinitions(t), runtime)
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
	executor, err := NewAgentNodeExecutor(testSchemaRegistry(t), testAgentDefinitions(t), runtime)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(t.Context(), NodeRequest{
		WorkflowRunID: "workflow_run_1", Node: singleNodeWorkflow().Nodes[0],
		Inputs:               []Handoff{{Payload: json.RawMessage(`{"subject":"x"}`)}},
		EffectivePermissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err == nil || result.AgentRunID == "" ||
		result.Usage != (WorkflowUsage{
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
	executor, err := NewAgentNodeExecutor(testSchemaRegistry(t), testAgentDefinitions(t), runtime)
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

type capturingAgentRuntime struct {
	request agentapi.RunRequest
	result  agentapi.RunResult
	err     error
	called  bool
}

func (runtime *capturingAgentRuntime) Run(
	_ context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	runtime.called = true
	runtime.request = request
	return runtime.result, runtime.err
}
