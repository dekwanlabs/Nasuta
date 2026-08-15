package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/scope"
)

func TestRetryPolicyNormalizesAndAffectsDefinitionHash(t *testing.T) {
	schemas := testSchemaRegistry(t)

	prepared, err := Prepare(singleNodeWorkflow(), schemas)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Nodes[0].Retry.MaxAttempts != 1 {
		t.Fatalf("default max attempts = %d, want 1", prepared.Nodes[0].Retry.MaxAttempts)
	}

	explicitDefault := singleNodeWorkflow()
	explicitDefault.Nodes[0].Retry.MaxAttempts = 1
	preparedExplicit, err := Prepare(explicitDefault, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ContentHash != preparedExplicit.ContentHash {
		t.Fatal("zero retry policy and explicit single attempt changed the definition hash")
	}

	withRetry := singleNodeWorkflow()
	withRetry.Nodes[0].Retry = RetryPolicy{MaxAttempts: 2, Backoff: time.Second}
	preparedRetry, err := Prepare(withRetry, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ContentHash == preparedRetry.ContentHash {
		t.Fatal("retry policy did not change the definition hash")
	}
}

func TestPrepareValidatesRetryPolicyBounds(t *testing.T) {
	tests := []struct {
		name   string
		policy RetryPolicy
	}{
		{name: "negative attempts", policy: RetryPolicy{MaxAttempts: -1}},
		{name: "too many attempts", policy: RetryPolicy{MaxAttempts: 11}},
		{name: "negative backoff", policy: RetryPolicy{Backoff: -time.Second}},
		{name: "too much backoff", policy: RetryPolicy{Backoff: 31 * time.Second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := singleNodeWorkflow()
			definition.Nodes[0].Retry = test.policy
			if _, err := Prepare(definition, testSchemaRegistry(t)); err == nil {
				t.Fatalf("Prepare accepted invalid retry policy %+v", test.policy)
			}
		})
	}
}

func TestOrchestratorRetriesClassifiedAgentFailure(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Nodes[0].Retry = RetryPolicy{MaxAttempts: 2}
	executor := &scriptedWorkflowExecutor{
		failAttempts: map[int]error{1: retryableWorkflowError{}},
	}
	observer := &recordingRunObserver{}
	orchestrator := NewOrchestrator(testSchemaRegistry(t), executor, nil)

	result, err := orchestrator.RunObserved(t.Context(), definition, RunRequest{
		RunID: "workflow_run_retry",
		Input: json.RawMessage(`{"subject":"x"}`),
	}, observer)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output.ID == "" {
		t.Fatal("successful retry produced no output")
	}
	if got := executor.Attempts(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("executor attempts = %v, want [1 2]", got)
	}
	if got := observer.Started(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("started attempts = %v, want [1 2]", got)
	}
	if got := observer.Failed(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("failed attempts = %v, want [1]", got)
	}
	if got := observer.Succeeded(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("succeeded attempts = %v, want [2]", got)
	}
}

func TestOrchestratorDoesNotRetryUnclassifiedFailure(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Nodes[0].Retry = RetryPolicy{MaxAttempts: 3}
	executor := &scriptedWorkflowExecutor{
		failAttempts: map[int]error{1: errors.New("permanent failure")},
	}
	observer := &recordingRunObserver{}

	_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		t.Context(), definition, RunRequest{
			RunID: "workflow_run_no_retry",
			Input: json.RawMessage(`{"subject":"x"}`),
		}, observer,
	)
	if err == nil {
		t.Fatal("workflow succeeded after an unclassified failure")
	}
	if got := executor.Attempts(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("executor attempts = %v, want [1]", got)
	}
}

func TestOrchestratorStopsAfterRetryAttemptsAreExhausted(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Nodes[0].Retry = RetryPolicy{MaxAttempts: 2}
	executor := &scriptedWorkflowExecutor{
		failAttempts: map[int]error{
			1: retryableWorkflowError{},
			2: retryableWorkflowError{},
		},
	}
	observer := &recordingRunObserver{}

	_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		t.Context(), definition, RunRequest{
			RunID: "workflow_run_exhausted",
			Input: json.RawMessage(`{"subject":"x"}`),
		}, observer,
	)
	if err == nil {
		t.Fatal("workflow succeeded after retry attempts were exhausted")
	}
	if got := executor.Attempts(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("executor attempts = %v, want [1 2]", got)
	}
	if got := observer.Failed(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("failed attempts = %v, want [1 2]", got)
	}
}

