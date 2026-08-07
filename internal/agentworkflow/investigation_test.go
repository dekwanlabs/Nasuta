package agentworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agentcatalog"
)

func TestDefaultDelegatedInvestigationPinsStableReadOnlyDAG(t *testing.T) {
	schemas, agents := investigationCatalogs(t, 6)
	nodeTimeout := 40 * time.Second
	definition, err := DefaultDelegatedInvestigation(6, nodeTimeout)
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
		resolved.FailurePolicy.Mode != FailFast {
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
	}
	if !reflect.DeepEqual(gotNodes, wantNodes) {
		t.Fatalf("topological order = %v, want %v", gotNodes, wantNodes)
	}
	if !reflect.DeepEqual(resolved.Permissions.Scopes, []string{"knowledge.read"}) {
		t.Fatalf("workflow permissions = %v", resolved.Permissions.Scopes)
	}
	if len(resolved.Edges) != 4 {
		t.Fatalf("edges = %v", resolved.Edges)
	}
}

func TestDelegatedInvestigationRunsFourIndependentAgentsAndSynthesizesJoin(t *testing.T) {
	const version int64 = 8
	schemas, agents := investigationCatalogs(t, version)
	workflow, err := DefaultDelegatedInvestigation(version, time.Second)
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
	actor := agentapi.Actor{UserID: 23, TenantID: "tenant-a"}
	result, err := service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: DelegatedInvestigationID, Version: version},
		Input:    json.RawMessage(`{"question":"Why is checkout failing?"}`),
		Actor:    actor,
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
			request.Correlation.NodeID != wantNodes[agentID] || request.ToolScope.AllowWrite ||
			!reflect.DeepEqual(request.Permissions.Scopes, []string{"knowledge.read"}) {
			t.Fatalf("request for %q = %+v", agentID, request)
		}
		if _, duplicate := runIDs[request.RunID]; duplicate || request.RunID == "" {
			t.Fatalf("agent run id %q is empty or duplicated", request.RunID)
		}
		runIDs[request.RunID] = struct{}{}
	}
	join := result.NodeOutputs["evidence.join"]
	if !bytes.Equal(requests["synthesizer"].Input, join.Payload) {
		t.Fatalf("synthesizer input = %s, join = %s", requests["synthesizer"].Input, join.Payload)
	}
	var focuses []struct {
		Focus string `json:"focus"`
	}
	if err := json.Unmarshal(join.Payload, &focuses); err != nil {
		t.Fatal(err)
	}
	if got := []string{focuses[0].Focus, focuses[1].Focus, focuses[2].Focus}; !reflect.DeepEqual(got, []string{"code", "docs", "runtime"}) {
		t.Fatalf("join order = %v", got)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.startedNodes) != 5 || len(persistence.succeededNodes) != 5 ||
		persistence.finishedStatus != RunSucceeded {
		t.Fatalf("persisted lifecycle = started:%d succeeded:%d status:%s", len(persistence.startedNodes), len(persistence.succeededNodes), persistence.finishedStatus)
	}
}

type investigationRuntime struct {
	mu       sync.Mutex
	requests map[string]agentapi.RunRequest
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
	return agentapi.RunResult{Status: agentapi.RunSucceeded, Output: payload}, nil
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

func investigationCatalogs(t *testing.T, version int64) (*agentapi.SchemaRegistry, *agentcatalog.Catalog) {
	t.Helper()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(agentcatalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	settings := &config.PlatformSettings{
		LLMProvider: "openai", LLMModel: "test-model", LLMAnswerMaxTokens: 1024,
		LLMContextWindow: 16000, AgentTimeout: config.Duration(time.Minute), AgentMaxSteps: 3,
	}
	definitions, err := agentcatalog.DefaultInvestigatorsVersion(settings, version)
	if err != nil {
		t.Fatalf("prepare agents: %v", err)
	}
	agents := agentcatalog.New(schemas)
	if err := agents.Publish(definitions); err != nil {
		t.Fatalf("publish agents: %v", err)
	}
	return schemas, agents
}
