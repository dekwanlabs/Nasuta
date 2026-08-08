package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
)

func TestPrepareRejectsCyclesAndSchemaMismatches(t *testing.T) {
	schemas := testSchemaRegistry(t)
	definition := testWorkflow()
	definition.Edges = append(definition.Edges, EdgeDefinition{From: "synthesize", To: "review.a", Required: true})
	if _, err := Prepare(definition, schemas); err == nil {
		t.Fatal("expected cycle to be rejected")
	}

	definition = testWorkflow()
	definition.Nodes[2].InputSchema = agentapi.SchemaRef{ID: "other.input", Version: 1}
	if _, err := Prepare(definition, schemas); err == nil {
		t.Fatal("expected incompatible edge schemas to be rejected")
	}
}

func TestPrepareRequiresPublishedSchemasAndAcceptsExplicitCompatibility(t *testing.T) {
	schemas := testSchemaRegistry(t)
	definition := testWorkflow()
	definition.Nodes[2].InputSchema = agentapi.SchemaRef{ID: "review.report.consumer", Version: 1}
	if _, err := Prepare(definition, schemas); err != nil {
		t.Fatalf("Prepare compatible workflow: %v", err)
	}

	definition.OutputSchema = agentapi.SchemaRef{ID: "review.report.missing", Version: 1}
	if _, err := Prepare(definition, schemas); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Prepare missing schema error = %v", err)
	}
}

func TestPrepareRejectsUnknownAndExpandedPermissionScopes(t *testing.T) {
	schemas := testSchemaRegistry(t)

	unknown := singleNodeWorkflow()
	unknown.Permissions.Scopes = []string{"approval.write"}
	if _, err := Prepare(unknown, schemas); err == nil ||
		!strings.Contains(err.Error(), "not registered") {
		t.Fatalf("Prepare unknown scope error = %v", err)
	}

	expanded := singleNodeWorkflow()
	expanded.Permissions.Scopes = []string{"knowledge.read"}
	expanded.Nodes[0].Permissions.Scopes = []string{
		"knowledge.read",
		"knowledge.write",
	}
	if _, err := Prepare(expanded, schemas); err == nil ||
		!strings.Contains(err.Error(), "outside the allowed set") {
		t.Fatalf("Prepare expanded node scope error = %v", err)
	}
}

func TestTopologicalOrderIsStableByNodeID(t *testing.T) {
	order, err := TopologicalOrder(testWorkflow(), testSchemaRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(order))
	for _, node := range order {
		got = append(got, node.ID)
	}
	want := []string{"review.a", "review.b", "synthesize"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("order=%v want=%v", got, want)
		}
	}
}

func TestIntersectPermissionsNeverExpandsScopes(t *testing.T) {
	got := IntersectPermissions(
		agentapi.PermissionPolicy{Scopes: []string{"knowledge.read", "code.read", "write"}},
		agentapi.PermissionPolicy{Scopes: []string{"code.read", "knowledge.read"}},
		agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	)
	if len(got.Scopes) != 1 || got.Scopes[0] != "knowledge.read" {
		t.Fatalf("permissions=%v", got.Scopes)
	}
}

func TestOrchestratorRunsParallelWaveAndJoinsByProducerNodeID(t *testing.T) {
	executor := &recordingExecutor{started: make(chan string, 2), release: make(chan struct{})}
	orchestrator := NewOrchestrator(testSchemaRegistry(t), executor, nil)
	done := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		result, runErr = orchestrator.Run(context.Background(), testWorkflow(), RunRequest{
			RunID: "workflow_run_1", Input: json.RawMessage(`{"subject":"x"}`),
			ActorPermissions:    agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
			ScenarioPermissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		})
		close(done)
	}()
	first := <-executor.started
	second := <-executor.started
	if first == second {
		t.Fatalf("parallel wave started duplicate node %q", first)
	}
	close(executor.release)
	<-done
	if runErr != nil {
		t.Fatal(runErr)
	}
	var payloads []map[string]string
	if err := json.Unmarshal(result.Output.Payload, &payloads); err != nil {
		t.Fatal(err)
	}
	if payloads[0]["node"] != "review.a" || payloads[1]["node"] != "review.b" {
		t.Fatalf("join order=%v", payloads)
	}
}

