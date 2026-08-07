package agentworkflow

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
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
	mu      sync.Mutex
	started chan string
	release chan struct{}
}

type invalidOutputExecutor struct{}

func (invalidOutputExecutor) Execute(context.Context, NodeRequest) (NodeResult, error) {
	return NodeResult{Handoff: Handoff{Payload: json.RawMessage(`{"unexpected":true}`), Completeness: Complete}}, nil
}

type staticOutputExecutor struct{}

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
