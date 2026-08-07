package agentworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestServiceResumesRetryableCheckpointAtNextAttempt(t *testing.T) {
	definition := recoveryWorkflow()
	executor := &scriptedWorkflowExecutor{}
	service, persistence := newRecoveryService(
		t,
		"workflow_recovery_retry",
		definition,
		executor,
	)
	now := time.Now().UTC()
	endedAt := now.Add(-time.Second)
	persistence.state.Nodes["review.a"] = NodeRunRecord{
		WorkflowRunID:  "workflow_recovery_retry",
		NodeID:         "review.a",
		Attempt:        1,
		Kind:           NodeAgent,
		Status:         RunFailed,
		ErrorCode:      nodeRetryableErrorCode,
		StartedAt:      now.Add(-2 * time.Second),
		FirstStartedAt: now.Add(-2 * time.Second),
		EndedAt:        &endedAt,
	}

	result, err := service.Resume(t.Context(), "workflow_recovery_retry")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.Status != RunSucceeded || result.Result == nil {
		t.Fatalf("resume result = %+v", result)
	}
	if attempts := executor.Attempts(); len(attempts) != 1 || attempts[0] != 2 {
		t.Fatalf("executor attempts = %v, want [2]", attempts)
	}
}

func TestServiceResumeHonorsPersistedWorkflowUsage(t *testing.T) {
	definition := recoveryWorkflow()
	definition.Budget.MaxInputTokens = 10
	definition.Nodes[0].Budget.MaxInputTokens = 10
	executor := &scriptedWorkflowExecutor{}
	service, persistence := newRecoveryService(
		t,
		"workflow_recovery_budget",
		definition,
		executor,
	)
	persistence.state.Run.Usage.InputTokens = 10
	persistence.state.Run.Usage.TotalTokens = 10

	result, err := service.Resume(t.Context(), "workflow_recovery_budget")
	if !errors.Is(err, ErrWorkflowBudgetExhausted) {
		t.Fatalf("Resume error = %v, want workflow budget exhaustion", err)
	}
	if result.Status != RunFailed {
		t.Fatalf("resume status = %s, want failed", result.Status)
	}
	if attempts := executor.Attempts(); len(attempts) != 0 {
		t.Fatalf("executor attempts = %v, want none", attempts)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.startedNodes) != 0 ||
		persistence.finishedError != "workflow_budget_exhausted" {
		t.Fatalf(
			"started nodes=%d finished error=%q",
			len(persistence.startedNodes),
			persistence.finishedError,
		)
	}
}

func TestServiceTakesOverRunningReadOnlyAgentAttempt(t *testing.T) {
	definition := recoveryWorkflow()
	definition.Budget.MaxInputTokens = 20
	definition.Budget.MaxOutputTokens = 10
	definition.Budget.MaxToolCalls = 4
	definition.Budget.MaxRetries = 1
	definition.Nodes[0].Budget = NodeBudget{
		MaxInputTokens: 10, MaxOutputTokens: 5, MaxToolCalls: 2,
	}
	executor := &scriptedWorkflowExecutor{}
	service, persistence := newRecoveryService(
		t,
		"workflow_recovery_running",
		definition,
		executor,
	)
	startedAt := time.Now().UTC().Add(-time.Second)
	persistence.state.Nodes["review.a"] = NodeRunRecord{
		WorkflowRunID:  "workflow_recovery_running",
		NodeID:         "review.a",
		Attempt:        1,
		Kind:           NodeAgent,
		Status:         RunRunning,
		StartedAt:      startedAt,
		FirstStartedAt: startedAt,
	}

	result, err := service.Resume(t.Context(), "workflow_recovery_running")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunSucceeded {
		t.Fatalf("resume status = %s", result.Status)
	}
	if attempts := executor.Attempts(); len(attempts) != 1 || attempts[0] != 2 {
		t.Fatalf("executor attempts = %v, want [2]", attempts)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.failedNodes) != 1 ||
		persistence.failedNodes[0].attempt != 1 ||
		persistence.failedNodes[0].errorCode != nodeRestartedRetryableErrorCode ||
		persistence.failedNodes[0].usage != (WorkflowUsage{
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15, ToolCalls: 2,
		}) {
		t.Fatalf("takeover transitions = %+v", persistence.failedNodes)
	}
	if persistence.state.Run.Usage != (WorkflowUsage{
		InputTokens: 10, OutputTokens: 5, TotalTokens: 15, ToolCalls: 2, Retries: 1,
	}) {
		t.Fatalf("restored workflow usage = %+v", persistence.state.Run.Usage)
	}
}

