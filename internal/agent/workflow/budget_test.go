package workflow

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

func TestPrepareValidatesResourceBudgets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Definition)
		want   string
	}{
		{
			name: "negative workflow budget",
			mutate: func(definition *Definition) {
				definition.Budget.MaxInputTokens = -1
			},
			want: "resource budgets cannot be negative",
		},
		{
			name: "negative node budget",
			mutate: func(definition *Definition) {
				definition.Nodes[0].Budget.MaxToolCalls = -1
			},
			want: "budgets cannot be negative",
		},
		{
			name: "missing input reservation",
			mutate: func(definition *Definition) {
				definition.Budget.MaxInputTokens = 10
			},
			want: "input token budget is required",
		},
		{
			name: "missing output reservation",
			mutate: func(definition *Definition) {
				definition.Budget.MaxOutputTokens = 10
			},
			want: "output token budget is required",
		},
		{
			name: "missing total reservation",
			mutate: func(definition *Definition) {
				definition.Budget.MaxTotalTokens = 10
			},
			want: "total token budget is required",
		},
		{
			name: "missing cost reservation",
			mutate: func(definition *Definition) {
				definition.Budget.MaxCostMicros = 10
			},
			want: "cost budget is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := singleNodeWorkflow()
			test.mutate(&definition)
			_, err := Prepare(definition, testSchemaRegistry(t))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Prepare error = %v, want %q", err, test.want)
			}
		})
	}

	definition := singleNodeWorkflow()
	definition.Budget.MaxInputTokens = 10
	definition.Budget.MaxOutputTokens = 20
	definition.Budget.MaxTotalTokens = 30
	definition.Budget.MaxToolCalls = 3
	definition.Budget.MaxCostMicros = 40
	definition.Budget.MaxRetries = 1
	definition.Nodes[0].Budget = NodeBudget{
		MaxInputTokens: 10, MaxOutputTokens: 20, MaxTotalTokens: 30,
		MaxToolCalls: 3, MaxCostMicros: 40,
	}
	if _, err := Prepare(definition, testSchemaRegistry(t)); err != nil {
		t.Fatalf("Prepare valid resource budgets: %v", err)
	}
}

func TestResourceBudgetsAffectWorkflowContentHash(t *testing.T) {
	base, err := Prepare(singleNodeWorkflow(), testSchemaRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	workflowBudget := singleNodeWorkflow()
	workflowBudget.Budget.MaxRetries = 1
	preparedWorkflowBudget, err := Prepare(workflowBudget, testSchemaRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	nodeBudget := singleNodeWorkflow()
	nodeBudget.Nodes[0].Budget.MaxInputTokens = 10
	preparedNodeBudget, err := Prepare(nodeBudget, testSchemaRegistry(t))
	if err != nil {
		t.Fatal(err)
	}
	if base.ContentHash == preparedWorkflowBudget.ContentHash {
		t.Fatal("workflow resource budget did not change the content hash")
	}
	if base.ContentHash == preparedNodeBudget.ContentHash {
		t.Fatal("node resource budget did not change the content hash")
	}
}

func TestOrchestratorReservesParallelBudgetsAtomically(t *testing.T) {
	definition := testWorkflow()
	definition.Budget.MaxInputTokens = 10
	for index := range 2 {
		definition.Nodes[index].Budget.MaxInputTokens = 10
	}
	executor := &blockingBudgetExecutor{
		started: make(chan string, 2),
		release: make(chan struct{}),
	}
	observer := &budgetRunObserver{}
	done := make(chan error, 1)
	go func() {
		_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
			t.Context(),
			definition,
			RunRequest{
				RunID: "workflow_budget_parallel",
				Input: json.RawMessage(`{"subject":"x"}`),
			},
			observer,
		)
		done <- err
	}()

	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("no parallel node started")
	}
	select {
	case nodeID := <-executor.started:
		t.Fatalf("second node %q started without workflow budget capacity", nodeID)
	case <-time.After(50 * time.Millisecond):
	}
	close(executor.release)
	err := <-done
	if !errors.Is(err, ErrNoAffordableTask) {
		t.Fatalf("Run error = %v, want no affordable task", err)
	}
	if observer.StartedCount() != 1 {
		t.Fatalf("started attempts = %d, want 1", observer.StartedCount())
	}
}

