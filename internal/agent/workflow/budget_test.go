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
	t.Run("negative workflow budget", func(t *testing.T) {
		definition := singleNodeWorkflow()
		definition.Budget.MaxInputTokens = -1
		_, err := Prepare(definition, testSchemaRegistry(t))
		if err == nil || !strings.Contains(err.Error(), "resource budgets cannot be negative") {
			t.Fatalf("Prepare error = %v, want negative workflow budget rejection", err)
		}
	})
	t.Run("negative node budget", func(t *testing.T) {
		definition := singleNodeWorkflow()
		definition.Nodes[0].Budget.MaxToolCalls = -1
		_, err := Prepare(definition, testSchemaRegistry(t))
		if err == nil || !strings.Contains(err.Error(), "budgets cannot be negative") {
			t.Fatalf("Prepare error = %v, want negative node budget rejection", err)
		}
	})

	// Workflow resource limits are run-level shared limits. They may be
	// configured independently; a node no longer needs a matching reservation.
	for name, mutate := range map[string]func(*Definition){
		"input":  func(definition *Definition) { definition.Budget.MaxInputTokens = 10 },
		"output": func(definition *Definition) { definition.Budget.MaxOutputTokens = 10 },
		"total":  func(definition *Definition) { definition.Budget.MaxTotalTokens = 10 },
		"cost":   func(definition *Definition) { definition.Budget.MaxCostMicros = 10 },
	} {
		t.Run("independent workflow limit "+name, func(t *testing.T) {
			definition := singleNodeWorkflow()
			mutate(&definition)
			if _, err := Prepare(definition, testSchemaRegistry(t)); err != nil {
				t.Fatalf("Prepare error = %v, want independent workflow limit to be valid", err)
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

func TestBudgetAccountProtectsDownstreamPhases(t *testing.T) {
	account, err := newBudgetAccount(Budget{
		MaxOutputTokens:       100,
		MaxTotalTokens:        100,
		VerifierReserveTokens: 20,
		ComposerReserveTokens: 30,
	}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	if got := account.AvailableForPhase(agentapi.RunBudgetPhaseDefault); got.OutputTokens != 50 || got.TotalTokens != 50 {
		t.Fatalf("default availability = %+v, want 50 output/total tokens", got)
	}
	if got := account.AvailableForPhase(agentapi.RunBudgetPhaseVerifier); got.OutputTokens != 70 || got.TotalTokens != 70 {
		t.Fatalf("verifier availability = %+v, want 70 output/total tokens", got)
	}
	if got := account.AvailableForPhase(agentapi.RunBudgetPhaseAnswer); got.OutputTokens != 100 || got.TotalTokens != 100 {
		t.Fatalf("answer availability = %+v, want full capacity", got)
	}

	if _, err := account.ReserveCallForPhase(agentapi.Usage{OutputTokens: 51, TotalTokens: 51}, agentapi.RunBudgetPhaseDefault); err == nil {
		t.Fatal("default phase reserved protected downstream capacity")
	}
	verifier, err := account.ReserveCallForPhase(agentapi.Usage{OutputTokens: 71, TotalTokens: 71}, agentapi.RunBudgetPhaseVerifier)
	if err == nil {
		_ = verifier.Release()
		t.Fatal("verifier phase consumed composer reserve")
	}
	answer, err := account.ReserveCallForPhase(agentapi.Usage{OutputTokens: 100, TotalTokens: 100}, agentapi.RunBudgetPhaseAnswer)
	if err != nil {
		t.Fatalf("answer phase could not consume remaining capacity: %v", err)
	}
	if err := answer.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetAccountReserveProtectionIsConcurrent(t *testing.T) {
	account, err := newBudgetAccount(Budget{
		MaxOutputTokens:       100,
		MaxTotalTokens:        100,
		VerifierReserveTokens: 20,
		ComposerReserveTokens: 30,
	}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	type reserveOutcome struct {
		reservation agentapi.RunBudgetCallReservation
		err         error
	}
	results := make(chan reserveOutcome, 2)
	release := make(chan struct{})
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			reservation, reserveErr := account.ReserveCallForPhase(
				agentapi.Usage{OutputTokens: 30, TotalTokens: 30},
				agentapi.RunBudgetPhaseDefault,
			)
			results <- reserveOutcome{reservation: reservation, err: reserveErr}
			if reserveErr == nil {
				<-release
				_ = reservation.Release()
			}
		}()
	}
	var succeeded, rejected int
	for range 2 {
		outcome := <-results
		if outcome.err == nil {
			succeeded++
			continue
		}
		rejected++
	}
	close(release)
	waitGroup.Wait()
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("reservation outcomes = succeeded:%d rejected:%d, want 1/1", succeeded, rejected)
	}
}

func TestPrepareValidatesDownstreamReserves(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Definition)
	}{
		{
			name: "negative verifier reserve",
			mutate: func(definition *Definition) {
				definition.Budget.VerifierReserveTokens = -1
			},
		},
		{
			name: "negative composer reserve",
			mutate: func(definition *Definition) {
				definition.Budget.ComposerReserveTokens = -1
			},
		},
		{
			name: "output reserve exceeds budget",
			mutate: func(definition *Definition) {
				definition.Budget.MaxOutputTokens = 10
				definition.Budget.VerifierReserveTokens = 6
				definition.Budget.ComposerReserveTokens = 5
			},
		},
		{
			name: "total reserve exceeds budget",
			mutate: func(definition *Definition) {
				definition.Budget.MaxTotalTokens = 10
				definition.Budget.VerifierReserveTokens = 6
				definition.Budget.ComposerReserveTokens = 5
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			definition := singleNodeWorkflow()
			testCase.mutate(&definition)
			if _, err := Prepare(definition, testSchemaRegistry(t)); err == nil {
				t.Fatal("Prepare accepted invalid downstream reserve")
			}
		})
	}
}

func TestBudgetAccountKeepsZeroLimitsUnlimitedWithReserves(t *testing.T) {
	account, err := newBudgetAccount(Budget{
		VerifierReserveTokens: 20,
		ComposerReserveTokens: 30,
	}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []agentapi.RunBudgetPhase{
		agentapi.RunBudgetPhaseDefault,
		agentapi.RunBudgetPhaseVerifier,
		agentapi.RunBudgetPhaseAnswer,
	} {
		available := account.AvailableForPhase(phase)
		if available.OutputTokens != int64(^uint64(0)>>1) || available.TotalTokens != int64(^uint64(0)>>1) {
			t.Fatalf("phase %q availability = %+v, want unlimited sentinel", phase, available)
		}
	}
}

func TestAttemptBudgetGateAccountsRuntimeSettlement(t *testing.T) {
	account, err := newBudgetAccount(Budget{
		MaxInputTokens: 100, MaxOutputTokens: 100, MaxTotalTokens: 200, MaxCostMicros: 100,
	}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	gate := newAttemptBudgetGate(account, agentapi.RunBudgetPhaseDefault)
	reservation, err := gate.ReserveCall(agentapi.Usage{InputTokens: 10, OutputTokens: 10, TotalTokens: 20, CostMicros: 2})
	if err != nil {
		t.Fatal(err)
	}
	actual := agentapi.Usage{InputTokens: 12, OutputTokens: 14, TotalTokens: 26, CostMicros: 3}
	if err := reservation.Settle(actual); err != nil {
		t.Fatal(err)
	}
	if got := gate.AccountedUsage(); got != workflowUsageFromAgent(actual) {
		t.Fatalf("accounted usage = %+v, want %+v", got, workflowUsageFromAgent(actual))
	}
	if got := account.Usage(); got.InputTokens != 12 || got.OutputTokens != 14 || got.TotalTokens != 26 || got.CostMicros != 3 {
		t.Fatalf("ledger usage = %+v", got)
	}
}

func TestAttemptBudgetGateLeavesToolCallsForNodeAccounting(t *testing.T) {
	account, err := newBudgetAccount(Budget{
		MaxInputTokens: 100, MaxOutputTokens: 100, MaxTotalTokens: 200,
		MaxToolCalls: 2, MaxCostMicros: 100,
	}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	gate := newAttemptBudgetGate(account, agentapi.RunBudgetPhaseDefault)
	reservation, err := gate.ReserveCall(agentapi.Usage{
		InputTokens: 10, OutputTokens: 10, TotalTokens: 20, CostMicros: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	actual := agentapi.Usage{InputTokens: 9, OutputTokens: 8, TotalTokens: 17, CostMicros: 2}
	if err := reservation.Settle(actual); err != nil {
		t.Fatal(err)
	}
	resultUsage := Usage{
		InputTokens: 9, OutputTokens: 8, TotalTokens: 17,
		ToolCalls: 2, CostMicros: 2,
	}
	remainingUsage := subtractAccountedUsage(resultUsage, gate.AccountedUsage())
	if remainingUsage != (Usage{ToolCalls: 2}) {
		t.Fatalf("remaining usage = %+v, want tool calls only", remainingUsage)
	}
	if err := account.RecordUsage(remainingUsage); err != nil {
		t.Fatal(err)
	}
	if got := account.Usage(); got != resultUsage {
		t.Fatalf("ledger usage = %+v, want %+v", got, resultUsage)
	}
	if err := account.checkCapacity(Usage{ToolCalls: 1}); err == nil {
		t.Fatal("workflow tool-call capacity remained after reaching the hard limit")
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

func TestBudgetAccountReservesParallelCallsAtomically(t *testing.T) {
	account, err := newBudgetAccount(Budget{
		MaxInputTokens:  10,
		MaxOutputTokens: 10,
		MaxTotalTokens:  20,
		MaxCostMicros:   20,
	}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	estimate := agentapi.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: 11, CostMicros: 1}
	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	for range 2 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			reservation, reserveErr := account.ReserveCall(estimate)
			if reserveErr != nil {
				results <- reserveErr
				return
			}
			results <- reservation.Settle(estimate)
		}()
	}
	waitGroup.Wait()
	close(results)

	var succeeded, rejected int
	for resultErr := range results {
		if resultErr == nil {
			succeeded++
			continue
		}
		rejected++
		if !errors.Is(resultErr, agentapi.ErrBudgetExceeded) {
			t.Fatalf("reservation error = %v, want budget error", resultErr)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("reservation outcomes = succeeded:%d rejected:%d, want 1/1", succeeded, rejected)
	}
	if usage := account.Usage(); usage.InputTokens != 10 || usage.TotalTokens != 11 {
		t.Fatalf("settled usage = %+v, want one admitted call", usage)
	}
}

func TestBudgetAccountSettlementAndRelease(t *testing.T) {
	account, err := newBudgetAccount(Budget{
		MaxInputTokens:  100,
		MaxOutputTokens: 100,
		MaxTotalTokens:  200,
		MaxCostMicros:   100,
	}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	estimate := agentapi.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CostMicros: 40}
	reservation, err := account.ReserveCall(estimate)
	if err != nil {
		t.Fatal(err)
	}
	available := account.Available()
	if available.InputTokens != 90 || available.OutputTokens != 80 ||
		available.TotalTokens != 170 || available.CostMicros != 60 {
		t.Fatalf("available with reservation = %+v", available)
	}
	if usage := account.Usage(); !usage.IsZero() {
		t.Fatalf("usage before settlement = %+v, want zero", usage)
	}
	actual := agentapi.Usage{InputTokens: 8, OutputTokens: 12, TotalTokens: 20, CostMicros: 25}
	if err := reservation.Settle(actual); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(actual); err != nil {
		t.Fatalf("idempotent settlement: %v", err)
	}
	if usage := account.Usage(); usage.InputTokens != 8 || usage.OutputTokens != 12 ||
		usage.TotalTokens != 20 || usage.CostMicros != 25 {
		t.Fatalf("settled usage = %+v", usage)
	}
	if err := reservation.Release(); err != nil {
		t.Fatalf("release after settlement: %v", err)
	}

	released, err := account.ReserveCall(estimate)
	if err != nil {
		t.Fatal(err)
	}
	if err := released.Release(); err != nil {
		t.Fatal(err)
	}
	available = account.Available()
	if available.InputTokens != 92 || available.OutputTokens != 88 ||
		available.TotalTokens != 180 || available.CostMicros != 75 {
		t.Fatalf("available after release = %+v", available)
	}
}

func TestBudgetAccountActualUsageAndRetryRemainRunScoped(t *testing.T) {
	account, err := newBudgetAccount(Budget{
		MaxInputTokens:  100,
		MaxOutputTokens: 100,
		MaxTotalTokens:  200,
		MaxCostMicros:   100,
		MaxRetries:      1,
	}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := account.ReserveCall(agentapi.Usage{
		InputTokens: 5, OutputTokens: 5, TotalTokens: 10, CostMicros: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(agentapi.Usage{
		InputTokens: 7, OutputTokens: 8, TotalTokens: 15, CostMicros: 20,
	}); err != nil {
		t.Fatalf("actual usage below workflow hard limit: %v", err)
	}
	if err := account.ConsumeRetry(); err != nil {
		t.Fatal(err)
	}
	usage := account.Usage()
	if usage.InputTokens != 7 || usage.OutputTokens != 8 || usage.TotalTokens != 15 ||
		usage.CostMicros != 20 || usage.Retries != 1 {
		t.Fatalf("run usage after retry = %+v", usage)
	}
	if err := account.ConsumeRetry(); !errors.Is(err, ErrNoAffordableTask) ||
		!errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("second retry error = %v, want no-affordable budget error", err)
	}

	ctx := agentapi.WithRunBudgetGate(context.Background(), account)
	if got := agentapi.RunBudgetUsageGateFromContext(ctx); got != account {
		t.Fatalf("context gate = %T %v, want shared account", got, account)
	}
}

func TestBudgetAccountRejectsActualUsageBeyondWorkflowLimit(t *testing.T) {
	account, err := newBudgetAccount(Budget{MaxInputTokens: 10}, Usage{})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := account.ReserveCall(agentapi.Usage{
		InputTokens: 5, OutputTokens: 1, TotalTokens: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	settleErr := reservation.Settle(agentapi.Usage{
		InputTokens: 11, OutputTokens: 1, TotalTokens: 12,
	})
	if !errors.Is(settleErr, agentapi.ErrBudgetExceeded) ||
		!errors.Is(settleErr, ErrBudgetExhausted) {
		t.Fatalf("settlement error = %v, want combined budget error", settleErr)
	}
	if usage := account.Usage(); usage.InputTokens != 11 || usage.TotalTokens != 12 {
		t.Fatalf("usage after overrun = %+v, want actual provider usage", usage)
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

func TestOrchestratorAllowsActualUsageBeyondLegacyNodeBudget(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Budget.MaxInputTokens = 100
	definition.Nodes[0].Budget.MaxInputTokens = 10
	executor := &usageWorkflowExecutor{
		usage: map[string]Usage{
			"review.a": {InputTokens: 11, TotalTokens: 11},
		},
	}
	observer := &budgetRunObserver{}

	result, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		t.Context(),
		definition,
		RunRequest{
			RunID: "workflow_budget_node_actual",
			Input: json.RawMessage(`{"subject":"x"}`),
		},
		observer,
	)
	if err != nil {
		t.Fatalf("Run error = %v, want legacy node budget to be non-binding", err)
	}
	if result.Usage.InputTokens != 11 || result.Usage.TotalTokens != 11 {
		t.Fatalf("workflow usage = %+v, want actual usage", result.Usage)
	}
	if observer.SucceededCount() != 1 || len(observer.FailedResults()) != 0 {
		t.Fatalf("node transitions = succeeded:%d failed:%d, want 1/0", observer.SucceededCount(), len(observer.FailedResults()))
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

func TestOrchestratorAccountsAndEnforcesWorkflowToolCallBudget(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Budget.MaxToolCalls = 2
	executor := &usageWorkflowExecutor{
		usage: map[string]Usage{
			"review.a": {ToolCalls: 3},
		},
	}
	observer := &budgetRunObserver{}

	_, err := NewOrchestrator(testSchemaRegistry(t), executor, nil).RunObserved(
		t.Context(),
		definition,
		RunRequest{
			RunID: "workflow_budget_tool_calls",
			Input: json.RawMessage(`{"subject":"x"}`),
		},
		observer,
	)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Run error = %v, want workflow tool-call budget exhaustion", err)
	}
	failed := observer.FailedResults()
	if len(failed) != 1 || failed[0].Usage.ToolCalls != 3 {
		t.Fatalf("failed results = %+v, want actual tool-call usage", failed)
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