func TestServiceTakesOverRunningRetrySafeTransformAttempt(t *testing.T) {
	definition := recoveryWorkflow()
	definition.ID = "delivery.recovery.transform"
	definition.Nodes[0].Kind = NodeTransform
	definition.Nodes[0].Agent = agentapi.DefinitionRef{}
	definition.Nodes[0].TransformID = "feature.pipeline.generate"
	definition.Nodes[0].RetrySafe = true
	executor := &scriptedWorkflowExecutor{}
	service, persistence := newRecoveryService(
		t,
		"workflow_recovery_transform",
		definition,
		executor,
	)
	startedAt := time.Now().UTC().Add(-time.Second)
	persistence.state.Nodes["review.a"] = NodeRunRecord{
		WorkflowRunID:  "workflow_recovery_transform",
		NodeID:         "review.a",
		Attempt:        1,
		Kind:           NodeTransform,
		Status:         RunRunning,
		StartedAt:      startedAt,
		FirstStartedAt: startedAt,
	}

	result, err := service.Resume(t.Context(), "workflow_recovery_transform")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != RunSucceeded {
		t.Fatalf("resume status = %s", result.Status)
	}
	if attempts := executor.Attempts(); len(attempts) != 1 || attempts[0] != 2 {
		t.Fatalf("executor attempts = %v, want [2]", attempts)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.failedNodes) != 1 ||
		persistence.failedNodes[0].attempt != 1 ||
		persistence.failedNodes[0].errorCode != nodeRestartedRetryableErrorCode {
		t.Fatalf("takeover transitions = %+v", persistence.failedNodes)
	}
}

func TestServiceDoesNotRetryUnsafeRunningAttempt(t *testing.T) {
	tests := []struct {
		name       string
		definition func() WorkflowDefinition
	}{
		{
			name: "write agent",
			definition: func() WorkflowDefinition {
				definition := recoveryWorkflow()
				write := agentapi.PermissionPolicy{
					Scopes: []string{"knowledge.read", "knowledge.write"},
				}
				definition.Permissions = write
				definition.Nodes[0].Permissions = write
				return definition
			},
		},
		{
			name: "transform",
			definition: func() WorkflowDefinition {
				definition := recoveryWorkflow()
				definition.Nodes[0].Kind = NodeTransform
				definition.Nodes[0].Agent = agentapi.DefinitionRef{}
				definition.Nodes[0].TransformID = "review.transform"
				return definition
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := test.definition()
			executor := &scriptedWorkflowExecutor{}
			runID := "workflow_recovery_unsafe"
			agents := testAgentDefinitions(t)
			if test.name == "write agent" {
				ref := definition.Nodes[0].Agent
				agentDefinition := agents.definitions[ref]
				agentDefinition.ContentHash = ""
				agentDefinition.Tools.AllowWrite = true
				agentDefinition.Permissions = definition.Nodes[0].Permissions
				prepared, err := agentapi.Prepare(agentDefinition)
				if err != nil {
					t.Fatal(err)
				}
				agents.definitions[ref] = prepared
			}
			service, persistence := newRecoveryServiceWithAgents(
				t,
				runID,
				definition,
				executor,
				agents,
			)
			startedAt := time.Now().UTC().Add(-time.Second)
			persistence.state.Nodes["review.a"] = NodeRunRecord{
				WorkflowRunID:  runID,
				NodeID:         "review.a",
				Attempt:        1,
				Kind:           definition.Nodes[0].Kind,
				Status:         RunRunning,
				StartedAt:      startedAt,
				FirstStartedAt: startedAt,
			}

			result, err := service.Resume(t.Context(), runID)
			if err == nil {
				t.Fatal("Resume succeeded for an unsafe running attempt")
			}
			if result.Status != RunFailed {
				t.Fatalf("resume status = %s, want failed", result.Status)
			}
			if attempts := executor.Attempts(); len(attempts) != 0 {
				t.Fatalf("executor attempts = %v, want none", attempts)
			}
			persistence.mu.Lock()
			defer persistence.mu.Unlock()
			if len(persistence.failedNodes) != 1 ||
				persistence.failedNodes[0].errorCode != nodeRestartedErrorCode {
				t.Fatalf("takeover transitions = %+v", persistence.failedNodes)
			}
		})
	}
}