func TestOrchestratorSkipsOptionalNodeWhenBudgetUnavailable(t *testing.T) {
	report := agentapi.SchemaRef{ID: "review.report", Version: 1}
	definition := Definition{
		ID:           "delivery.review.optional-budget",
		Version:      1,
		Purpose:      "Continue with available outputs when an optional review exceeds budget.",
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "review.report.list", Version: 1},
		Budget: Budget{
			MaxNodes: 3, MaxParallelism: 1, MaxRounds: 1, MaxDepth: 3,
			Timeout:         time.Second,
			MaxHandoffBytes: 4096, MaxInputTokens: 10,
		},
		FailurePolicy: FailurePolicy{Mode: CollectAvailable},
		Nodes: []NodeDefinition{
			{
				ID: "review.required", Kind: NodeAgent,
				Agent:        agentapi.DefinitionRef{ID: "review.correctness", Version: 1},
				InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
				OutputSchema: report, Budget: NodeBudget{MaxInputTokens: 10},
				Timeout: time.Second,
			},
			{
				ID: "review.optional", Kind: NodeAgent, Optional: true,
				Agent:        agentapi.DefinitionRef{ID: "review.security", Version: 1},
				InputSchema:  report,
				OutputSchema: report, Budget: NodeBudget{MaxInputTokens: 10},
				Timeout: time.Second,
			},
			{
				ID: "synthesize", Kind: NodeJoin,
				InputSchema: report, OutputSchema: agentapi.SchemaRef{ID: "review.report.list", Version: 1},
				Timeout: time.Second,
			},
		},
		Edges: []EdgeDefinition{
			{From: "review.required", To: "review.optional", Required: true},
			{From: "review.required", To: "synthesize", Required: true},
			{From: "review.optional", To: "synthesize", Required: false},
		},
	}
	executor := &usageWorkflowExecutor{
		usage: map[string]Usage{
			"review.required": {InputTokens: 10, TotalTokens: 10},
		},
	}
	observer := &budgetRunObserver{}
	result, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		t.Context(),
		definition,
		RunRequest{
			RunID: "workflow_budget_optional",
			Input: json.RawMessage(`{"subject":"x"}`),
		},
		observer,
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls := executor.Calls(); len(calls) != 1 || calls[0] != "review.required" {
		t.Fatalf("executor calls = %v, want only required node", calls)
	}
	if observer.StartedCount() != 2 {
		t.Fatalf("started attempts = %d, want required node and join", observer.StartedCount())
	}
	if result.Usage.InputTokens != 10 {
		t.Fatalf("workflow usage = %+v", result.Usage)
	}
	var reports []map[string]string
	if err := json.Unmarshal(result.Output.Payload, &reports); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0]["node"] != "review.required" {
		t.Fatalf("output reports = %v", reports)
	}
}

func TestOrchestratorRejectsActualUsageBeyondNodeBudget(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Budget.MaxInputTokens = 100
	definition.Nodes[0].Budget.MaxInputTokens = 10
	executor := &usageWorkflowExecutor{
		usage: map[string]Usage{
			"review.a": {InputTokens: 11, TotalTokens: 11},
		},
	}
	observer := &budgetRunObserver{}

	_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		t.Context(),
		definition,
		RunRequest{
			RunID: "workflow_budget_node_actual",
			Input: json.RawMessage(`{"subject":"x"}`),
		},
		observer,
	)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Run error = %v, want workflow budget exhaustion", err)
	}
	failed := observer.FailedResults()
	if len(failed) != 1 || failed[0].Usage.InputTokens != 11 {
		t.Fatalf("failed results = %+v", failed)
	}
	if observer.SucceededCount() != 0 {
		t.Fatalf("succeeded attempts = %d, want 0", observer.SucceededCount())
	}
}

