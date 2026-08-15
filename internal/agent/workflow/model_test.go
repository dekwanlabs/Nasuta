package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
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

func TestPrepareStoredDefinitionRestoresLegacyExecutionBudget(t *testing.T) {
	schemas := testSchemaRegistry(t)
	definition := legacyDefinition(t, schemas)

	if _, err := Prepare(definition, schemas); err == nil ||
		!strings.Contains(err.Error(), "budgets must be positive") {
		t.Fatalf("Prepare legacy workflow error = %v", err)
	}
	prepared, err := prepareStored(definition, schemas)
	if err != nil {
		t.Fatalf("prepareStoredDefinition: %v", err)
	}
	if !prepared.legacyExecutionBudget ||
		prepared.ContentHash != definition.ContentHash {
		t.Fatalf("prepared legacy workflow = %+v", prepared)
	}
	maxRounds, maxDepth := executionLimits(prepared)
	if maxRounds != 1 || maxDepth != prepared.Budget.MaxNodes {
		t.Fatalf(
			"legacy execution limits = rounds %d depth %d",
			maxRounds,
			maxDepth,
		)
	}
	orchestrator := NewOrchestrator(schemas, staticOutputExecutor{}, nil)
	if _, _, err := orchestrator.prepareRun(
		prepared,
		RunRequest{RunID: "legacy_workflow_run"},
	); err != nil {
		t.Fatalf("prepareRun restored legacy workflow: %v", err)
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

func TestPrepareValidatesJoinModeOwnership(t *testing.T) {
	definition := testWorkflow()
	definition.Nodes[0].JoinMode = JoinEvidenceView
	if _, err := Prepare(definition, testSchemaRegistry(t)); err == nil ||
		!strings.Contains(err.Error(), "cannot use join mode") {
		t.Fatalf("non-join mode error = %v", err)
	}

	definition = testWorkflow()
	definition.Nodes[2].JoinMode = JoinMode("unsupported")
	if _, err := Prepare(definition, testSchemaRegistry(t)); err == nil ||
		!strings.Contains(err.Error(), "join node") {
		t.Fatalf("unsupported join mode error = %v", err)
	}
}

func TestValidateNodeRequiresHighRiskGoalsToBeRequired(t *testing.T) {
	node := NodeDefinition{
		ID:           "evidence.verify",
		Kind:         NodeVerifier,
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "review.report", Version: 1},
		Timeout:      time.Second,
		Verifier: &VerifierSpec{
			RequiredGoals: []string{"core_flow"},
			HighRiskGoals: []string{"live_state"},
		},
	}
	err := validateNode("delivery.review", node, testSchemaRegistry(t))
	if err == nil || !strings.Contains(
		err.Error(),
		`high-risk goal "live_state" must also be required`,
	) {
		t.Fatalf("validateNode error = %v", err)
	}

	node.Verifier.RequiredGoals = append(
		node.Verifier.RequiredGoals,
		"live_state",
	)
	if err := validateNode(
		"delivery.review",
		node,
		testSchemaRegistry(t),
	); err != nil {
		t.Fatalf("validateNode valid high-risk goals: %v", err)
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

func TestOrchestratorRejectsExecutionPositionBeforeDispatch(t *testing.T) {
	tests := []struct {
		name       string
		definition Definition
		request    RunRequest
	}{
		{
			name:       "round",
			definition: singleNodeWorkflow(),
			request: RunRequest{
				RunID: "workflow_round_exhausted", Round: 2,
				Input: json.RawMessage(`{"subject":"x"}`),
			},
		},
		{
			name:       "depth",
			definition: testWorkflow(),
			request: RunRequest{
				RunID: "workflow_depth_exhausted", Round: 1, BaseDepth: 1,
				Input: json.RawMessage(`{"subject":"x"}`),
			},
		},
		{
			name:       "depth overflow",
			definition: singleNodeWorkflow(),
			request: RunRequest{
				RunID: "workflow_depth_overflow", Round: 1,
				BaseDepth: int(^uint(0) >> 1),
				Input:     json.RawMessage(`{"subject":"x"}`),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observer := &executionPositionObserver{}
			result, err := NewOrchestrator(
				testSchemaRegistry(t),
				staticOutputExecutor{},
				nil,
			).RunObserved(t.Context(), test.definition, test.request, observer)
			if !errors.Is(err, ErrBudgetExhausted) {
				t.Fatalf("RunObserved error = %v, want workflow budget exhausted", err)
			}
			if result.RunID != test.request.RunID ||
				result.StopReason != StopBudgetExhausted {
				t.Fatalf("result = %+v", result)
			}
			if started := observer.Started(); len(started) != 0 {
				t.Fatalf("started nodes = %v, want none", started)
			}
		})
	}
}

func TestOrchestratorPropagatesRoundAndGlobalDepth(t *testing.T) {
	definition := testWorkflow()
	definition.Budget.MaxRounds = 2
	definition.Budget.MaxDepth = 4
	observer := &executionPositionObserver{}
	_, err := NewOrchestrator(
		testSchemaRegistry(t),
		staticOutputExecutor{},
		nil,
	).RunObserved(t.Context(), definition, RunRequest{
		RunID: "workflow_position", Round: 2, BaseDepth: 2,
		Input: json.RawMessage(`{"subject":"x"}`),
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	}, observer)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]executionPosition{
		"review.a":   {round: 2, depth: 3},
		"review.b":   {round: 2, depth: 3},
		"synthesize": {round: 2, depth: 4},
	}
	if got := observer.Started(); !reflect.DeepEqual(got, want) {
		t.Fatalf("execution positions = %v, want %v", got, want)
	}
}

func TestOrchestratorRunsParallelWaveAndJoinsByDeclaredEdgeOrder(t *testing.T) {
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
	if payloads[0]["node"] != "review.b" || payloads[1]["node"] != "review.a" {
		t.Fatalf("join order=%v", payloads)
	}
}

func TestOrchestratorTracesMultipleTerminalAggregation(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
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
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		events = append(events, event)
	})
	agentExecutor, err := NewAgentExecutor(
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
	if len(events) != 8 {
		t.Fatalf("events = %#v", events)
	}
	traceID := events[0].TraceID
	childRuns := make(map[string]string, 2)
	var dispatch, aggregate, converged *domain.EvaluationTrace
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
		case "workflow.converged":
			converged = event
		}
	}
	if len(childRuns) != 2 || dispatch == nil || aggregate == nil || converged == nil {
		t.Fatalf(
			"child runs=%v dispatch=%#v aggregate=%#v converged=%#v",
			childRuns,
			dispatch,
			aggregate,
			converged,
		)
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
	if !ok || !reflect.DeepEqual(producers, []string{"review.b", "review.a"}) ||
		aggregate.WorkflowNodeID != "synthesize" {
		t.Fatalf("aggregate = %#v", aggregate)
	}
	if converged.Status != "completed" ||
		converged.Output["outcome"] != "complete" ||
		converged.Output["completeness"] != Complete ||
		converged.Output["completed_node_count"] != 3 ||
		converged.Output["unavailable_node_count"] != 0 ||
		converged.Output["waiting_human_count"] != 0 {
		t.Fatalf("converged = %#v", converged)
	}
}

