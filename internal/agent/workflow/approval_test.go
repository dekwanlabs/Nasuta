package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestServiceApprovalResumesOnlyIncompleteNodesWithOriginalSnapshot(t *testing.T) {
	executor := &approvalRecordingExecutor{}
	service, persistence, runID := startApprovalWorkflow(t, approvalResumeWorkflow(), executor)

	result, err := service.DecideHumanApproval(t.Context(), ApprovalRequest{
		WorkflowRunID: runID,
		NodeID:        "approve",
		Decision:      ApprovalApproved,
		Approver:      agentapi.Actor{UserID: 99, TenantID: "tenant-other"},
		Admin:         true,
		Comment:       "approved",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Status != RunSucceeded || result.Result == nil {
		t.Fatalf("approval result = %+v", result)
	}
	requests := executor.Requests()
	if len(requests) != 2 ||
		requests[0].Node.ID != "review.before" ||
		requests[1].Node.ID != "review.after" {
		t.Fatalf("executed nodes = %v", nodeRequestIDs(requests))
	}
	resumed := requests[1]
	if resumed.Actor.UserID != 41 || resumed.Actor.TenantID != "tenant-a" {
		t.Fatalf("resumed actor = %+v", resumed.Actor)
	}
	if len(resumed.EffectivePermissions.Scopes) != 1 ||
		resumed.EffectivePermissions.Scopes[0] != "knowledge.read" {
		t.Fatalf("resumed permissions = %v", resumed.EffectivePermissions.Scopes)
	}

	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	counts := make(map[string]int, len(persistence.startedNodes))
	for _, node := range persistence.startedNodes {
		counts[node.NodeID]++
		if node.Attempt != 1 {
			t.Fatalf("node %q attempt = %d", node.NodeID, node.Attempt)
		}
	}
	for _, nodeID := range []string{"review.before", "approve", "review.after"} {
		if counts[nodeID] != 1 {
			t.Fatalf("node start counts = %v", counts)
		}
	}
}

func TestServiceApprovalIsIdempotentAndRejectsConflictingDecision(t *testing.T) {
	executor := &approvalRecordingExecutor{}
	service, _, runID := startApprovalWorkflow(t, approvalResumeWorkflow(), executor)
	request := ApprovalRequest{
		WorkflowRunID: runID,
		NodeID:        "approve",
		Decision:      ApprovalApproved,
		Approver:      agentapi.Actor{UserID: 99, TenantID: "tenant-a"},
		Admin:         true,
	}
	first, err := service.DecideHumanApproval(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.DecideHumanApproval(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Applied || second.Applied || second.Status != RunSucceeded {
		t.Fatalf("approval results: first=%+v second=%+v", first, second)
	}
	if got := nodeRequestIDs(executor.Requests()); len(got) != 2 {
		t.Fatalf("executed nodes after idempotent command = %v", got)
	}

	request.Decision = ApprovalRejected
	if _, err := service.DecideHumanApproval(t.Context(), request); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("conflicting approval error = %v", err)
	}
}

func TestServiceApprovalClassifiesRequestAuthorizationAndStateErrors(t *testing.T) {
	tests := []struct {
		name    string
		request ApprovalRequest
		mutate  func(*recordingWorkflowPersistence)
		want    error
	}{
		{
			name: "invalid run id",
			request: ApprovalRequest{
				WorkflowRunID: "INVALID", NodeID: "approve",
				Decision: ApprovalApproved, Approver: agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
			},
			want: ErrInvalid,
		},
		{
			name: "invalid node id",
			request: ApprovalRequest{
				NodeID: "INVALID", Decision: ApprovalApproved,
				Approver: agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
			},
			want: ErrInvalid,
		},
		{
			name: "invalid decision",
			request: ApprovalRequest{
				NodeID: "approve", Decision: "maybe",
				Approver: agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
			},
			want: ErrInvalid,
		},
		{
			name: "wrong tenant",
			request: ApprovalRequest{
				NodeID: "approve", Decision: ApprovalApproved,
				Approver: agentapi.Actor{UserID: 41, TenantID: "tenant-b"},
			},
			want: ErrForbidden,
		},
		{
			name: "wrong owner",
			request: ApprovalRequest{
				NodeID: "approve", Decision: ApprovalApproved,
				Approver: agentapi.Actor{UserID: 42, TenantID: "tenant-a"},
			},
			want: ErrForbidden,
		},
		{
			name: "unknown definition node",
			request: ApprovalRequest{
				NodeID: "unknown", Decision: ApprovalApproved,
				Approver: agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
			},
			want: ErrNotFound,
		},
		{
			name: "non approval node",
			request: ApprovalRequest{
				NodeID: "review.before", Decision: ApprovalApproved,
				Approver: agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
			},
			want: ErrConflict,
		},
		{
			name: "run not waiting",
			request: ApprovalRequest{
				NodeID: "approve", Decision: ApprovalApproved,
				Approver: agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
			},
			mutate: func(persistence *recordingWorkflowPersistence) {
				persistence.state.Run.Status = RunRunning
			},
			want: ErrConflict,
		},
		{
			name: "approval node missing",
			request: ApprovalRequest{
				NodeID: "approve", Decision: ApprovalApproved,
				Approver: agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
			},
			mutate: func(persistence *recordingWorkflowPersistence) {
				delete(persistence.state.Nodes, "approve")
			},
			want: ErrNotFound,
		},
		{
			name: "approval node not waiting",
			request: ApprovalRequest{
				NodeID: "approve", Decision: ApprovalApproved,
				Approver: agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
			},
			mutate: func(persistence *recordingWorkflowPersistence) {
				node := persistence.state.Nodes["approve"]
				node.Status = RunRunning
				persistence.state.Nodes["approve"] = node
			},
			want: ErrConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, persistence, runID := startApprovalWorkflow(
				t,
				approvalResumeWorkflow(),
				&approvalRecordingExecutor{},
			)
			test.request.WorkflowRunID = firstNonEmpty(test.request.WorkflowRunID, runID)
			if test.mutate != nil {
				persistence.mu.Lock()
				test.mutate(persistence)
				persistence.mu.Unlock()
			}
			_, err := service.DecideHumanApproval(t.Context(), test.request)
			if !errors.Is(err, test.want) {
				t.Fatalf("DecideHumanApproval error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestServiceApprovalReportsUnavailableExecution(t *testing.T) {
	service, _, _ := startApprovalWorkflow(
		t,
		approvalResumeWorkflow(),
		&approvalRecordingExecutor{},
	)
	service.SetOrchestrator(nil)
	_, err := service.DecideHumanApproval(t.Context(), ApprovalRequest{
		WorkflowRunID: "workflow_1",
		NodeID:        "approve",
		Decision:      ApprovalApproved,
		Approver:      agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("DecideHumanApproval error = %v", err)
	}
}

func TestServiceApprovalCanRecoverAfterCheckpointLoadFailure(t *testing.T) {
	executor := &approvalRecordingExecutor{}
	service, persistence, runID := startApprovalWorkflow(t, approvalResumeWorkflow(), executor)
	checkpointErr := errors.New("checkpoint unavailable")
	persistence.mu.Lock()
	persistence.loadStateErrs = []error{nil, checkpointErr}
	persistence.mu.Unlock()
	request := ApprovalRequest{
		WorkflowRunID: runID,
		NodeID:        "approve",
		Decision:      ApprovalApproved,
		Approver:      agentapi.Actor{UserID: 99, TenantID: "tenant-a"},
		Admin:         true,
	}

	first, err := service.DecideHumanApproval(t.Context(), request)
	if !errors.Is(err, checkpointErr) {
		t.Fatalf("first approval error = %v", err)
	}
	if !first.Applied || first.Status != RunRunning {
		t.Fatalf("first approval result = %+v", first)
	}
	if got := nodeRequestIDs(executor.Requests()); len(got) != 1 || got[0] != "review.before" {
		t.Fatalf("nodes before recovery = %v", got)
	}

	recovered, err := service.DecideHumanApproval(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Applied || recovered.Status != RunSucceeded || recovered.Result == nil {
		t.Fatalf("recovered approval = %+v", recovered)
	}
	if got := nodeRequestIDs(executor.Requests()); len(got) != 2 ||
		got[0] != "review.before" || got[1] != "review.after" {
		t.Fatalf("nodes after recovery = %v", got)
	}
}

func firstNonEmpty(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func TestServiceRejectedApprovalDoesNotResumeWorkflow(t *testing.T) {
	executor := &approvalRecordingExecutor{}
	service, persistence, runID := startApprovalWorkflow(t, approvalResumeWorkflow(), executor)
	request := ApprovalRequest{
		WorkflowRunID: runID,
		NodeID:        "approve",
		Decision:      ApprovalRejected,
		Approver:      agentapi.Actor{UserID: 99, TenantID: "tenant-a"},
		Admin:         true,
		Comment:       "not approved",
	}
	result, err := service.DecideHumanApproval(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Status != RunFailed || result.Result != nil {
		t.Fatalf("rejected approval = %+v", result)
	}
	if got := nodeRequestIDs(executor.Requests()); len(got) != 1 || got[0] != "review.before" {
		t.Fatalf("executed nodes = %v", got)
	}

	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if persistence.state.Run.Status != RunFailed ||
		persistence.state.Run.ErrorCode != "human_approval_rejected" {
		t.Fatalf("run state = %+v", persistence.state.Run)
	}
	if persistence.state.Nodes["approve"].Status != RunFailed {
		t.Fatalf("approval node = %+v", persistence.state.Nodes["approve"])
	}
}

func TestServiceApprovesParallelHumanNodesWithoutRestartingAttempts(t *testing.T) {
	service, persistence, runID := startApprovalWorkflow(t, parallelApprovalWorkflow(), nil)
	approve := func(nodeID string) ApprovalResult {
		t.Helper()
		result, err := service.DecideHumanApproval(t.Context(), ApprovalRequest{
			WorkflowRunID: runID,
			NodeID:        nodeID,
			Decision:      ApprovalApproved,
			Approver:      agentapi.Actor{UserID: 99, TenantID: "tenant-a"},
			Admin:         true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	first := approve("approve.a")
	if !first.Applied || first.Status != RunWaitingHuman || first.Result != nil {
		t.Fatalf("first approval = %+v", first)
	}
	persistence.mu.Lock()
	if len(persistence.startedNodes) != 2 {
		started := append([]NodeRunRecord(nil), persistence.startedNodes...)
		persistence.mu.Unlock()
		t.Fatalf("started nodes after first approval = %+v", started)
	}
	persistence.mu.Unlock()

	second := approve("approve.b")
	if !second.Applied || second.Status != RunSucceeded || second.Result == nil {
		t.Fatalf("second approval = %+v", second)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.startedNodes) != 2 {
		t.Fatalf("started nodes = %+v", persistence.startedNodes)
	}
	counts := make(map[string]int, 2)
	for _, node := range persistence.startedNodes {
		counts[node.NodeID]++
		if node.Attempt != 1 {
			t.Fatalf("node %q attempt = %d", node.NodeID, node.Attempt)
		}
	}
	if counts["approve.a"] != 1 || counts["approve.b"] != 1 {
		t.Fatalf("node start counts = %v", counts)
	}
}

func TestResumeObservedSkipsSucceededNodes(t *testing.T) {
	schemas := approvalTestSchemas(t)
	definition := approvalResumeWorkflow()
	executor := &approvalRecordingExecutor{}
	orchestrator := NewOrchestrator(schemas, executor, nil)
	runID := "workflow_resume_1"
	startedAt := time.Now().UTC()
	input := prepareTestHandoff(t, schemas, definition, Handoff{
		WorkflowRunID: runID, ProducerNodeID: "workflow.input",
		Schema: definition.InputSchema, Payload: json.RawMessage(`{"subject":"x"}`),
		Completeness: Complete,
	})
	before := prepareTestHandoff(t, schemas, definition, Handoff{
		WorkflowRunID: runID, ProducerNodeID: "review.before",
		Schema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		Payload: json.RawMessage(`{"node":"review.before"}`), Completeness: Complete,
	})
	approved := prepareTestHandoff(t, schemas, definition, Handoff{
		WorkflowRunID: runID, ProducerNodeID: "approve",
		Schema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		Payload: json.RawMessage(`{"node":"review.before"}`), Completeness: Complete,
	})
	result, err := orchestrator.ResumeObserved(t.Context(), definition, RunRequest{
		RunID: runID, StartedAt: startedAt,
		ActorPermissions:    agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		ScenarioPermissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	}, Progress{
		StartedAt: startedAt,
		Input:     input,
		NodeOutputs: map[string]Handoff{
			"review.before": before,
			"approve":       approved,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output.ProducerNodeID != "workflow.output" {
		t.Fatalf("workflow output = %+v", result.Output)
	}
	if got := nodeRequestIDs(executor.Requests()); len(got) != 1 || got[0] != "review.after" {
		t.Fatalf("executed nodes = %v", got)
	}
}

func TestRunObservedWaitsForEveryHumanNodeInReadyWave(t *testing.T) {
	schemas := approvalTestSchemas(t)
	definition := parallelApprovalWorkflow()
	observer := &approvalRecordingObserver{
		started: make(map[string]int),
		failed:  make(map[string]int),
	}
	orchestrator := NewOrchestrator(schemas, nil, nil)
	_, err := orchestrator.RunObserved(t.Context(), definition, RunRequest{
		RunID: "workflow_parallel_1", Input: json.RawMessage(`{"subject":"x"}`),
	}, observer)
	if !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("RunObserved error = %v", err)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	for _, nodeID := range []string{"approve.a", "approve.b"} {
		if observer.started[nodeID] != 1 || observer.failed[nodeID] != 1 {
			t.Fatalf("observer state: started=%v failed=%v", observer.started, observer.failed)
		}
	}
}

func TestResumeObservedDoesNotRestartWaitingHumanNode(t *testing.T) {
	schemas := approvalTestSchemas(t)
	definition := parallelApprovalWorkflow()
	runID := "workflow_parallel_2"
	startedAt := time.Now().UTC()
	input := prepareTestHandoff(t, schemas, definition, Handoff{
		WorkflowRunID: runID, ProducerNodeID: "workflow.input",
		Schema: definition.InputSchema, Payload: json.RawMessage(`{"subject":"x"}`),
		Completeness: Complete,
	})
	approved := prepareTestHandoff(t, schemas, definition, Handoff{
		WorkflowRunID: runID, ProducerNodeID: "approve.a",
		Schema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		Payload: json.RawMessage(`{"subject":"x"}`), Completeness: Complete,
	})
	observer := &approvalRecordingObserver{
		started: make(map[string]int),
		failed:  make(map[string]int),
	}
	_, err := NewOrchestrator(schemas, nil, nil).ResumeObserved(
		t.Context(),
		definition,
		RunRequest{RunID: runID, StartedAt: startedAt},
		Progress{
			StartedAt:   startedAt,
			Input:       input,
			NodeOutputs: map[string]Handoff{"approve.a": approved},
			WaitingHuman: map[string]struct{}{
				"approve.b": {},
			},
		},
		observer,
	)
	if !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("ResumeObserved error = %v", err)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.started) != 0 || len(observer.failed) != 0 {
		t.Fatalf("waiting node restarted: started=%v failed=%v", observer.started, observer.failed)
	}
}

type approvalRecordingExecutor struct {
	mu       sync.Mutex
	requests []NodeRequest
}

func (executor *approvalRecordingExecutor) Execute(
	_ context.Context,
	request NodeRequest,
) (NodeResult, error) {
	executor.mu.Lock()
	executor.requests = append(executor.requests, request)
	executor.mu.Unlock()
	payload, err := json.Marshal(map[string]string{"node": request.Node.ID})
	if err != nil {
		return NodeResult{}, err
	}
	return NodeResult{
		AgentRunID: "agent_" + request.Node.ID,
		Handoff: Handoff{
			Payload: payload, Completeness: Complete,
		},
	}, nil
}

func (executor *approvalRecordingExecutor) Requests() []NodeRequest {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]NodeRequest(nil), executor.requests...)
}

type approvalRecordingObserver struct {
	mu      sync.Mutex
	started map[string]int
	failed  map[string]int
}

func (observer *approvalRecordingObserver) NodeStarted(
	_ context.Context,
	request NodeRequest,
) error {
	observer.mu.Lock()
	observer.started[request.Node.ID]++
	observer.mu.Unlock()
	return nil
}

func (*approvalRecordingObserver) NodeSucceeded(
	context.Context,
	NodeRequest,
	NodeResult,
	*GateDecision,
) error {
	return nil
}

func (observer *approvalRecordingObserver) NodeFailed(
	_ context.Context,
	request NodeRequest,
	_ NodeResult,
	err error,
) error {
	if !errors.Is(err, ErrHumanApprovalRequired) {
		return err
	}
	observer.mu.Lock()
	observer.failed[request.Node.ID]++
	observer.mu.Unlock()
	return nil
}

func startApprovalWorkflow(
	t *testing.T,
	definition Definition,
	executor NodeExecutor,
) (*Service, *recordingWorkflowPersistence, string) {
	t.Helper()
	schemas := approvalTestSchemas(t)
	agents := approvalTestAgentDefinitions(t)
	catalog := NewCatalog(schemas, agents)
	if err := catalog.Publish([]Definition{definition}); err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	service, err := NewService(catalog, persistence, NewOrchestrator(schemas, executor, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: definition.ID, Version: definition.Version},
		Input:    json.RawMessage(`{"subject":"x"}`),
		Actor:    agentapi.Actor{UserID: 41, TenantID: "tenant-a"},
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read", "knowledge.write"},
		},
		Scenario: "approval.test",
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("Execute error = %v", err)
	}
	persistence.mu.Lock()
	runID := persistence.state.Run.ID
	persistence.mu.Unlock()
	if runID == "" {
		t.Fatal("workflow run id was not persisted")
	}
	return service, persistence, runID
}

func approvalResumeWorkflow() Definition {
	subject := agentapi.SchemaRef{ID: "review.subject", Version: 1}
	report := agentapi.SchemaRef{ID: "review.report", Version: 1}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	return Definition{
		ID: "delivery.approval.resume", Version: 1,
		Purpose:     "Resume a review after an explicit approval.",
		InputSchema: subject, OutputSchema: report, Permissions: readOnly,
		Budget: Budget{
			MaxNodes: 3, MaxParallelism: 1, MaxRounds: 1, MaxDepth: 3,
			Timeout: 5 * time.Second, MaxHandoffBytes: 4096,
		},
		FailurePolicy: FailurePolicy{Mode: FailFast},
		Nodes: []NodeDefinition{
			{
				ID: "review.before", Kind: NodeAgent,
				Agent:       agentapi.DefinitionRef{ID: "review.correctness", Version: 1},
				InputSchema: subject, OutputSchema: report,
				Permissions: readOnly, Timeout: time.Second,
			},
			{
				ID: "approve", Kind: NodeHumanApproval,
				InputSchema: report, OutputSchema: report,
				Permissions: readOnly, Timeout: time.Second,
			},
			{
				ID: "review.after", Kind: NodeAgent,
				Agent:       agentapi.DefinitionRef{ID: "review.followup", Version: 1},
				InputSchema: report, OutputSchema: report,
				Permissions: readOnly, Timeout: time.Second,
			},
		},
		Edges: []EdgeDefinition{
			{From: "review.before", To: "approve", Required: true},
			{From: "approve", To: "review.after", Required: true},
		},
	}
}

func parallelApprovalWorkflow() Definition {
	subject := agentapi.SchemaRef{ID: "review.subject", Version: 1}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	return Definition{
		ID: "delivery.approval.parallel", Version: 1,
		Purpose:      "Wait for independent approvals in one stable wave.",
		InputSchema:  subject,
		OutputSchema: agentapi.SchemaRef{ID: "review.subject.list", Version: 1},
		Permissions:  readOnly,
		Budget: Budget{
			MaxNodes: 2, MaxParallelism: 2, MaxRounds: 1, MaxDepth: 1,
			Timeout: 5 * time.Second, MaxHandoffBytes: 4096,
		},
		FailurePolicy: FailurePolicy{Mode: FailFast},
		Nodes: []NodeDefinition{
			{
				ID: "approve.a", Kind: NodeHumanApproval,
				InputSchema: subject, OutputSchema: subject,
				Permissions: readOnly, Timeout: time.Second,
			},
			{
				ID: "approve.b", Kind: NodeHumanApproval,
				InputSchema: subject, OutputSchema: subject,
				Permissions: readOnly, Timeout: time.Second,
			},
		},
	}
}

func approvalTestSchemas(t *testing.T) *agentapi.SchemaRegistry {
	t.Helper()
	schemas := testSchemaRegistry(t)
	if err := schemas.Publish([]agentapi.SchemaDefinition{{
		ID: "review.subject.list", Version: 1,
		Document: json.RawMessage(`{
			"type":"array",
			"items":{
				"type":"object",
				"required":["subject"],
				"properties":{"subject":{"type":"string","minLength":1}},
				"additionalProperties":false
			}
		}`),
	}}); err != nil {
		t.Fatal(err)
	}
	return schemas
}

func approvalTestAgentDefinitions(t *testing.T) *testAgentResolver {
	t.Helper()
	agents := testAgentDefinitions(t)
	definition, err := agentapi.Prepare(agentapi.Definition{
		ID: "review.followup", Version: 1, Purpose: "Review a prior report.",
		Prompt:       agentapi.PromptSpec{System: "Review the report.", Version: "v1"},
		InputSchema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "review.report", Version: 1},
		Model: agentapi.ModelPolicy{
			Provider: "openai", Model: "test", MaxOutputTokens: 256,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout: time.Minute, MaxSteps: 2, ContextTokens: 4096,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agents.definitions[agentapi.DefinitionRef{
		ID: definition.ID, Version: definition.Version,
	}] = definition
	return agents
}

func prepareTestHandoff(
	t *testing.T,
	schemas *agentapi.SchemaRegistry,
	definition Definition,
	handoff Handoff,
) Handoff {
	t.Helper()
	prepared, err := PrepareHandoff(
		handoff,
		definition.Budget.MaxHandoffBytes,
		schemas,
	)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func nodeRequestIDs(requests []NodeRequest) []string {
	ids := make([]string, 0, len(requests))
	for _, request := range requests {
		ids = append(ids, request.Node.ID)
	}
	return ids
}