func TestOrchestratorEnforcesTotalTokenBudget(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Budget.MaxTotalTokens = 10
	definition.Nodes[0].Budget.MaxTotalTokens = 10
	executor := &usageWorkflowExecutor{
		usage: map[string]Usage{
			"review.a": {InputTokens: 6, OutputTokens: 3, TotalTokens: 11},
		},
	}

	_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).Run(
		t.Context(),
		definition,
		RunRequest{
			RunID: "workflow_budget_total_tokens",
			Input: json.RawMessage(`{"subject":"x"}`),
		},
	)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Run error = %v, want workflow budget exhaustion", err)
	}
}

func TestOrchestratorEnforcesWorkflowRetryBudgetBeforeStartingAttempt(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Budget.MaxRetries = 1
	definition.Nodes[0].Retry = RetryPolicy{MaxAttempts: 3}
	executor := &scriptedWorkflowExecutor{
		failAttempts: map[int]error{
			1: retryableWorkflowError{},
			2: retryableWorkflowError{},
		},
	}
	observer := &recordingRunObserver{}

	_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		t.Context(),
		definition,
		RunRequest{
			RunID: "workflow_budget_retry",
			Input: json.RawMessage(`{"subject":"x"}`),
		},
		observer,
	)
	if !errors.Is(err, ErrNoAffordableTask) {
		t.Fatalf("Run error = %v, want no affordable task", err)
	}
	if attempts := executor.Attempts(); len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("executor attempts = %v, want [1 2]", attempts)
	}
	if started := observer.Started(); len(started) != 2 {
		t.Fatalf("started attempts = %v, want [1 2]", started)
	}
}

type blockingBudgetExecutor struct {
	started chan string
	release chan struct{}
}

func (executor *blockingBudgetExecutor) Execute(
	ctx context.Context,
	request NodeRequest,
) (NodeResult, error) {
	executor.started <- request.Node.ID
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

type usageWorkflowExecutor struct {
	mu    sync.Mutex
	calls []string
	usage map[string]Usage
}

func (executor *usageWorkflowExecutor) Execute(
	_ context.Context,
	request NodeRequest,
) (NodeResult, error) {
	executor.mu.Lock()
	executor.calls = append(executor.calls, request.Node.ID)
	usage := executor.usage[request.Node.ID]
	executor.mu.Unlock()
	payload, _ := json.Marshal(map[string]string{"node": request.Node.ID})
	return NodeResult{
		Handoff: Handoff{Payload: payload, Completeness: Complete},
		Usage:   usage,
	}, nil
}

func (executor *usageWorkflowExecutor) Calls() []string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]string(nil), executor.calls...)
}

type budgetRunObserver struct {
	mu        sync.Mutex
	started   int
	succeeded []NodeResult
	failed    []NodeResult
}

func (observer *budgetRunObserver) NodeStarted(context.Context, NodeRequest) error {
	observer.mu.Lock()
	observer.started++
	observer.mu.Unlock()
	return nil
}

func (observer *budgetRunObserver) NodeSucceeded(
	_ context.Context,
	_ NodeRequest,
	result NodeResult,
	_ *GateDecision,
) error {
	observer.mu.Lock()
	observer.succeeded = append(observer.succeeded, result)
	observer.mu.Unlock()
	return nil
}

func (observer *budgetRunObserver) NodeFailed(
	_ context.Context,
	_ NodeRequest,
	result NodeResult,
	_ error,
) error {
	observer.mu.Lock()
	observer.failed = append(observer.failed, result)
	observer.mu.Unlock()
	return nil
}

func (observer *budgetRunObserver) StartedCount() int {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.started
}

func (observer *budgetRunObserver) SucceededCount() int {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return len(observer.succeeded)
}

func (observer *budgetRunObserver) FailedResults() []NodeResult {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return append([]NodeResult(nil), observer.failed...)
}