func TestOrchestratorEmitsWorkflowNodeTraceFromSharedScope(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
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
	if len(events) != 2 {
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
	converged := events[1]
	if converged.Sequence != 2 || converged.Node != "workflow.converged" ||
		converged.Status != "completed" ||
		converged.Output["outcome"] != "complete" ||
		converged.Output["completeness"] != Complete ||
		converged.Output["completed_node_count"] != 1 {
		t.Fatalf("converged = %#v", converged)
	}
}

func TestOrchestratorTracesWaitingHumanStatus(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Nodes[0].Kind = NodeHumanApproval
	definition.Nodes[0].OutputSchema = definition.InputSchema
	definition.OutputSchema = definition.InputSchema
	var events []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		events = append(events, event)
	})
	_, err := NewOrchestrator(testSchemaRegistry(t), nil, nil).Run(ctx, definition, RunRequest{
		RunID: "workflow_trace_wait", Input: json.RawMessage(`{"subject":"x"}`),
	})
	if !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("error = %v", err)
	}
	if len(events) != 2 ||
		events[0].Status != "waiting_human" ||
		events[0].Output["node_id"] != "review.a" {
		t.Fatalf("events = %#v", events)
	}
	converged := events[1]
	if converged.Node != "workflow.converged" ||
		converged.Status != "waiting_human" ||
		converged.Output["outcome"] != "waiting_human" ||
		converged.Output["error_code"] != "human_approval_required" ||
		converged.Output["completed_node_count"] != 0 ||
		converged.Output["waiting_human_count"] != 1 {
		t.Fatalf("converged = %#v", converged)
	}
}

