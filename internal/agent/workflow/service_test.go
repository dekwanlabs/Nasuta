package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestServiceExecutionAvailableTracksOrchestrator(t *testing.T) {
	var unavailable *Service
	if unavailable.ExecutionAvailable() {
		t.Fatal("nil service reported execution available")
	}

	schemas := testSchemaRegistry(t)
	service, err := NewService(
		NewCatalog(schemas, testAgentDefinitions(t)),
		&recordingWorkflowPersistence{},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if service.ExecutionAvailable() {
		t.Fatal("service without an orchestrator reported execution available")
	}
	service.SetOrchestrator(NewOrchestrator(schemas, staticOutputExecutor{}, nil))
	if !service.ExecutionAvailable() {
		t.Fatal("service with an orchestrator reported execution unavailable")
	}
	service.Close()
	if service.ExecutionAvailable() {
		t.Fatal("closed service reported execution available")
	}
}

func TestServicePersistsSuccessfulWorkflowLifecycle(t *testing.T) {
	schemas := testSchemaRegistry(t)
	catalog := NewCatalog(schemas, testAgentDefinitions(t))
	definition := singleNodeWorkflow()
	definition.Permissions = agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	definition.Nodes[0].Permissions = agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	if err := catalog.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	service, err := NewService(
		catalog,
		persistence,
		NewOrchestrator(schemas, staticOutputExecutor{}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(t.Context(), ExecuteRequest{
		ParentRunID: "qa_parent_1",
		Workflow:    DefinitionRef{ID: definition.ID, Version: definition.Version},
		Input:       json.RawMessage(`{"subject":"x"}`),
		Actor:       agentapi.Actor{UserID: 9, TenantID: "tenant-a"},
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		Scenario: "test.review",
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID == "" || result.Output.ID == "" {
		t.Fatalf("result = %+v", result)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if persistence.startedRun.ID != result.RunID ||
		persistence.startedRun.WorkflowHash == "" ||
		persistence.startedRun.InputHash == "" ||
		persistence.startedRun.ParentRunID != "qa_parent_1" ||
		persistence.startedRun.ActorUserID != 9 ||
		persistence.startedRun.Scenario != "test.review" {
		t.Fatalf("started run = %+v", persistence.startedRun)
	}
	if len(persistence.startedNodes) != 1 || len(persistence.succeededNodes) != 1 ||
		len(persistence.failedNodes) != 0 {
		t.Fatalf(
			"node transitions = started:%d succeeded:%d failed:%d",
			len(persistence.startedNodes),
			len(persistence.succeededNodes),
			len(persistence.failedNodes),
		)
	}
	if persistence.finishedStatus != RunSucceeded || persistence.finishedOutput == nil ||
		persistence.finishedOutput.ID != result.Output.ID {
		t.Fatalf("workflow terminal = %s output=%+v", persistence.finishedStatus, persistence.finishedOutput)
	}
}

func TestServiceUsesRequestedWorkflowRunID(t *testing.T) {
	schemas := testSchemaRegistry(t)
	catalog := NewCatalog(schemas, testAgentDefinitions(t))
	definition := singleNodeWorkflow()
	definition.Permissions = agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	definition.Nodes[0].Permissions = agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	if err := catalog.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	service, err := NewService(
		catalog,
		persistence,
		NewOrchestrator(schemas, staticOutputExecutor{}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Execute(t.Context(), ExecuteRequest{
		RunID:    "review_round-1",
		Workflow: DefinitionRef{ID: definition.ID, Version: definition.Version},
		Input:    json.RawMessage(`{"subject":"x"}`),
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RunID != "review_round-1" || persistence.startedRun.ID != result.RunID {
		t.Fatalf("result = %+v, started = %+v", result, persistence.startedRun)
	}
}

func TestServiceRejectsInvalidRequestedWorkflowRunID(t *testing.T) {
	schemas := testSchemaRegistry(t)
	catalog := NewCatalog(schemas, testAgentDefinitions(t))
	definition := singleNodeWorkflow()
	if err := catalog.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		catalog,
		&recordingWorkflowPersistence{},
		NewOrchestrator(schemas, staticOutputExecutor{}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(t.Context(), ExecuteRequest{
		RunID:    "Review Round 1",
		Workflow: DefinitionRef{ID: definition.ID, Version: definition.Version},
		Input:    json.RawMessage(`{"subject":"x"}`),
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Execute error = %v, want invalid", err)
	}
}

func TestServiceValidatesActorAndScenarioPermissionContracts(t *testing.T) {
	schemas := testSchemaRegistry(t)
	catalog := NewCatalog(schemas, testAgentDefinitions(t))
	definition := singleNodeWorkflow()
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	definition.Permissions = readOnly
	definition.Nodes[0].Permissions = readOnly
	if err := catalog.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	service, err := NewService(
		catalog,
		persistence,
		NewOrchestrator(schemas, staticOutputExecutor{}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		actor    agentapi.PermissionPolicy
		scenario agentapi.PermissionPolicy
		want     error
	}{
		{
			name:     "actor lacks workflow scope",
			scenario: readOnly,
			want:     ErrForbidden,
		},
		{
			name:  "scenario lacks workflow scope",
			actor: readOnly,
			want:  ErrForbidden,
		},
		{
			name:     "actor uses unknown scope",
			actor:    agentapi.PermissionPolicy{Scopes: []string{"approval.write"}},
			scenario: readOnly,
			want:     ErrInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, runErr := service.Execute(t.Context(), ExecuteRequest{
				Workflow:            DefinitionRef{ID: definition.ID, Version: definition.Version},
				Input:               json.RawMessage(`{"subject":"x"}`),
				ActorPermissions:    test.actor,
				ScenarioPermissions: test.scenario,
			})
			if !errors.Is(runErr, test.want) {
				t.Fatalf("Execute error = %v, want %v", runErr, test.want)
			}
		})
	}
	if persistence.startedRun.ID != "" {
		t.Fatalf("invalid permission request started run %+v", persistence.startedRun)
	}
}

func TestServicePersistsHumanApprovalAsWaiting(t *testing.T) {
	schemas := testSchemaRegistry(t)
	catalog := NewCatalog(schemas, testAgentDefinitions(t))
	definition := singleNodeWorkflow()
	definition.ID = "delivery.approval"
	definition.Nodes[0] = NodeDefinition{
		ID: "approve", Kind: NodeHumanApproval,
		InputSchema: definition.InputSchema, OutputSchema: definition.InputSchema,
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		Timeout:     time.Second,
	}
	definition.OutputSchema = definition.InputSchema
	definition.Permissions = agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	if err := catalog.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	service, err := NewService(catalog, persistence, NewOrchestrator(schemas, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: definition.ID, Version: 1},
		Input:    json.RawMessage(`{"subject":"x"}`),
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("Execute error = %v", err)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.failedNodes) != 1 ||
		persistence.failedNodes[0].status != RunWaitingHuman ||
		persistence.finishedStatus != RunWaitingHuman {
		t.Fatalf(
			"waiting transitions = nodes:%+v workflow:%s",
			persistence.failedNodes,
			persistence.finishedStatus,
		)
	}
}

func TestServiceClosesNodeWhenSuccessPersistenceFails(t *testing.T) {
	schemas := testSchemaRegistry(t)
	catalog := NewCatalog(schemas, testAgentDefinitions(t))
	definition := singleNodeWorkflow()
	definition.Permissions = agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	definition.Nodes[0].Permissions = agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	if err := catalog.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{succeedNodeErr: errors.New("write unavailable")}
	service, err := NewService(
		catalog,
		persistence,
		NewOrchestrator(schemas, staticOutputExecutor{}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: definition.ID, Version: definition.Version},
		Input:    json.RawMessage(`{"subject":"x"}`),
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	})
	if err == nil || !errors.Is(err, persistence.succeedNodeErr) {
		t.Fatalf("Execute error = %v", err)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.succeededNodes) != 0 || len(persistence.failedNodes) != 1 {
		t.Fatalf(
			"node transitions = succeeded:%d failed:%d",
			len(persistence.succeededNodes),
			len(persistence.failedNodes),
		)
	}
	if persistence.failedNodes[0].status != RunFailed ||
		persistence.failedNodes[0].errorCode != "node_persistence_failed" ||
		persistence.finishedStatus != RunFailed {
		t.Fatalf(
			"failure transitions = node:%+v workflow:%s",
			persistence.failedNodes[0],
			persistence.finishedStatus,
		)
	}
}

type nodeTerminalTransition struct {
	workflowRunID string
	nodeID        string
	attempt       int
	status        RunStatus
	errorCode     string
	agentRunID    string
	handoff       Handoff
	usage         WorkflowUsage
}

type recordingWorkflowPersistence struct {
	mu sync.Mutex

	startedRun     WorkflowRunRecord
	startedInput   Handoff
	activeRuns     []ActiveRunRef
	startedNodes   []NodeRunRecord
	succeededNodes []nodeTerminalTransition
	failedNodes    []nodeTerminalTransition
	finishedStatus RunStatus
	finishedOutput *Handoff
	finishedError  string
	succeedNodeErr error
	loadStateErrs  []error
	loadStateCalls int
	events         []Event
	state          WorkflowRunState
}

func (persistence *recordingWorkflowPersistence) StartWorkflow(
	_ context.Context,
	run WorkflowRunRecord,
	input Handoff,
) error {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	persistence.startedRun = cloneWorkflowRunRecord(run)
	persistence.startedInput = cloneHandoff(input)
	persistence.state = WorkflowRunState{
		Run:         cloneWorkflowRunRecord(run),
		Input:       cloneHandoff(input),
		Nodes:       make(map[string]NodeRunRecord),
		Handoffs:    map[string]Handoff{input.ID: cloneHandoff(input)},
		NodeOutputs: make(map[string]Handoff),
		Gates:       make(map[string]GateDecision),
		Approvals:   make(map[string]WorkflowApproval),
	}
	return nil
}

func (persistence *recordingWorkflowPersistence) StartNode(
	_ context.Context,
	run NodeRunRecord,
) error {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	cloned := cloneNodeRunRecord(run)
	if previous, ok := persistence.state.Nodes[run.NodeID]; ok &&
		!previous.FirstStartedAt.IsZero() {
		cloned.FirstStartedAt = previous.FirstStartedAt
	} else {
		cloned.FirstStartedAt = cloned.StartedAt
	}
	persistence.startedNodes = append(persistence.startedNodes, cloned)
	persistence.state.Nodes[run.NodeID] = cloned
	return nil
}

func (persistence *recordingWorkflowPersistence) SucceedNode(
	_ context.Context,
	workflowRunID string,
	nodeID string,
	attempt int,
	agentRunID string,
	handoff Handoff,
	decision *GateDecision,
	usage WorkflowUsage,
	endedAt time.Time,
) error {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if persistence.succeedNodeErr != nil {
		return persistence.succeedNodeErr
	}
	handoff = cloneHandoff(handoff)
	persistence.succeededNodes = append(persistence.succeededNodes, nodeTerminalTransition{
		workflowRunID: workflowRunID, nodeID: nodeID, status: RunSucceeded,
		attempt:    attempt,
		agentRunID: agentRunID, handoff: handoff,
		usage: usage,
	})
	node := persistence.state.Nodes[nodeID]
	node.Attempt = attempt
	node.AgentRunID = agentRunID
	node.OutputHandoffID = handoff.ID
	node.Status = RunSucceeded
	node.ErrorCode = ""
	node.Usage = usage
	ended := endedAt
	node.EndedAt = &ended
	persistence.state.Nodes[nodeID] = node
	persistence.state.Run.Usage = addWorkflowUsage(persistence.state.Run.Usage, usage)
	persistence.state.Handoffs[handoff.ID] = handoff
	persistence.state.NodeOutputs[nodeID] = handoff
	if decision != nil {
		persistence.state.Gates[nodeID] = cloneGateDecision(*decision)
	}
	return nil
}

func (persistence *recordingWorkflowPersistence) FailNode(
	_ context.Context,
	workflowRunID string,
	nodeID string,
	attempt int,
	agentRunID string,
	status RunStatus,
	errorCode string,
	usage WorkflowUsage,
	endedAt time.Time,
) error {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	persistence.failedNodes = append(persistence.failedNodes, nodeTerminalTransition{
		workflowRunID: workflowRunID, nodeID: nodeID, status: status,
		attempt:   attempt,
		errorCode: errorCode, agentRunID: agentRunID,
		usage: usage,
	})
	node := persistence.state.Nodes[nodeID]
	node.Attempt = attempt
	node.AgentRunID = agentRunID
	node.Status = status
	node.ErrorCode = errorCode
	node.Usage = usage
	ended := endedAt
	node.EndedAt = &ended
	persistence.state.Nodes[nodeID] = node
	persistence.state.Run.Usage = addWorkflowUsage(persistence.state.Run.Usage, usage)
	return nil
}

func (persistence *recordingWorkflowPersistence) FinishWorkflow(
	_ context.Context,
	workflowRunID string,
	status RunStatus,
	errorCode string,
	output *Handoff,
	endedAt time.Time,
) error {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	persistence.finishedStatus = status
	persistence.finishedError = errorCode
	persistence.state.Run.ID = workflowRunID
	persistence.state.Run.Status = status
	persistence.state.Run.ErrorCode = errorCode
	ended := endedAt
	persistence.state.Run.EndedAt = &ended
	persistence.finishedOutput = nil
	if output != nil {
		cloned := cloneHandoff(*output)
		persistence.finishedOutput = &cloned
		persistence.state.Handoffs[cloned.ID] = cloned
	}
	return nil
}

func (persistence *recordingWorkflowPersistence) LoadFullRunState(
	_ context.Context,
	workflowRunID string,
) (*WorkflowRunState, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	persistence.loadStateCalls++
	if len(persistence.loadStateErrs) > 0 {
		err := persistence.loadStateErrs[0]
		persistence.loadStateErrs = persistence.loadStateErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if persistence.state.Run.ID != workflowRunID {
		return nil, errors.New("workflow run not found")
	}
	state := cloneWorkflowRunState(persistence.state)
	return &state, nil
}

func (persistence *recordingWorkflowPersistence) GetRun(
	_ context.Context,
	workflowRunID string,
) (*WorkflowRunRecord, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if persistence.state.Run.ID != workflowRunID {
		return nil, ErrNotFound
	}
	run := cloneWorkflowRunRecord(persistence.state.Run)
	return &run, nil
}

func (persistence *recordingWorkflowPersistence) ListNodeRuns(
	_ context.Context,
	workflowRunID string,
	cursor NodeRunCursor,
	limit int,
) ([]NodeRunRecord, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	runs := make([]NodeRunRecord, 0, len(persistence.startedNodes))
	for _, run := range persistence.startedNodes {
		if run.WorkflowRunID != workflowRunID {
			continue
		}
		if cursor.NodeID != "" &&
			(run.NodeID < cursor.NodeID ||
				(run.NodeID == cursor.NodeID && run.Attempt <= cursor.Attempt)) {
			continue
		}
		runs = append(runs, cloneNodeRunRecord(run))
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].NodeID == runs[j].NodeID {
			return runs[i].Attempt < runs[j].Attempt
		}
		return runs[i].NodeID < runs[j].NodeID
	})
	if len(runs) > limit {
		runs = runs[:limit]
	}
	return runs, nil
}

func (persistence *recordingWorkflowPersistence) ListEvents(
	_ context.Context,
	workflowRunID string,
	afterSeq int64,
	limit int,
) ([]Event, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	events := make([]Event, 0, min(limit, len(persistence.events)))
	for _, event := range persistence.events {
		if event.WorkflowRunID != workflowRunID || event.Seq <= afterSeq {
			continue
		}
		events = append(events, event)
		if len(events) == limit {
			break
		}
	}
	return events, nil
}

func (persistence *recordingWorkflowPersistence) ListHandoffs(
	_ context.Context,
	workflowRunID string,
	cursor HandoffCursor,
	limit int,
) ([]Handoff, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	handoffs := make([]Handoff, 0, len(persistence.state.Handoffs))
	for _, handoff := range persistence.state.Handoffs {
		if handoff.WorkflowRunID != workflowRunID {
			continue
		}
		if cursor.ID != "" &&
			(handoff.CreatedAt.Before(cursor.CreatedAt) ||
				(handoff.CreatedAt.Equal(cursor.CreatedAt) && handoff.ID <= cursor.ID)) {
			continue
		}
		handoffs = append(handoffs, cloneHandoff(handoff))
	}
	sort.Slice(handoffs, func(i, j int) bool {
		if handoffs[i].CreatedAt.Equal(handoffs[j].CreatedAt) {
			return handoffs[i].ID < handoffs[j].ID
		}
		return handoffs[i].CreatedAt.Before(handoffs[j].CreatedAt)
	})
	if len(handoffs) > limit {
		handoffs = handoffs[:limit]
	}
	return handoffs, nil
}

func (persistence *recordingWorkflowPersistence) CancelWorkflow(
	_ context.Context,
	workflowRunID string,
	endedAt time.Time,
) (CancelTransition, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if persistence.state.Run.ID != workflowRunID {
		return CancelTransition{}, ErrNotFound
	}
	if persistence.state.Run.Status != RunRunning &&
		persistence.state.Run.Status != RunWaitingHuman {
		return CancelTransition{Status: persistence.state.Run.Status}, nil
	}
	persistence.state.Run.Status = RunCancelled
	persistence.state.Run.ErrorCode = "workflow_cancelled"
	persistence.state.Run.EndedAt = &endedAt
	for nodeID, run := range persistence.state.Nodes {
		if run.Status != RunRunning && run.Status != RunWaitingHuman {
			continue
		}
		run.Status = RunCancelled
		run.ErrorCode = "workflow_cancelled"
		run.EndedAt = &endedAt
		persistence.state.Nodes[nodeID] = run
	}
	return CancelTransition{Applied: true, Status: RunCancelled}, nil
}

func (persistence *recordingWorkflowPersistence) SubscribeEvents(
	string,
) (<-chan Event, func()) {
	return make(chan Event), func() {}
}

func (persistence *recordingWorkflowPersistence) ListActiveRuns(
	_ context.Context,
	startedBefore time.Time,
	cursor ActiveRunCursor,
	limit int,
) ([]ActiveRunRef, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	runs := make([]ActiveRunRef, 0, min(limit, len(persistence.activeRuns)))
	for _, run := range persistence.activeRuns {
		if !run.StartedAt.Before(startedBefore) {
			continue
		}
		if cursor.ID != "" &&
			(run.StartedAt.Before(cursor.StartedAt) ||
				(run.StartedAt.Equal(cursor.StartedAt) && run.ID <= cursor.ID)) {
			continue
		}
		runs = append(runs, run)
		if len(runs) == limit {
			break
		}
	}
	return runs, nil
}

func (persistence *recordingWorkflowPersistence) DecideHumanApproval(
	_ context.Context,
	approval WorkflowApproval,
	approvedHandoff *Handoff,
) (ApprovalTransition, error) {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if existing, ok := persistence.state.Approvals[approval.NodeID]; ok {
		if existing.Decision != approval.Decision {
			return ApprovalTransition{}, ErrApprovalConflict
		}
		return ApprovalTransition{
			Approval: existing, Applied: false, RunStatus: persistence.state.Run.Status,
		}, nil
	}
	if persistence.state.Run.Status != RunWaitingHuman {
		return ApprovalTransition{}, errors.New("workflow run is not waiting for approval")
	}
	node, ok := persistence.state.Nodes[approval.NodeID]
	if !ok || node.Kind != NodeHumanApproval || node.Status != RunWaitingHuman {
		return ApprovalTransition{}, errors.New("workflow node is not waiting for approval")
	}
	persistence.state.Approvals[approval.NodeID] = approval
	ended := approval.DecidedAt
	node.EndedAt = &ended
	switch approval.Decision {
	case ApprovalApproved:
		if approvedHandoff == nil {
			return ApprovalTransition{}, errors.New("approved workflow node requires a handoff")
		}
		handoff := cloneHandoff(*approvedHandoff)
		node.OutputHandoffID = handoff.ID
		node.Status = RunSucceeded
		node.ErrorCode = ""
		persistence.state.Nodes[approval.NodeID] = node
		persistence.state.Handoffs[handoff.ID] = handoff
		persistence.state.NodeOutputs[approval.NodeID] = handoff
		persistence.state.Run.Status = RunRunning
		persistence.state.Run.ErrorCode = ""
		persistence.state.Run.EndedAt = nil
	case ApprovalRejected:
		node.Status = RunFailed
		node.ErrorCode = "human_approval_rejected"
		persistence.state.Nodes[approval.NodeID] = node
		persistence.state.Run.Status = RunFailed
		persistence.state.Run.ErrorCode = "human_approval_rejected"
		persistence.state.Run.EndedAt = &ended
	default:
		return ApprovalTransition{}, errors.New("workflow approval decision is invalid")
	}
	return ApprovalTransition{
		Approval: approval, Applied: true, RunStatus: persistence.state.Run.Status,
	}, nil
}

func cloneWorkflowRunState(state WorkflowRunState) WorkflowRunState {
	cloned := WorkflowRunState{
		Run:         cloneWorkflowRunRecord(state.Run),
		Input:       cloneHandoff(state.Input),
		Nodes:       make(map[string]NodeRunRecord, len(state.Nodes)),
		Handoffs:    make(map[string]Handoff, len(state.Handoffs)),
		NodeOutputs: make(map[string]Handoff, len(state.NodeOutputs)),
		Gates:       make(map[string]GateDecision, len(state.Gates)),
		Approvals:   make(map[string]WorkflowApproval, len(state.Approvals)),
	}
	for nodeID, node := range state.Nodes {
		cloned.Nodes[nodeID] = cloneNodeRunRecord(node)
	}
	for id, handoff := range state.Handoffs {
		cloned.Handoffs[id] = cloneHandoff(handoff)
	}
	for nodeID, handoff := range state.NodeOutputs {
		cloned.NodeOutputs[nodeID] = cloneHandoff(handoff)
	}
	for nodeID, decision := range state.Gates {
		cloned.Gates[nodeID] = cloneGateDecision(decision)
	}
	for nodeID, approval := range state.Approvals {
		cloned.Approvals[nodeID] = approval
	}
	return cloned
}

func cloneWorkflowRunRecord(run WorkflowRunRecord) WorkflowRunRecord {
	run.ActorPermissions.Scopes = append([]string(nil), run.ActorPermissions.Scopes...)
	run.ScenarioPermissions.Scopes = append([]string(nil), run.ScenarioPermissions.Scopes...)
	if run.EndedAt != nil {
		ended := *run.EndedAt
		run.EndedAt = &ended
	}
	return run
}

func cloneNodeRunRecord(run NodeRunRecord) NodeRunRecord {
	run.InputHandoffIDs = append([]string(nil), run.InputHandoffIDs...)
	if run.EndedAt != nil {
		ended := *run.EndedAt
		run.EndedAt = &ended
	}
	return run
}

func cloneHandoff(handoff Handoff) Handoff {
	handoff.Payload = append(json.RawMessage(nil), handoff.Payload...)
	handoff.References = append([]agentapi.Reference(nil), handoff.References...)
	return handoff
}

func cloneGateDecision(decision GateDecision) GateDecision {
	decision.ReasonCodes = append([]string(nil), decision.ReasonCodes...)
	decision.FindingIDs = append([]string(nil), decision.FindingIDs...)
	return decision
}