func TestServiceRestoresRunningHumanApprovalAsWaiting(t *testing.T) {
	definition := recoveryWorkflow()
	definition.ID = "delivery.recovery.approval"
	definition.OutputSchema = definition.InputSchema
	definition.Nodes[0] = NodeDefinition{
		ID:           "review.a",
		Kind:         NodeHumanApproval,
		InputSchema:  definition.InputSchema,
		OutputSchema: definition.InputSchema,
		Permissions:  definition.Permissions,
		Retry:        RetryPolicy{MaxAttempts: 2},
		Timeout:      5 * time.Second,
	}
	service, persistence := newRecoveryService(
		t,
		"workflow_recovery_human",
		definition,
		nil,
	)
	startedAt := time.Now().UTC().Add(-time.Second)
	persistence.state.Nodes["review.a"] = NodeRunRecord{
		WorkflowRunID:  "workflow_recovery_human",
		NodeID:         "review.a",
		Attempt:        1,
		Kind:           NodeHumanApproval,
		Status:         RunRunning,
		StartedAt:      startedAt,
		FirstStartedAt: startedAt,
	}

	result, err := service.Resume(t.Context(), "workflow_recovery_human")
	if !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("Resume error = %v", err)
	}
	if result.Status != RunWaitingHuman {
		t.Fatalf("resume status = %s, want waiting_human", result.Status)
	}
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.failedNodes) != 1 ||
		persistence.failedNodes[0].status != RunWaitingHuman {
		t.Fatalf("human takeover transitions = %+v", persistence.failedNodes)
	}
}

func TestServiceDoesNotResumeExhaustedAttempt(t *testing.T) {
	definition := recoveryWorkflow()
	executor := &scriptedWorkflowExecutor{}
	service, persistence := newRecoveryService(
		t,
		"workflow_recovery_exhausted",
		definition,
		executor,
	)
	now := time.Now().UTC()
	endedAt := now.Add(-time.Second)
	persistence.state.Nodes["review.a"] = NodeRunRecord{
		WorkflowRunID:  "workflow_recovery_exhausted",
		NodeID:         "review.a",
		Attempt:        2,
		Kind:           NodeAgent,
		Status:         RunFailed,
		ErrorCode:      nodeRetryableErrorCode,
		StartedAt:      now.Add(-2 * time.Second),
		FirstStartedAt: now.Add(-3 * time.Second),
		EndedAt:        &endedAt,
	}

	result, err := service.Resume(t.Context(), "workflow_recovery_exhausted")
	if err == nil {
		t.Fatal("Resume succeeded after attempts were exhausted")
	}
	if result.Status != RunFailed {
		t.Fatalf("resume status = %s, want failed", result.Status)
	}
	if attempts := executor.Attempts(); len(attempts) != 0 {
		t.Fatalf("executor attempts = %v, want none", attempts)
	}
}

func TestServiceResumeBackoffStopsOnCancellation(t *testing.T) {
	definition := recoveryWorkflow()
	definition.Nodes[0].Retry.Backoff = 30 * time.Second
	executor := &scriptedWorkflowExecutor{}
	service, persistence := newRecoveryService(
		t,
		"workflow_recovery_cancel",
		definition,
		executor,
	)
	now := time.Now().UTC()
	persistence.state.Nodes["review.a"] = NodeRunRecord{
		WorkflowRunID:  "workflow_recovery_cancel",
		NodeID:         "review.a",
		Attempt:        1,
		Kind:           NodeAgent,
		Status:         RunFailed,
		ErrorCode:      nodeRetryableErrorCode,
		StartedAt:      now.Add(-time.Second),
		FirstStartedAt: now.Add(-time.Second),
		EndedAt:        &now,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	result, err := service.Resume(ctx, "workflow_recovery_cancel")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Resume error = %v, want context deadline", err)
	}
	if result.Status != RunTimedOut {
		t.Fatalf("resume status = %s, want timed_out", result.Status)
	}
	if attempts := executor.Attempts(); len(attempts) != 0 {
		t.Fatalf("executor attempts = %v, want none", attempts)
	}
}