func TestOrchestratorTracesPartialAndUnavailableConvergence(t *testing.T) {
	for _, test := range []struct {
		name         string
		completeness Completeness
		wantStatus   string
	}{
		{name: "partial", completeness: Partial, wantStatus: "degraded"},
		{name: "unavailable", completeness: Unavailable, wantStatus: "degraded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []domain.EvaluationTrace
			ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
				events = append(events, event)
			})
			orchestrator := NewOrchestrator(
				testSchemaRegistry(t),
				completenessOutputExecutor{completeness: test.completeness},
				nil,
			)
			result, err := orchestrator.Run(ctx, singleNodeWorkflow(), RunRequest{
				RunID: "workflow_trace_" + test.name,
				Input: json.RawMessage(`{"subject":"x"}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Output.Completeness != test.completeness {
				t.Fatalf("completeness = %q", result.Output.Completeness)
			}
			if len(events) != 2 {
				t.Fatalf("events = %#v", events)
			}
			converged := events[1]
			if converged.Node != "workflow.converged" ||
				converged.Status != test.wantStatus ||
				converged.Output["outcome"] != string(test.completeness) ||
				converged.Output["completeness"] != test.completeness {
				t.Fatalf("converged = %#v", converged)
			}
		})
	}
}

func TestOrchestratorTracesFailedConvergence(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		events = append(events, event)
	})
	_, err := NewOrchestrator(
		testSchemaRegistry(t),
		invalidOutputExecutor{},
		nil,
	).Run(ctx, singleNodeWorkflow(), RunRequest{
		RunID: "workflow_trace_failed",
		Input: json.RawMessage(`{"subject":"x"}`),
	})
	if err == nil {
		t.Fatal("expected workflow failure")
	}
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	converged := events[1]
	if converged.Node != "workflow.converged" ||
		converged.Status != "failed" ||
		converged.Output["outcome"] != "failed" ||
		converged.Output["error_code"] != "workflow_failed" ||
		converged.Output["completed_node_count"] != 0 {
		t.Fatalf("converged = %#v", converged)
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

type executionPosition struct {
	round int
	depth int
}

type executionPositionObserver struct {
	mu      sync.Mutex
	started map[string]executionPosition
}

func (observer *executionPositionObserver) NodeStarted(
	_ context.Context,
	request NodeRequest,
) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.started == nil {
		observer.started = make(map[string]executionPosition)
	}
	observer.started[request.Node.ID] = executionPosition{
		round: request.Round,
		depth: request.Depth,
	}
	return nil
}

func (*executionPositionObserver) NodeSucceeded(
	context.Context,
	NodeRequest,
	NodeResult,
	*GateDecision,
) error {
	return nil
}

func (*executionPositionObserver) NodeFailed(
	context.Context,
	NodeRequest,
	NodeResult,
	error,
) error {
	return nil
}

func (observer *executionPositionObserver) Started() map[string]executionPosition {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	started := make(map[string]executionPosition, len(observer.started))
	for nodeID, position := range observer.started {
		started[nodeID] = position
	}
	return started
}

type invalidOutputExecutor struct{}

func (invalidOutputExecutor) Execute(context.Context, NodeRequest) (NodeResult, error) {
	return NodeResult{Handoff: Handoff{Payload: json.RawMessage(`{"unexpected":true}`), Completeness: Complete}}, nil
}

type staticOutputExecutor struct{}

type completenessOutputExecutor struct {
	completeness Completeness
}

type agentRuntimeFunc func(context.Context, agentapi.RunRequest) (agentapi.RunResult, error)

func (run agentRuntimeFunc) Run(ctx context.Context, request agentapi.RunRequest) (agentapi.RunResult, error) {
	return run(ctx, request)
}

func (staticOutputExecutor) Execute(_ context.Context, request NodeRequest) (NodeResult, error) {
	payload, _ := json.Marshal(map[string]string{"node": request.Node.ID})
	return NodeResult{Handoff: Handoff{Payload: payload, Completeness: Complete}}, nil
}

func (executor completenessOutputExecutor) Execute(
	_ context.Context,
	request NodeRequest,
) (NodeResult, error) {
	payload, _ := json.Marshal(map[string]string{"node": request.Node.ID})
	return NodeResult{
		Handoff: Handoff{Payload: payload, Completeness: executor.completeness},
	}, nil
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

func testWorkflow() Definition {
	report := agentapi.SchemaRef{ID: "review.report", Version: 1}
	reportList := agentapi.SchemaRef{ID: "review.report.list", Version: 1}
	return Definition{
		ID: "delivery.review", Version: 1, Purpose: "Run independent reviewers.",
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: reportList,
		Permissions:  agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		Budget: Budget{
			MaxNodes: 3, MaxParallelism: 2, MaxRounds: 1, MaxDepth: 2,
			Timeout: time.Second, MaxHandoffBytes: 4096,
		},
		FailurePolicy: FailurePolicy{Mode: FailFast},
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

func legacyDefinition(
	t *testing.T,
	schemas *agentapi.SchemaRegistry,
) Definition {
	t.Helper()
	definition, err := Prepare(testWorkflow(), schemas)
	if err != nil {
		t.Fatal(err)
	}
	definition.ContentHash = ""
	definition.Budget.MaxRounds = 0
	definition.Budget.MaxDepth = 0
	definition.Budget.MaxDuplicateRatio = 0
	legacy := struct {
		ID            string                    `json:"id"`
		Version       int64                     `json:"version"`
		Purpose       string                    `json:"purpose"`
		InputSchema   agentapi.SchemaRef        `json:"input_schema"`
		OutputSchema  agentapi.SchemaRef        `json:"output_schema"`
		Nodes         []NodeDefinition          `json:"nodes"`
		Edges         []EdgeDefinition          `json:"edges"`
		Permissions   agentapi.PermissionPolicy `json:"permissions"`
		Budget        legacyBudget              `json:"budget"`
		FailurePolicy FailurePolicy             `json:"failure_policy"`
		ContentHash   string                    `json:"content_hash"`
	}{
		ID: definition.ID, Version: definition.Version,
		Purpose: definition.Purpose, InputSchema: definition.InputSchema,
		OutputSchema: definition.OutputSchema, Nodes: definition.Nodes,
		Edges: definition.Edges, Permissions: definition.Permissions,
		Budget: legacyBudget{
			MaxNodes:        definition.Budget.MaxNodes,
			MaxParallelism:  definition.Budget.MaxParallelism,
			Timeout:         definition.Budget.Timeout,
			MaxHandoffBytes: definition.Budget.MaxHandoffBytes,
			MaxInputTokens:  definition.Budget.MaxInputTokens,
			MaxOutputTokens: definition.Budget.MaxOutputTokens,
			MaxTotalTokens:  definition.Budget.MaxTotalTokens,
			MaxToolCalls:    definition.Budget.MaxToolCalls,
			MaxCostMicros:   definition.Budget.MaxCostMicros,
			MaxRetries:      definition.Budget.MaxRetries,
		},
		FailurePolicy: definition.FailurePolicy,
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	definition.ContentHash = hex.EncodeToString(sum[:])
	return definition
}

type legacyBudget struct {
	MaxNodes        int           `json:"max_nodes"`
	MaxParallelism  int           `json:"max_parallelism"`
	Timeout         time.Duration `json:"timeout"`
	MaxHandoffBytes int64         `json:"max_handoff_bytes"`
	MaxInputTokens  int64         `json:"max_input_tokens"`
	MaxOutputTokens int64         `json:"max_output_tokens"`
	MaxTotalTokens  int64         `json:"max_total_tokens"`
	MaxToolCalls    int64         `json:"max_tool_calls"`
	MaxCostMicros   int64         `json:"max_cost_micros"`
	MaxRetries      int64         `json:"max_retries"`
}

func singleNodeWorkflow() Definition {
	report := agentapi.SchemaRef{ID: "review.report", Version: 1}
	return Definition{
		ID: "delivery.review.single", Version: 1, Purpose: "Run one reviewer.",
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: report,
		Budget: Budget{
			MaxNodes: 1, MaxParallelism: 1, MaxRounds: 1, MaxDepth: 1,
			Timeout: time.Second, MaxHandoffBytes: 4096,
		},
		FailurePolicy: FailurePolicy{Mode: FailFast},
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
			ID: "gate.decision", Version: 1,
			Document: json.RawMessage(`{
					"type":"object",
					"required":["gate_id","subject_hash","decision","evaluated_at"],
					"properties":{
						"gate_id":{"type":"string","minLength":1},
						"subject_hash":{"type":"string"},
						"decision":{"type":"string","minLength":1},
						"reason_codes":{"type":"array","items":{"type":"string"}},
						"finding_ids":{"type":"array","items":{"type":"string"}},
						"evaluated_at":{"type":"string","minLength":1}
					},
					"additionalProperties":false
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