func TestOrchestratorTracesMultipleTerminalAggregation(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := executiontrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		events = append(events, event)
	})
	definition := testWorkflow()
	definition.Nodes = definition.Nodes[:2]
	definition.Edges = nil
	definition.Budget.MaxNodes = 2
	orchestrator := NewOrchestrator(testSchemaRegistry(t), staticOutputExecutor{}, nil)
	_, err := orchestrator.Run(ctx, definition, RunRequest{
		RunID: "workflow_trace_terminals", Input: json.RawMessage(`{"subject":"x"}`),
		ActorPermissions:    agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		ScenarioPermissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var aggregate *domain.EvaluationTrace
	for index := range events {
		if events[index].Node == "multi_agent_aggregate" {
			aggregate = &events[index]
			break
		}
	}
	if aggregate == nil || aggregate.RunID != "workflow_trace_terminals" ||
		aggregate.WorkflowRunID != "workflow_trace_terminals" || aggregate.WorkflowNodeID != "workflow.output" {
		t.Fatalf("aggregate = %#v events = %#v", aggregate, events)
	}
	producers, ok := aggregate.Input["producer_node_ids"].([]string)
	if !ok || !reflect.DeepEqual(producers, []string{"review.a", "review.b"}) {
		t.Fatalf("aggregate producers = %#v", aggregate.Input["producer_node_ids"])
	}
}

func TestOrchestratorAggregatesParallelChildRunTrace(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := executiontrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		events = append(events, event)
	})
	agentExecutor, err := NewAgentNodeExecutor(
		testSchemaRegistry(t),
		testAgentDefinitions(t),
		agentRuntimeFunc(func(_ context.Context, request agentapi.RunRequest) (agentapi.RunResult, error) {
			payload, marshalErr := json.Marshal(map[string]string{"node": request.Correlation.NodeID})
			if marshalErr != nil {
				return agentapi.RunResult{}, marshalErr
			}
			return agentapi.RunResult{
				RunID: request.RunID, Status: agentapi.RunSucceeded, Output: payload,
			}, nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	orchestrator := NewOrchestrator(testSchemaRegistry(t), agentExecutor, nil)
	_, err = orchestrator.Run(ctx, testWorkflow(), RunRequest{
		RunID: "workflow_trace_parallel", Input: json.RawMessage(`{"subject":"x"}`),
		ActorPermissions:    agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		ScenarioPermissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("events = %#v", events)
	}
	traceID := events[0].TraceID
	childRuns := make(map[string]string, 2)
	var dispatch, aggregate *domain.EvaluationTrace
	for index := range events {
		event := &events[index]
		if event.Sequence != index+1 || event.TraceID == "" || event.TraceID != traceID {
			t.Fatalf("event %d = %#v", index, event)
		}
		switch event.Node {
		case "multi_agent_child_run":
			if event.RunID == "" || event.RunID != event.AgentRunID ||
				event.ParentRunID != "workflow_trace_parallel" ||
				event.WorkflowRunID != "workflow_trace_parallel" ||
				event.WorkflowNodeID == "" {
				t.Fatalf("child event = %#v", event)
			}
			childRuns[event.RunID] = event.WorkflowNodeID
		case "multi_agent_dispatch":
			dispatch = event
		case "multi_agent_aggregate":
			aggregate = event
		}
	}
	if len(childRuns) != 2 || dispatch == nil || aggregate == nil {
		t.Fatalf("child runs=%v dispatch=%#v aggregate=%#v", childRuns, dispatch, aggregate)
	}
	if dispatch.RunID != "workflow_trace_parallel" || dispatch.WorkflowRunID != "workflow_trace_parallel" ||
		dispatch.Output["dispatched"] != 2 || dispatch.Output["failed"] != 0 {
		t.Fatalf("dispatch = %#v", dispatch)
	}
	dispatchedRuns, ok := dispatch.Output["child_run_ids"].([]string)
	if !ok || len(dispatchedRuns) != 2 {
		t.Fatalf("dispatch child runs = %#v", dispatch.Output["child_run_ids"])
	}
	for _, runID := range dispatchedRuns {
		if _, exists := childRuns[runID]; !exists {
			t.Fatalf("dispatched child %q not found in %v", runID, childRuns)
		}
	}
	producers, ok := aggregate.Input["producer_node_ids"].([]string)
	if !ok || !reflect.DeepEqual(producers, []string{"review.a", "review.b"}) ||
		aggregate.WorkflowNodeID != "synthesize" {
		t.Fatalf("aggregate = %#v", aggregate)
	}
}

func TestOrchestratorEmitsWorkflowNodeTraceFromSharedScope(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := executiontrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		events = append(events, event)
	})
	orchestrator := NewOrchestrator(testSchemaRegistry(t), staticOutputExecutor{}, nil)
	_, err := orchestrator.Run(ctx, singleNodeWorkflow(), RunRequest{
		RunID: "workflow_trace_1", ParentRunID: "qa_parent_1",
		Input: json.RawMessage(`{"subject":"x"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	event := events[0]
	if event.Sequence != 1 || event.Node != "workflow_node" || event.Status != "completed" ||
		event.TraceID == "" || event.RunID != "workflow_trace_1" ||
		event.ParentRunID != "qa_parent_1" ||
		event.WorkflowRunID != "workflow_trace_1" || event.WorkflowNodeID != "review.a" ||
		event.Output["workflow_run_id"] != "workflow_trace_1" || event.Output["node_id"] != "review.a" {
		t.Fatalf("event = %#v", event)
	}
}

func TestOrchestratorTracesWaitingHumanStatus(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Nodes[0].Kind = NodeHumanApproval
	definition.Nodes[0].OutputSchema = definition.InputSchema
	definition.OutputSchema = definition.InputSchema
	var events []domain.EvaluationTrace
	ctx := executiontrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		events = append(events, event)
	})
	_, err := NewOrchestrator(testSchemaRegistry(t), nil, nil).Run(ctx, definition, RunRequest{
		RunID: "workflow_trace_wait", Input: json.RawMessage(`{"subject":"x"}`),
	})
	if !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 1 || events[0].Status != "waiting_human" || events[0].Output["node_id"] != "review.a" {
		t.Fatalf("events = %#v", events)
	}
}

func TestOrchestratorRejectsInputOutsideWorkflowSchema(t *testing.T) {
	orchestrator := NewOrchestrator(testSchemaRegistry(t), &recordingExecutor{}, nil)
	_, err := orchestrator.Run(t.Context(), testWorkflow(), RunRequest{
		RunID: "workflow_run_1", Input: json.RawMessage(`{"wrong":"x"}`),
	})
	if err == nil || !strings.Contains(err.Error(), `validate schema "review.subject"`) {
		t.Fatalf("Run error = %v, want workflow input schema failure", err)
	}
}

func TestOrchestratorRejectsNodeOutputOutsideSchema(t *testing.T) {
	orchestrator := NewOrchestrator(testSchemaRegistry(t), invalidOutputExecutor{}, nil)
	_, err := orchestrator.Run(t.Context(), singleNodeWorkflow(), RunRequest{
		RunID: "workflow_run_1", Input: json.RawMessage(`{"subject":"x"}`),
	})
	if err == nil || !strings.Contains(err.Error(), `validate schema "review.report"`) {
		t.Fatalf("Run error = %v, want node output schema failure", err)
	}
}

func TestOrchestratorValidatesPayloadAgainstCompatibleConsumer(t *testing.T) {
	schemas := testSchemaRegistry(t)
	definition := singleNodeWorkflow()
	definition.OutputSchema = agentapi.SchemaRef{ID: "review.report.strict", Version: 1}
	orchestrator := NewOrchestrator(schemas, staticOutputExecutor{}, nil)
	_, err := orchestrator.Run(t.Context(), definition, RunRequest{
		RunID: "workflow_run_1", Input: json.RawMessage(`{"subject":"x"}`),
	})
	if err == nil || !strings.Contains(err.Error(), `validate schema "review.report.strict"`) {
		t.Fatalf("Run error = %v, want final consumer payload failure", err)
	}
}

func TestPrepareHandoffDetectsMutation(t *testing.T) {
	schemas := testSchemaRegistry(t)
	handoff, err := PrepareHandoff(Handoff{
		WorkflowRunID: "run_1", ProducerNodeID: "review.a",
		Schema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		Payload: json.RawMessage(`{"node":"review.a"}`), Completeness: Complete,
	}, 1024, schemas)
	if err != nil {
		t.Fatal(err)
	}
	handoff.Payload = json.RawMessage(`{"node":"review.b"}`)
	if _, err := PrepareHandoff(handoff, 1024, schemas); err == nil {
		t.Fatal("expected content hash mismatch")
	}
}

func TestPrepareHandoffRejectsPayloadOutsideSchema(t *testing.T) {
	_, err := PrepareHandoff(Handoff{
		WorkflowRunID: "run_1", ProducerNodeID: "review.a",
		Schema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		Payload: json.RawMessage(`{"unexpected":true}`), Completeness: Complete,
	}, 1024, testSchemaRegistry(t))
	if err == nil || !strings.Contains(err.Error(), `validate schema "review.report"`) {
		t.Fatalf("PrepareHandoff error = %v", err)
	}
}

func TestPrepareHandoffBoundsPayloadBeforeSchemaValidation(t *testing.T) {
	_, err := PrepareHandoff(Handoff{
		WorkflowRunID: "run_1", ProducerNodeID: "review.a",
		Schema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		Payload: json.RawMessage(`{"unexpected":true}`), Completeness: Complete,
	}, 4, testSchemaRegistry(t))
	if err == nil || !strings.Contains(err.Error(), "exceeds 4 bytes") {
		t.Fatalf("PrepareHandoff error = %v, want size limit", err)
	}
}

type recordingExecutor struct {
	started chan string
	release chan struct{}
}

type invalidOutputExecutor struct{}

func (invalidOutputExecutor) Execute(context.Context, NodeRequest) (NodeResult, error) {
	return NodeResult{Handoff: Handoff{Payload: json.RawMessage(`{"unexpected":true}`), Completeness: Complete}}, nil
}

type staticOutputExecutor struct{}

type agentRuntimeFunc func(context.Context, agentapi.RunRequest) (agentapi.RunResult, error)

func (run agentRuntimeFunc) Run(ctx context.Context, request agentapi.RunRequest) (agentapi.RunResult, error) {
	return run(ctx, request)
}

func (staticOutputExecutor) Execute(_ context.Context, request NodeRequest) (NodeResult, error) {
	payload, _ := json.Marshal(map[string]string{"node": request.Node.ID})
	return NodeResult{Handoff: Handoff{Payload: payload, Completeness: Complete}}, nil
}

func (executor *recordingExecutor) Execute(ctx context.Context, request NodeRequest) (NodeResult, error) {
	if request.Node.Kind == NodeAgent {
		executor.started <- request.Node.ID
		select {
		case <-executor.release:
		case <-ctx.Done():
			return NodeResult{}, ctx.Err()
		}
	}
	payload, _ := json.Marshal(map[string]string{"node": request.Node.ID})
	return NodeResult{Handoff: Handoff{Payload: payload, Completeness: Complete}}, nil
}

func testWorkflow() WorkflowDefinition {
	report := agentapi.SchemaRef{ID: "review.report", Version: 1}
	reportList := agentapi.SchemaRef{ID: "review.report.list", Version: 1}
	return WorkflowDefinition{
		ID: "delivery.review", Version: 1, Purpose: "Run independent reviewers.",
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: reportList,
		Permissions:  agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		Budget: WorkflowBudget{
			MaxNodes: 3, MaxParallelism: 2, Timeout: time.Second, MaxHandoffBytes: 4096,
		},
		FailurePolicy: WorkflowFailurePolicy{Mode: FailFast},
		Nodes: []NodeDefinition{
			{
				ID: "review.b", Kind: NodeAgent,
				Agent:        agentapi.DefinitionRef{ID: "review.security", Version: 1},
				InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
				OutputSchema: report, Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
				Timeout: time.Second,
			},
			{
				ID: "review.a", Kind: NodeAgent,
				Agent:        agentapi.DefinitionRef{ID: "review.correctness", Version: 1},
				InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
				OutputSchema: report, Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
				Timeout: time.Second,
			},
			{
				ID: "synthesize", Kind: NodeJoin,
				InputSchema: report, OutputSchema: reportList,
				Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
				Timeout:     time.Second,
			},
		},
		Edges: []EdgeDefinition{
			{From: "review.b", To: "synthesize", Required: true},
			{From: "review.a", To: "synthesize", Required: true},
		},
	}
}

func singleNodeWorkflow() WorkflowDefinition {
	report := agentapi.SchemaRef{ID: "review.report", Version: 1}
	return WorkflowDefinition{
		ID: "delivery.review.single", Version: 1, Purpose: "Run one reviewer.",
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: report,
		Budget: WorkflowBudget{
			MaxNodes: 1, MaxParallelism: 1, Timeout: time.Second, MaxHandoffBytes: 4096,
		},
		FailurePolicy: WorkflowFailurePolicy{Mode: FailFast},
		Nodes: []NodeDefinition{{
			ID: "review.a", Kind: NodeAgent,
			Agent:        agentapi.DefinitionRef{ID: "review.correctness", Version: 1},
			InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
			OutputSchema: report, Timeout: time.Second,
		}},
	}
}

func testSchemaRegistry(t *testing.T) *agentapi.SchemaRegistry {
	t.Helper()
	registry := agentapi.NewSchemaRegistry()
	definitions := []agentapi.SchemaDefinition{
		{
			ID: "review.subject", Version: 1,
			Document: json.RawMessage(`{
				"type":"object",
				"required":["subject"],
				"properties":{"subject":{"type":"string","minLength":1}},
				"additionalProperties":false
			}`),
		},
		{
			ID: "review.report", Version: 1,
			Document: json.RawMessage(`{
				"type":"object",
				"required":["node"],
				"properties":{"node":{"type":"string","minLength":1}},
				"additionalProperties":false
			}`),
		},
		{
			ID: "review.report.consumer", Version: 1,
			CompatibleFrom: []agentapi.SchemaRef{{ID: "review.report", Version: 1}},
			Document: json.RawMessage(`{
				"type":"object",
				"required":["node"],
				"properties":{"node":{"type":"string","minLength":1}},
				"additionalProperties":false
			}`),
		},
		{
			ID: "review.report.strict", Version: 1,
			CompatibleFrom: []agentapi.SchemaRef{{ID: "review.report", Version: 1}},
			Document: json.RawMessage(`{
				"type":"object",
				"required":["node","approved"],
				"properties":{"node":{"type":"string"},"approved":{"type":"boolean"}},
				"additionalProperties":false
			}`),
		},
		{
			ID: "review.report.list", Version: 1,
			Document: json.RawMessage(`{
				"type":"array",
				"items":{
					"type":"object",
					"required":["node"],
					"properties":{"node":{"type":"string","minLength":1}},
					"additionalProperties":false
				}
			}`),
		},
		{
			ID: "other.input", Version: 1,
			Document: json.RawMessage(`{"type":"object","required":["other"]}`),
		},
	}
	if err := registry.Publish(definitions); err != nil {
		t.Fatalf("publish test schemas: %v", err)
	}
	return registry
}