func TestServiceResumeKeepsNodeTimeoutAcrossAttempts(t *testing.T) {
	definition := recoveryWorkflow()
	definition.Nodes[0].Timeout = 100 * time.Millisecond
	executor := &scriptedWorkflowExecutor{}
	service, persistence := newRecoveryService(
		t,
		"workflow_recovery_node_timeout",
		definition,
		executor,
	)
	now := time.Now().UTC()
	endedAt := now.Add(-time.Second)
	persistence.state.Nodes["review.a"] = NodeRunRecord{
		WorkflowRunID:  "workflow_recovery_node_timeout",
		NodeID:         "review.a",
		Attempt:        1,
		Kind:           NodeAgent,
		Status:         RunFailed,
		ErrorCode:      nodeRetryableErrorCode,
		StartedAt:      now.Add(-2 * time.Second),
		FirstStartedAt: now.Add(-2 * time.Second),
		EndedAt:        &endedAt,
	}

	result, err := service.Resume(t.Context(), "workflow_recovery_node_timeout")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Resume error = %v, want node deadline", err)
	}
	if result.Status != RunTimedOut {
		t.Fatalf("resume status = %s, want timed_out", result.Status)
	}
	if attempts := executor.Attempts(); len(attempts) != 0 {
		t.Fatalf("executor attempts = %v, want none", attempts)
	}
}

