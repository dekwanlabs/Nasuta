package agentworkflow

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestPrepareRejectsCyclesAndSchemaMismatches(t *testing.T) {
	definition := testWorkflow()
	definition.Edges = append(definition.Edges, EdgeDefinition{From: "synthesize", To: "review.a", Required: true})
	if _, err := Prepare(definition); err == nil {
		t.Fatal("expected cycle to be rejected")
	}

	definition = testWorkflow()
	definition.Nodes[2].InputSchema = agentapi.SchemaRef{ID: "other.input", Version: 1}
	if _, err := Prepare(definition); err == nil {
		t.Fatal("expected incompatible edge schemas to be rejected")
	}
}

func TestTopologicalOrderIsStableByNodeID(t *testing.T) {
	order, err := TopologicalOrder(testWorkflow())
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
	orchestrator := NewOrchestrator(executor, nil)
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

func TestPrepareHandoffDetectsMutation(t *testing.T) {
	handoff, err := PrepareHandoff(Handoff{
		WorkflowRunID: "run_1", ProducerNodeID: "review.a",
		Schema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		Payload: json.RawMessage(`{"ok":true}`), Completeness: Complete,
	}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	handoff.Payload = json.RawMessage(`{"ok":false}`)
	if _, err := PrepareHandoff(handoff, 1024); err == nil {
		t.Fatal("expected content hash mismatch")
	}
}

type recordingExecutor struct {
	mu      sync.Mutex
	started chan string
	release chan struct{}
}

func (executor *recordingExecutor) Execute(ctx context.Context, request NodeRequest) (Handoff, error) {
	if request.Node.Kind == NodeAgent {
		executor.started <- request.Node.ID
		select {
		case <-executor.release:
		case <-ctx.Done():
			return Handoff{}, ctx.Err()
		}
	}
	payload, _ := json.Marshal(map[string]string{"node": request.Node.ID})
	return Handoff{Payload: payload, Completeness: Complete}, nil
}

func testWorkflow() WorkflowDefinition {
	report := agentapi.SchemaRef{ID: "review.report", Version: 1}
	return WorkflowDefinition{
		ID: "delivery.review", Version: 1, Purpose: "Run independent reviewers.",
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: report,
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
				InputSchema: report, OutputSchema: report,
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