func TestOrchestratorRetryBackoffStopsOnContextCancellation(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Nodes[0].Retry = RetryPolicy{MaxAttempts: 2, Backoff: 30 * time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	executor := &scriptedWorkflowExecutor{
		failAttempts: map[int]error{1: retryableWorkflowError{}},
		cancel:       cancel,
	}

	_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		ctx, definition, RunRequest{
			RunID: "workflow_run_cancelled_backoff",
			Input: json.RawMessage(`{"subject":"x"}`),
		}, nil,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if got := executor.Attempts(); len(got) != 1 {
		t.Fatalf("executor attempts = %v, want one attempt", got)
	}
}

func TestOrchestratorDoesNotRetryHumanApproval(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.ID = "delivery.approval.retry"
	definition.InputSchema = agentapi.SchemaRef{ID: "review.subject", Version: 1}
	definition.OutputSchema = definition.InputSchema
	definition.Nodes[0] = NodeDefinition{
		ID: "approve", Kind: NodeHumanApproval,
		InputSchema: definition.InputSchema, OutputSchema: definition.OutputSchema,
		Retry: RetryPolicy{MaxAttempts: 3}, Timeout: time.Second,
	}
	observer := &recordingRunObserver{}

	_, err := NewOrchestrator(testSchemaRegistry(t), nil, nil).RunObserved(
		t.Context(), definition, RunRequest{
			RunID: "workflow_run_approval",
			Input: json.RawMessage(`{"subject":"x"}`),
		}, observer,
	)
	if !errors.Is(err, ErrHumanApprovalRequired) {
		t.Fatalf("Run error = %v, want human approval", err)
	}
	if got := observer.Started(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("started attempts = %v, want [1]", got)
	}
	if got := observer.Failed(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("failed attempts = %v, want [1]", got)
	}
}

func TestOrchestratorDoesNotRetryWritePermissionNode(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Permissions = agentapi.PermissionPolicy{Scopes: []string{"knowledge.read", "knowledge.write"}}
	definition.Nodes[0].Permissions = definition.Permissions
	definition.Nodes[0].Retry = RetryPolicy{MaxAttempts: 2}
	executor := &scriptedWorkflowExecutor{
		failAttempts: map[int]error{1: retryableWorkflowError{}},
	}

	_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		t.Context(), definition, RunRequest{
			RunID: "workflow_run_write",
			Input: json.RawMessage(`{"subject":"x"}`),
			ActorPermissions: agentapi.PermissionPolicy{
				Scopes: []string{"knowledge.read", "knowledge.write"},
			},
			ScenarioPermissions: agentapi.PermissionPolicy{
				Scopes: []string{"knowledge.read", "knowledge.write"},
			},
		}, nil,
	)
	if err == nil {
		t.Fatal("write-permission node unexpectedly retried into success")
	}
	if got := executor.Attempts(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("executor attempts = %v, want [1]", got)
	}
}

func TestOrchestratorRetriesOnlyExplicitlySafeTransformFailure(t *testing.T) {
	t.Run("safe", func(t *testing.T) {
		definition := singleNodeWorkflow()
		definition.ID = "transform.retry.safe"
		definition.Nodes[0] = NodeDefinition{
			ID: "transform", Kind: NodeTransform, TransformID: "feature.transform",
			InputSchema: definition.InputSchema, OutputSchema: definition.OutputSchema,
			Retry: RetryPolicy{MaxAttempts: 2}, RetrySafe: true, Timeout: time.Second,
		}
		executor := &scriptedWorkflowExecutor{
			failAttempts: map[int]error{1: retryableWorkflowError{}},
		}

		result, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
			t.Context(), definition, RunRequest{
				RunID: "workflow_run_transform_retry_safe",
				Input: json.RawMessage(`{"subject":"x"}`),
			}, nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		if result.Output.ID == "" {
			t.Fatal("successful transform retry produced no output")
		}
		if got := executor.Attempts(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
			t.Fatalf("executor attempts = %v, want [1 2]", got)
		}
	})

	t.Run("unsafe", func(t *testing.T) {
		definition := singleNodeWorkflow()
		definition.ID = "transform.retry.unsafe"
		definition.Nodes[0] = NodeDefinition{
			ID: "transform", Kind: NodeTransform, TransformID: "feature.transform",
			InputSchema: definition.InputSchema, OutputSchema: definition.OutputSchema,
			Retry: RetryPolicy{MaxAttempts: 2}, Timeout: time.Second,
		}
		executor := &scriptedWorkflowExecutor{
			failAttempts: map[int]error{1: retryableWorkflowError{}},
		}

		_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
			t.Context(), definition, RunRequest{
				RunID: "workflow_run_transform_retry_unsafe",
				Input: json.RawMessage(`{"subject":"x"}`),
			}, nil,
		)
		if err == nil {
			t.Fatal("unsafe transform unexpectedly retried into success")
		}
		if got := executor.Attempts(); len(got) != 1 || got[0] != 1 {
			t.Fatalf("executor attempts = %v, want [1]", got)
		}
	})
}