func TestServiceResumeSingleFlightExecutesOneAttempt(t *testing.T) {
	definition := recoveryWorkflow()
	executor := &blockingRecoveryExecutor{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	service, _ := newRecoveryService(
		t,
		"workflow_recovery_singleflight",
		definition,
		executor,
	)
	type response struct {
		result ResumeResult
		err    error
	}
	responses := make(chan response, 2)
	resume := func() {
		result, err := service.Resume(t.Context(), "workflow_recovery_singleflight")
		responses <- response{result: result, err: err}
	}
	go resume()
	<-executor.started
	go resume()
	close(executor.release)

	for range 2 {
		response := <-responses
		if response.err != nil {
			t.Fatal(response.err)
		}
		if response.result.Status != RunSucceeded {
			t.Fatalf("resume result = %+v", response.result)
		}
	}
	if executor.Attempts() != 1 {
		t.Fatalf("executor attempts = %d, want 1", executor.Attempts())
	}
}

func TestServiceRecoverActiveScansStartupRuns(t *testing.T) {
	definition := recoveryWorkflow()
	executor := &scriptedWorkflowExecutor{}
	runID := "workflow_recovery_startup"
	service, persistence := newRecoveryService(t, runID, definition, executor)
	persistence.activeRuns = []ActiveRunRef{{
		ID: runID, StartedAt: persistence.state.Run.StartedAt,
	}}

	report, err := service.RecoverActive(
		t.Context(),
		time.Now().UTC(),
		10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 1 || report.Resumed != 1 || report.Succeeded != 1 ||
		report.Errors != 0 {
		t.Fatalf("recovery report = %+v", report)
	}
	if attempts := executor.Attempts(); len(attempts) != 1 || attempts[0] != 1 {
		t.Fatalf("executor attempts = %v, want [1]", attempts)
	}
}

func TestServiceRecoverActiveStreamsObserverErrors(t *testing.T) {
	definition := recoveryWorkflow()
	executor := &scriptedWorkflowExecutor{}
	runID := "workflow_recovery_observer"
	service, persistence := newRecoveryService(t, runID, definition, executor)
	persistence.activeRuns = []ActiveRunRef{{
		ID: runID, StartedAt: persistence.state.Run.StartedAt,
	}}
	var observed ResumeResult
	report, err := service.RecoverActiveWithObserver(
		t.Context(),
		time.Now().UTC(),
		10,
		func(_ context.Context, observedRunID string, result ResumeResult, resumeErr error) error {
			if observedRunID != runID || resumeErr != nil {
				t.Fatalf("observed run = %q, resume error = %v", observedRunID, resumeErr)
			}
			observed = result
			return errors.New("domain reconciliation failed")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "domain reconciliation failed") {
		t.Fatalf("RecoverActiveWithObserver error = %v", err)
	}
	if observed.RunID != runID || observed.Status != RunSucceeded {
		t.Fatalf("observed result = %+v", observed)
	}
	if report.Scanned != 1 || report.Resumed != 1 || report.Succeeded != 1 ||
		report.Errors != 1 {
		t.Fatalf("recovery report = %+v", report)
	}
}

func recoveryWorkflow() WorkflowDefinition {
	definition := singleNodeWorkflow()
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	definition.ID = "delivery.recovery"
	definition.Permissions = readOnly
	definition.Nodes[0].Permissions = readOnly
	definition.Nodes[0].Retry = RetryPolicy{MaxAttempts: 2}
	definition.Nodes[0].Timeout = 5 * time.Second
	definition.Budget.Timeout = 10 * time.Second
	return definition
}

func newRecoveryService(
	t *testing.T,
	runID string,
	definition WorkflowDefinition,
	executor NodeExecutor,
) (*Service, *recordingWorkflowPersistence) {
	t.Helper()
	return newRecoveryServiceWithAgents(
		t,
		runID,
		definition,
		executor,
		testAgentDefinitions(t),
	)
}

func newRecoveryServiceWithAgents(
	t *testing.T,
	runID string,
	definition WorkflowDefinition,
	executor NodeExecutor,
	agents AgentDefinitionResolver,
) (*Service, *recordingWorkflowPersistence) {
	t.Helper()
	schemas := testSchemaRegistry(t)
	catalog := NewCatalog(schemas, agents)
	if err := catalog.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatal(err)
	}
	prepared, err := catalog.Resolve(DefinitionRef{
		ID: definition.ID, Version: definition.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-2 * time.Second)
	input, err := PrepareHandoff(Handoff{
		WorkflowRunID:  runID,
		ProducerNodeID: "workflow.input",
		Schema:         prepared.InputSchema,
		Payload:        json.RawMessage(`{"subject":"x"}`),
		Completeness:   Complete,
		CreatedAt:      startedAt,
	}, prepared.Budget.MaxHandoffBytes, schemas)
	if err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{
		state: WorkflowRunState{
			Run: WorkflowRunRecord{
				ID: runID, WorkflowID: prepared.ID, WorkflowVersion: prepared.Version,
				WorkflowHash: prepared.ContentHash,
				ActorUserID:  41, ActorTenantID: "tenant-a",
				ActorPermissions:    prepared.Permissions,
				ScenarioPermissions: prepared.Permissions,
				Status:              RunRunning,
				Budget:              prepared.Budget,
				StartedAt:           startedAt,
			},
			Input:       input,
			Nodes:       make(map[string]NodeRunRecord),
			Handoffs:    map[string]Handoff{input.ID: input},
			NodeOutputs: make(map[string]Handoff),
			Gates:       make(map[string]GateDecision),
			Approvals:   make(map[string]WorkflowApproval),
		},
	}
	service, err := NewService(
		catalog,
		persistence,
		NewOrchestrator(schemas, executor, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	return service, persistence
}

type blockingRecoveryExecutor struct {
	once     sync.Once
	mu       sync.Mutex
	attempts int
	started  chan struct{}
	release  chan struct{}
}

func (executor *blockingRecoveryExecutor) Execute(
	ctx context.Context,
	request NodeRequest,
) (NodeResult, error) {
	executor.mu.Lock()
	executor.attempts++
	executor.mu.Unlock()
	executor.once.Do(func() { close(executor.started) })
	select {
	case <-ctx.Done():
		return NodeResult{}, ctx.Err()
	case <-executor.release:
	}
	payload, _ := json.Marshal(map[string]string{"node": request.Node.ID})
	return NodeResult{
		Handoff: Handoff{Payload: payload, Completeness: Complete},
	}, nil
}

func (executor *blockingRecoveryExecutor) Attempts() int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.attempts
}