func TestOrchestratorDoesNotRetrySideEffectingDomainTransform(t *testing.T) {
	definition := singleNodeWorkflow()
	permission := agentapi.PermissionPolicy{
		Scopes: []string{scope.FeatureDelivery},
	}
	definition.ID = "transform.retry.domain-write"
	definition.Permissions = permission
	definition.Nodes[0] = NodeDefinition{
		ID: "transform", Kind: NodeTransform, TransformID: "feature.transform",
		InputSchema: definition.InputSchema, OutputSchema: definition.OutputSchema,
		Permissions: permission, RetrySafe: true,
		Retry: RetryPolicy{MaxAttempts: 2}, Timeout: time.Second,
	}
	executor := &scriptedWorkflowExecutor{
		failAttempts: map[int]error{1: retryableWorkflowError{}},
	}

	_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		t.Context(), definition, RunRequest{
			RunID:               "workflow_run_transform_domain_write",
			Input:               json.RawMessage(`{"subject":"x"}`),
			ActorPermissions:    permission,
			ScenarioPermissions: permission,
		}, nil,
	)
	if err == nil {
		t.Fatal("side-effecting domain transform unexpectedly retried into success")
	}
	if got := executor.Attempts(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("executor attempts = %v, want [1]", got)
	}
}

func TestServicePersistsRetryAttempts(t *testing.T) {
	schemas := testSchemaRegistry(t)
	definition := singleNodeWorkflow()
	definition.Permissions = agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	definition.Nodes[0].Permissions = definition.Permissions
	definition.Nodes[0].Retry = RetryPolicy{MaxAttempts: 2}
	catalog := NewCatalog(schemas, testAgentDefinitions(t))
	if err := catalog.Publish([]Definition{definition}); err != nil {
		t.Fatal(err)
	}
	persistence := &recordingWorkflowPersistence{}
	executor := &scriptedWorkflowExecutor{
		failAttempts: map[int]error{1: retryableWorkflowError{}},
	}
	service, err := NewService(catalog, persistence, NewOrchestrator(schemas, executor, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Execute(t.Context(), ExecuteRequest{
		Workflow: DefinitionRef{ID: definition.ID, Version: definition.Version},
		Input:    json.RawMessage(`{"subject":"x"}`),
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if len(persistence.startedNodes) != 2 ||
		persistence.startedNodes[0].Attempt != 1 ||
		persistence.startedNodes[1].Attempt != 2 {
		t.Fatalf("started node attempts = %+v", persistence.startedNodes)
	}
	if len(persistence.failedNodes) != 1 || persistence.failedNodes[0].attempt != 1 {
		t.Fatalf("failed node attempts = %+v", persistence.failedNodes)
	}
	if len(persistence.succeededNodes) != 1 || persistence.succeededNodes[0].attempt != 2 {
		t.Fatalf("succeeded node attempts = %+v", persistence.succeededNodes)
	}
}

type retryableWorkflowError struct{}

func (retryableWorkflowError) Error() string {
	return "temporary infrastructure failure"
}

func (retryableWorkflowError) Retryable() bool {
	return true
}

type scriptedWorkflowExecutor struct {
	mu           sync.Mutex
	attempts     []int
	failAttempts map[int]error
	cancel       context.CancelFunc
}

func (executor *scriptedWorkflowExecutor) Execute(
	_ context.Context,
	request NodeRequest,
) (NodeResult, error) {
	executor.mu.Lock()
	executor.attempts = append(executor.attempts, request.Attempt)
	executor.mu.Unlock()
	if runErr, ok := executor.failAttempts[request.Attempt]; ok {
		if executor.cancel != nil {
			executor.cancel()
		}
		return NodeResult{}, runErr
	}
	payload, _ := json.Marshal(map[string]string{"node": request.Node.ID})
	return NodeResult{
		Handoff: Handoff{Payload: payload, Completeness: Complete},
	}, nil
}

func (executor *scriptedWorkflowExecutor) Attempts() []int {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]int(nil), executor.attempts...)
}

type recordingRunObserver struct {
	mu        sync.Mutex
	started   []int
	succeeded []int
	failed    []int
}

func (observer *recordingRunObserver) NodeStarted(_ context.Context, request NodeRequest) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.started = append(observer.started, request.Attempt)
	return nil
}

func (observer *recordingRunObserver) NodeSucceeded(
	_ context.Context,
	request NodeRequest,
	_ NodeResult,
	_ *GateDecision,
) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.succeeded = append(observer.succeeded, request.Attempt)
	return nil
}

func (observer *recordingRunObserver) NodeFailed(
	_ context.Context,
	request NodeRequest,
	_ NodeResult,
	_ error,
) error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.failed = append(observer.failed, request.Attempt)
	return nil
}

func (observer *recordingRunObserver) Started() []int {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return append([]int(nil), observer.started...)
}

func (observer *recordingRunObserver) Succeeded() []int {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return append([]int(nil), observer.succeeded...)
}

func (observer *recordingRunObserver) Failed() []int {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return append([]int(nil), observer.failed...)
}
