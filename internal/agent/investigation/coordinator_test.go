package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestCoordinatorRejectsUnsupportedContractVersion(t *testing.T) {
	store := NewMemoryRunStore()
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: NewTaskTemplateCatalog(),
		Store:   store,
		Executors: testExecutors(TaskExecutorFunc(func(
			context.Context, ExecutableTask, TaskExecutionInput,
		) (TaskExecutionResult, error) {
			return TaskExecutionResult{}, nil
		})),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.Version = 2

	_, err := coordinator.Execute(t.Context(), contract)
	if !errors.Is(err, ErrPlanInvalid) || !strings.Contains(err.Error(), "current version is 1") {
		t.Fatalf("Execute error = %v", err)
	}
	if _, err := store.Get(investigationRunID(contract)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsupported contract was persisted: %v", err)
	}
}

func TestCoordinatorDeliversVerifiedAnswer(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("verify", 1, []string{"flow"}, ExecutorVerifier, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	evidenceCandidate := EvidenceCandidate{SourceKind: "code", Target: "service-a", Content: "the model client is called here"}
	normalized, err := normalizeEvidence("", evidenceCandidate)
	if err != nil {
		t.Fatal(err)
	}
	claim := ClaimCandidate{
		GoalID:       "g1",
		Text:         "service-a calls the model client",
		Status:       ClaimSupported,
		EvidenceRefs: []EvidenceRef{{EvidenceID: normalized.ID, SourceKind: normalized.SourceKind, Target: normalized.Target, ContentHash: normalized.ContentHash}},
	}
	var executionInput TaskExecutionInput
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog:     catalog,
		Schemas:     testSchemas(),
		Store:       NewMemoryRunStore(),
		BudgetLimit: BudgetVector{},
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, input TaskExecutionInput) (TaskExecutionResult, error) {
			executionInput = input
			return TaskExecutionResult{
				Output:             []byte(`{"ok":true}`),
				EvidenceCandidates: []EvidenceCandidate{evidenceCandidate},
				Claims:             []ClaimCandidate{claim},
			}, nil
		})),
		Composer: ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
			return AnswerDraft{Text: "verified answer", Status: DeliverySucceeded, ClaimIDs: []string{claimID(claim)}}, nil
		}),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.ParentRunID = "run-parent"
	contract.Actor.UserID = 42
	contract.Actor.TenantID = "tenant-a"
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunDelivered || run.Delivery == nil || run.Delivery.Status != DeliverySucceeded {
		t.Fatalf("run = %#v", run)
	}
	if run.Delivery.Text != "verified answer" || len(run.Report.Evidence) != 1 || len(run.Report.Claims) != 1 {
		t.Fatalf("delivery = %#v", run.Delivery)
	}
	if executionInput.WorkflowRunID != run.ID || executionInput.ParentRunID != contract.ParentRunID ||
		executionInput.Actor != contract.Actor || executionInput.Attempt != 1 {
		t.Fatalf("execution input identity = %#v", executionInput)
	}
}

func TestCoordinatorProtectsVerifierBudgetFromInvestigatorUsage(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryRunStore()
	var calls []ExecutorType
	investigator := TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		calls = append(calls, task.Executor)
		return TaskExecutionResult{Output: []byte(`{"investigator":true}`), Usage: BudgetVector{OutputTokens: 20}}, nil
	})
	verifier := minimumBudgetTaskExecutor{
		minOutput: 80,
		fn: func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			calls = append(calls, task.Executor)
			return TaskExecutionResult{Output: []byte(`{"verified":true}`), Usage: BudgetVector{OutputTokens: 80}}, nil
		},
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish([]agentapi.SchemaDefinition{
		{ID: DefaultTaskInputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
		{ID: DefaultTaskOutputSchema, Version: 1, Document: json.RawMessage(`{"type":"object"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog, Schemas: schemas, Store: store,
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{
			ExecutorInvestigator: investigator,
			ExecutorVerifier:     verifier,
		}),
		// The verification role receives ten percent of the Run output budget.
		// With a 1000-token Run, its protected pool is 100 tokens, not the
		// verifier's old fixed 80-token minimum.
		BudgetLimit: BudgetVector{OutputTokens: 1000}, MaxRounds: 1,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: GoalKindCoreFlow, Required: true})
	contract.CreatedAt = time.Now().UTC()
	proposal := &agentapi.TaskGraphProposal{Tasks: []agentapi.TaskSpec{{
		ID: "inspect", Purpose: "inspect the core flow", Capability: "knowledge.code.inspect",
		EvidenceGoalIDs: []string{"g1"},
	}}, Stop: agentapi.StopPolicy{MaxRounds: 1}}

	run, err := coordinator.ExecuteWithProposal(context.Background(), contract, proposal)
	if err != nil {
		t.Fatalf("ExecuteWithProposal: %v", err)
	}
	if run.Status != RunDelivered || run.Delivery == nil {
		t.Fatalf("run = %#v, want delivered fallback", run)
	}
	if len(calls) != 2 || calls[0] != ExecutorInvestigator || calls[1] != ExecutorVerifier {
		t.Fatalf("executor calls = %#v, want investigator then verifier", calls)
	}
	if result := run.Results["evidence.verify"]; result.Status != TaskSucceeded {
		t.Fatalf("verifier result = %#v, want success", result)
	}
	if got := run.Budget.Run.Used.OutputTokens; got != 100 {
		t.Fatalf("run output usage = %d, want 100", got)
	}
	if got := run.Budget.Run.Reserved.OutputTokens; got != 0 {
		t.Fatalf("run reserved output = %d, want 0", got)
	}
}

func TestCoordinatorRequiredInvestigatorFailureBlocksVerifierAndFailsRun(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	for _, template := range []TaskTemplate{
		{
			ID: "proposal.docs.verify", Version: 1,
			GoalKinds:    []string{"flow"},
			InputSchema:  agentapi.SchemaRef{ID: "investigation.input", Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: "investigation.output", Version: 1},
			Executor:     ExecutorInvestigator, Enabled: true,
		},
		{
			ID: "evidence.verify", Version: 1,
			GoalKinds:    []string{"flow"},
			InputSchema:  agentapi.SchemaRef{ID: "investigation.input", Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: "investigation.output", Version: 1},
			Executor:     ExecutorVerifier, Enabled: true,
		},
	} {
		if err := catalog.Register(template); err != nil {
			t.Fatal(err)
		}
	}
	store := NewMemoryRunStore()
	verifierCalled := false
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   store,
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			if task.ID == "evidence.verify" {
				verifierCalled = true
			}
			return TaskExecutionResult{}, errors.New("investigator provider failed")
		})),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	proposal := &agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{{
			ID: "docs-task", Purpose: "inspect documentation", Capability: "knowledge.docs.verify",
			EvidenceGoalIDs: []string{"g1"},
		}},
	}

	run, err := coordinator.ExecuteWithProposal(context.Background(), contract, proposal)
	if err == nil {
		t.Fatal("required investigator failure returned success")
	}
	if verifierCalled {
		t.Fatal("verifier started after its required investigator dependency failed")
	}
	if run.Status != RunFailed || run.Failure == nil || run.Failure.Code != FailureExecution {
		t.Fatalf("run = %#v, err = %v", run, err)
	}
	investigator, ok := run.Results["docs-task"]
	if !ok || investigator.Status != TaskFailed {
		t.Fatalf("investigator result = %#v", investigator)
	}
	verifier, ok := run.Results["evidence.verify"]
	if !ok || verifier.Status != TaskBlocked || verifier.Failure == nil ||
		!strings.Contains(verifier.Failure.Message, `required dependency "docs-task" failed`) ||
		!strings.Contains(verifier.Failure.Message, "investigator provider failed") {
		t.Fatalf("verifier result = %#v", verifier)
	}
	persisted, getErr := store.Get(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if persisted.Status != RunFailed {
		t.Fatalf("persisted status = %q, want %q", persisted.Status, RunFailed)
	}
}

func TestCoordinatorReturnsNonEmptyInsufficiencyWithoutEvidence(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
		})),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunDelivered || run.Delivery == nil || run.Delivery.Status != DeliveryEvidenceInsufficient {
		t.Fatalf("run = %#v", run)
	}
	if strings.TrimSpace(run.Delivery.Text) == "" {
		t.Fatal("coordinator returned an empty insufficiency answer")
	}
}

func TestCoordinatorFallsBackWhenComposerFails(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("verify", 1, []string{"flow"}, ExecutorVerifier, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	evidenceCandidate := EvidenceCandidate{SourceKind: "docs", Target: "runbook", Content: "the documented flow"}
	normalized, err := normalizeEvidence("", evidenceCandidate)
	if err != nil {
		t.Fatal(err)
	}
	claim := ClaimCandidate{
		GoalID:       "g1",
		Text:         "the flow is documented",
		Status:       ClaimSupported,
		EvidenceRefs: []EvidenceRef{{EvidenceID: normalized.ID, SourceKind: normalized.SourceKind, Target: normalized.Target, ContentHash: normalized.ContentHash}},
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte(`{"ok":true}`), EvidenceCandidates: []EvidenceCandidate{evidenceCandidate}, Claims: []ClaimCandidate{claim}}, nil
		})),
		Composer: ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
			return AnswerDraft{}, errors.New("composer unavailable")
		}),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != RunDelivered || run.Delivery == nil || strings.TrimSpace(run.Delivery.Text) == "" {
		t.Fatalf("run = %#v", run)
	}
	if run.Delivery.Failure == nil || run.Delivery.Failure.Code != FailureComposer {
		t.Fatalf("delivery failure = %#v", run.Delivery.Failure)
	}
}

func TestCoordinatorFailsPlanWhenRequiredGoalHasNoCapability(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("other", 1, []string{"other"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryRunStore()
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   store,
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{}, nil
		})),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	_, err := coordinator.Execute(context.Background(), contract)
	if err == nil {
		t.Fatal("coordinator accepted a contract without a required capability")
	}
	run, getErr := store.Get(investigationRunID(contract))
	if getErr != nil {
		t.Fatal(getErr)
	}
	if run.Status != RunFailed || run.Failure == nil || run.Failure.Code != FailurePlan {
		t.Fatalf("failed run = %#v", run)
	}
}

func TestCoordinatorPersistsCancellation(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(ctx context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			close(started)
			<-ctx.Done()
			return TaskExecutionResult{}, ctx.Err()
		})),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	type execution struct {
		run InvestigationRun
		err error
	}
	done := make(chan execution, 1)
	go func() {
		run, err := coordinator.Execute(ctx, contract)
		done <- execution{run: run, err: err}
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	select {
	case result := <-done:
		if result.err == nil || result.run.Status != RunCancelled || result.run.Failure == nil || result.run.Failure.Code != FailureCancelled {
			t.Fatalf("execution = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not finish after cancellation")
	}
}

func TestCoordinatorFreezesBudgetPolicyIntoRunSnapshot(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog:       catalog,
		Schemas:       testSchemas(),
		Store:         NewMemoryRunStore(),
		BudgetLimit:   BudgetVector{ToolCalls: 10},
		BudgetProfile: ProfileDeep,
		PolicyVersion: DefaultBudgetPolicyVersion,
		MaxRounds:     3,
		MaxTasks:      4,
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
		})),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	if run.Budget.Run.Profile != string(ProfileDeep) || run.Budget.Run.PolicyVersion != DefaultBudgetPolicyVersion {
		t.Fatalf("run budget policy = %#v", run.Budget.Run)
	}
	if run.Budget.Run.MaxRounds != 3 || run.Budget.Run.MaxTasks != 4 {
		t.Fatalf("run budget controls = %#v", run.Budget.Run)
	}
	for _, stage := range []BudgetStage{StagePlanning, StageExecution, StageVerification, StageComposition, StageFallback} {
		if _, ok := run.Budget.Stages[stage]; !ok {
			t.Fatalf("run budget is missing stage %q", stage)
		}
	}
}

type renewFailureLease struct{}

func (renewFailureLease) AcquireLease(context.Context, string, string, time.Duration) error {
	return nil
}
func (renewFailureLease) RenewLease(context.Context, string, string, time.Duration) error {
	return errors.New("renew failed")
}
func (renewFailureLease) ReleaseLease(context.Context, string, string) error { return nil }

func TestCoordinatorLeaseRenewalCancelsOnFailure(t *testing.T) {
	coordinator := NewCoordinator(CoordinatorOptions{
		Lease: renewFailureLease{}, BudgetLimit: BudgetVector{Duration: 30 * time.Millisecond},
	})
	ctx, stop := coordinator.withLeaseRenewal(context.Background(), "run-renew", "owner")
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("lease renewal failure did not cancel execution context")
	}
}

func TestExecuteWithProposalReadyPersistsBeforeExecution(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("verify", 1, []string{"flow"}, ExecutorVerifier, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryRunStore()
	persisted := make(chan struct{})
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog:     catalog,
		Schemas:     testSchemas(),
		Store:       store,
		BudgetLimit: BudgetVector{},
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			select {
			case <-persisted:
			default:
				t.Error("execution started before the initial snapshot was persisted")
			}
			return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
		})),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()

	var callbackRun InvestigationRun
	run, err := coordinator.ExecuteWithProposalReady(
		context.Background(),
		contract,
		nil,
		func(persistedRun InvestigationRun) {
			callbackRun = persistedRun
			loaded, loadErr := store.Get(persistedRun.ID)
			if loadErr != nil {
				t.Errorf("load persisted snapshot in readiness callback: %v", loadErr)
			} else if loaded.Status != RunCreated {
				t.Errorf("persisted snapshot status = %q, want %q", loaded.Status, RunCreated)
			}
			close(persisted)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if callbackRun.ID != run.ID || callbackRun.Status != RunCreated {
		t.Fatalf("callback run = %#v, final run = %#v", callbackRun, run)
	}
}

func TestCoordinatorLeaseRenewalFailurePersistsLeaseLost(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	store := NewMemoryRunStore()
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   store,
		Lease:   renewFailureLease{},
		Executors: testExecutors(TaskExecutorFunc(func(ctx context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			<-ctx.Done()
			return TaskExecutionResult{}, ctx.Err()
		})),
		BudgetLimit: BudgetVector{Duration: 30 * time.Millisecond},
		MaxRounds:   1,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(t.Context(), contract)
	if err == nil {
		t.Fatal("lease renewal failure returned nil error")
	}
	if run.Status != RunFailed || run.Failure == nil || run.Failure.Code != FailureLease || !run.Failure.Retryable {
		t.Fatalf("run = %#v", run)
	}
}

func TestPlanFailureClassifiesLeaseFenceAsRetryableLeaseLoss(t *testing.T) {
	failure := planFailure(fmt.Errorf("append plan event: %w", ErrLeaseFenced))
	if failure.Code != FailureLease || failure.Stage != string(StagePlanning) || !failure.Retryable {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestValidateVerifierExecutionRejectsMissingResult(t *testing.T) {
	tasks := []ExecutableTask{{ID: "evidence.verify", Executor: ExecutorVerifier}}

	failure := validateVerifierExecution(tasks, nil, nil, nil)
	if failure == nil || failure.Code != FailureVerifier || failure.TaskID != "evidence.verify" ||
		!strings.Contains(failure.Message, "did not execute") {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestValidateVerifierExecutionRejectsBlockedVerifier(t *testing.T) {
	task := ExecutableTask{ID: "evidence.verify", Executor: ExecutorVerifier}
	results := []ScheduledTaskResult{{
		Task:   task,
		Status: TaskBlocked,
		Failure: &RunFailure{
			Code: FailureExecution, Message: "required dependency failed", Stage: string(StageExecution),
		},
	}}

	failure := validateVerifierExecution([]ExecutableTask{task}, results, nil, nil)
	if failure == nil || failure.Code != FailureVerifier || failure.Stage != string(StageVerification) ||
		failure.TaskID != task.ID || failure.Message != "required dependency failed" {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestValidateVerifierExecutionAcceptsSucceededVerifier(t *testing.T) {
	task := ExecutableTask{ID: "evidence.verify", Executor: ExecutorVerifier}
	results := []ScheduledTaskResult{{Task: task, Status: TaskSucceeded}}

	if failure := validateVerifierExecution([]ExecutableTask{task}, results, nil, nil); failure != nil {
		t.Fatalf("failure = %#v", failure)
	}
}

func TestCoordinatorReleasesCompositionProtectionBeforeDelivery(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("verify", 1, []string{"flow"}, ExecutorVerifier, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	evidenceCandidate := EvidenceCandidate{SourceKind: "code", Target: "service-a", Content: "verified implementation"}
	normalized, err := normalizeEvidence("", evidenceCandidate)
	if err != nil {
		t.Fatal(err)
	}
	claim := ClaimCandidate{
		GoalID: "g1", Text: "service-a contains the implementation", Status: ClaimSupported,
		EvidenceRefs: []EvidenceRef{{
			EvidenceID: normalized.ID, SourceKind: normalized.SourceKind, Target: normalized.Target, ContentHash: normalized.ContentHash,
		}},
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog, Schemas: testSchemas(), Store: NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte(`{"ok":true}`), EvidenceCandidates: []EvidenceCandidate{evidenceCandidate}, Claims: []ClaimCandidate{claim}}, nil
		})),
		BudgetLimit:       BudgetVector{OutputTokens: 100},
		CompositionBudget: BudgetVector{OutputTokens: 100},
		Composer: ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
			return AnswerDraft{Text: "answer", Status: DeliverySucceeded, ClaimIDs: []string{claimID(claim)}, Usage: BudgetVector{OutputTokens: 50}}, nil
		}),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()

	run, err := coordinator.Execute(t.Context(), contract)
	if err != nil || run.Status != RunDelivered || run.Delivery == nil || run.Delivery.Text != "answer" {
		t.Fatalf("run = %#v, err = %v", run, err)
	}
	if got := run.Budget.Run.Used.OutputTokens; got != 50 {
		t.Fatalf("run output usage = %d, want 50", got)
	}
	if got := run.Budget.Run.Reserved.OutputTokens; got != 0 {
		t.Fatalf("run reserved output = %d, want 0", got)
	}
}

func TestCoordinatorDoesNotDeliverAfterCompositionExceedsRunLimit(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("verify", 1, []string{"flow"}, ExecutorVerifier, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	evidenceCandidate := EvidenceCandidate{SourceKind: "code", Target: "service-a", Content: "verified implementation"}
	normalized, err := normalizeEvidence("", evidenceCandidate)
	if err != nil {
		t.Fatal(err)
	}
	claim := ClaimCandidate{
		GoalID: "g1", Text: "service-a contains the implementation", Status: ClaimSupported,
		EvidenceRefs: []EvidenceRef{{
			EvidenceID: normalized.ID, SourceKind: normalized.SourceKind, Target: normalized.Target, ContentHash: normalized.ContentHash,
		}},
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog, Schemas: testSchemas(), Store: NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte(`{"ok":true}`), EvidenceCandidates: []EvidenceCandidate{evidenceCandidate}, Claims: []ClaimCandidate{claim}}, nil
		})),
		BudgetLimit:       BudgetVector{OutputTokens: 40},
		CompositionBudget: BudgetVector{OutputTokens: 100},
		Composer: ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
			return AnswerDraft{Text: "answer", Status: DeliverySucceeded, ClaimIDs: []string{claimID(claim)}, Usage: BudgetVector{OutputTokens: 50}}, nil
		}),
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()

	run, err := coordinator.Execute(t.Context(), contract)
	if err == nil || run.Status != RunBudgetExhausted || run.Delivery != nil || run.Failure == nil || run.Failure.Code != FailureBudget {
		t.Fatalf("run = %#v, err = %v", run, err)
	}
	if len(run.Report.Failures) == 0 || run.Report.Failures[len(run.Report.Failures)-1].Code != FailureBudget {
		t.Fatalf("report failures = %#v", run.Report.Failures)
	}
	if got := run.Budget.Run.Reserved.OutputTokens; got != 0 {
		t.Fatalf("run reserved output = %d, want 0", got)
	}
}

func TestRequiredTaskFailureTreatsPartialBudgetFailureAsRunFailure(t *testing.T) {
	result := ScheduledTaskResult{
		Task:   ExecutableTask{ID: "optional-investigator", Optional: true},
		Status: TaskPartial,
		Failure: &RunFailure{
			Code:    FailureBudget,
			Message: "shared investigation run budget is exhausted",
		},
	}

	failure := requiredTaskFailure(result)
	if failure == nil || failure.Code != FailureBudget || failure.TaskID != result.Task.ID {
		t.Fatalf("requiredTaskFailure = %#v, want task-scoped budget failure", failure)
	}
}

func TestPreferRunFailurePromotesBudgetFailure(t *testing.T) {
	ordinary := &RunFailure{Code: FailureExecution, Message: "dependency failed"}
	budget := &RunFailure{Code: FailureBudget, Message: "shared budget exhausted"}

	if got := preferRunFailure(ordinary, budget); got != budget {
		t.Fatalf("preferRunFailure(ordinary, budget) = %#v, want budget failure", got)
	}
	if got := preferRunFailure(budget, ordinary); got != budget {
		t.Fatalf("preferRunFailure(budget, ordinary) = %#v, want existing budget failure", got)
	}
}

func TestRunFailureErrorPreservesBudgetSentinel(t *testing.T) {
	err := failureError(RunFailure{Code: FailureBudget, Message: "budget exhausted"})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal("budget failure does not preserve ErrBudgetExceeded")
	}
	if errors.Is(failureError(RunFailure{Code: FailureExecution, Message: "runtime failed"}), ErrBudgetExceeded) {
		t.Fatal("ordinary failure was classified as budget exhaustion")
	}
}

func TestRunFailureStatusForFailureMakesBudgetTerminal(t *testing.T) {
	if got := runFailureStatusForFailure(RunFailure{Code: FailureBudget}, RunFailed); got != RunBudgetExhausted {
		t.Fatalf("budget failure status = %q, want %q", got, RunBudgetExhausted)
	}
	if got := runFailureStatusForFailure(RunFailure{Code: FailureExecution}, RunFailed); got != RunFailed {
		t.Fatalf("ordinary failure status = %q, want %q", got, RunFailed)
	}
}

func TestAdmitTaskResultRejectsOpaqueOnlyPartialInvestigator(t *testing.T) {
	evidence := NewEvidenceLedger()
	claims := NewClaimLedger([]EvidenceGoal{{ID: "g1", Kind: "flow", Required: true}}, evidence)
	coordinator := &Coordinator{}
	record, failure := coordinator.admitTaskResult(ScheduledTaskResult{
		Task:   ExecutableTask{ID: "investigator", Executor: ExecutorInvestigator},
		Status: TaskPartial,
		Result: TaskExecutionResult{EvidenceCandidates: []EvidenceCandidate{{
			SourceKind: "code", Target: "service.go:42", Content: "856d907454773e97fd50c8e2609629031f2910c0229376261da8e7d1b59f7ff7",
		}}},
		Failure: &RunFailure{Code: FailureReasoning, Message: "worker stopped"},
	}, evidence, claims)
	if failure == nil || failure.Code != FailureEmptyOutput {
		t.Fatalf("failure = %#v, want empty-output admission failure", failure)
	}
	if record.Status != TaskFailed || record.Failure == nil {
		t.Fatalf("record = %#v, want persisted failed admission", record)
	}
	if got := len(evidence.All()); got != 0 {
		t.Fatalf("evidence count = %d, want 0", got)
	}
}

type failReportStore struct {
	*MemoryRunStore
	err error
}

func (store *failReportStore) SaveReport(string, InvestigationReport) error {
	return store.err
}

func TestCoordinatorPreservesBudgetFailureWhenFailureReportCannotBePersisted(t *testing.T) {
	store := &failReportStore{MemoryRunStore: NewMemoryRunStore(), err: errors.New("report store unavailable")}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: closedLoopCatalog(t),
		Schemas: testSchemas(),
		Store:   store,
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Failure: &RunFailure{
				Code: FailureBudget, Message: "shared budget exhausted",
			}}, nil
		})),
		MaxRounds: 1,
	})

	run, err := coordinator.ExecuteWithProposal(t.Context(), testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true}), closedLoopProposal())
	if err == nil || !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("Execute error = %v, want wrapped budget error", err)
	}
	if run.Status != RunBudgetExhausted || run.Failure == nil || run.Failure.Code != FailureBudget {
		t.Fatalf("run = %#v, want terminal budget failure", run)
	}
	if !strings.Contains(run.Failure.Message, "shared budget exhausted") || !strings.Contains(run.Failure.Message, "report store unavailable") {
		t.Fatalf("failure message = %q, want root and persistence causes", run.Failure.Message)
	}
}

type vectorMinimumBudgetTaskExecutor struct {
	minimum BudgetVector
}

func (executor vectorMinimumBudgetTaskExecutor) MinimumBudget(ExecutableTask) (BudgetVector, error) {
	return executor.minimum, nil
}

func (executor vectorMinimumBudgetTaskExecutor) Execute(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
	return TaskExecutionResult{}, nil
}

func TestCoordinatorReserveVerificationAllocatesRunShare(t *testing.T) {
	const verifierID = "verify-1"
	ledger, err := NewBudgetLedger(BudgetVector{
		InputTokens: 300_000, OutputTokens: 128_000, TotalTokens: 512_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{
			ExecutorVerifier: vectorMinimumBudgetTaskExecutor{minimum: BudgetVector{
				InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
			}},
		}),
		BudgetProfile: ProfileInteractive,
	}
	reservations, err := coordinator.reserveVerification(ledger, []ExecutableTask{{
		ID: verifierID, Executor: ExecutorVerifier,
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := reservations[verifierID].Grant
	want := BudgetVector{InputTokens: 30_000, OutputTokens: 12_800, TotalTokens: 51_200}
	if got != want {
		t.Fatalf("verifier grant = %+v, want %+v", got, want)
	}
	gate := reservationBudgetGate{ledger: ledger, id: reservations[verifierID].ID}
	if _, err := gate.ReserveCall(agentapi.Usage{OutputTokens: 12_801}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("oversized verifier call error = %v, want ErrBudgetExceeded", err)
	}
}

func TestCoordinatorReserveVerificationSplitsPoolAcrossVerifiers(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{
		InputTokens: 300_000, OutputTokens: 128_000, TotalTokens: 512_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{
			ExecutorVerifier: vectorMinimumBudgetTaskExecutor{minimum: BudgetVector{OutputTokens: 1}},
		}),
		BudgetProfile: ProfileInteractive,
	}
	tasks := []ExecutableTask{
		{ID: "verify-1", Executor: ExecutorVerifier},
		{ID: "verify-2", Executor: ExecutorVerifier},
		{ID: "verify-3", Executor: ExecutorVerifier},
	}
	reservations, err := coordinator.reserveVerification(ledger, tasks)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]BudgetVector{
		"verify-1": {InputTokens: 10_000, OutputTokens: 4_267, TotalTokens: 17_067},
		"verify-2": {InputTokens: 10_000, OutputTokens: 4_267, TotalTokens: 17_067},
		"verify-3": {InputTokens: 10_000, OutputTokens: 4_266, TotalTokens: 17_066},
	}
	for id, expected := range want {
		if got := reservations[id].Grant; got != expected {
			t.Fatalf("%s grant = %+v, want %+v", id, got, expected)
		}
	}
}

func TestCoordinatorReserveVerificationTaskBudgetOnlyNarrowsRoleShare(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit BudgetVector
		want  BudgetVector
	}{
		{
			name:  "larger task budget does not expand role share",
			limit: BudgetVector{InputTokens: 100_000, OutputTokens: 50_000, TotalTokens: 200_000},
			want:  BudgetVector{InputTokens: 30_000, OutputTokens: 12_800, TotalTokens: 51_200},
		},
		{
			name:  "smaller task budget narrows role share",
			limit: BudgetVector{InputTokens: 20_000, OutputTokens: 5_000, TotalTokens: 25_000},
			want:  BudgetVector{InputTokens: 20_000, OutputTokens: 5_000, TotalTokens: 25_000},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ledger, err := NewBudgetLedger(BudgetVector{
				InputTokens: 300_000, OutputTokens: 128_000, TotalTokens: 512_000,
			})
			if err != nil {
				t.Fatal(err)
			}
			coordinator := &Coordinator{
				Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{
					ExecutorVerifier: vectorMinimumBudgetTaskExecutor{minimum: BudgetVector{
						InputTokens: 1, OutputTokens: 1, TotalTokens: 2,
					}},
				}),
				BudgetProfile: ProfileInteractive,
			}
			reservations, err := coordinator.reserveVerification(ledger, []ExecutableTask{{
				ID: "verify-1", Executor: ExecutorVerifier, Budget: TaskBudget{Limit: test.limit},
			}})
			if err != nil {
				t.Fatal(err)
			}
			if got := reservations["verify-1"].Grant; got != test.want {
				t.Fatalf("grant = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestCoordinatorReserveVerificationRejectsMinimumWithoutLeakingReservation(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{
		InputTokens: 300_000, OutputTokens: 128_000, TotalTokens: 512_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := &Coordinator{
		Executors: NewExecutorRegistry(map[ExecutorType]TaskExecutor{
			ExecutorVerifier: vectorMinimumBudgetTaskExecutor{minimum: BudgetVector{OutputTokens: 12_801}},
		}),
		BudgetProfile: ProfileInteractive,
	}
	reservations, err := coordinator.reserveVerification(ledger, []ExecutableTask{{
		ID: "verify-1", Executor: ExecutorVerifier,
	}})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("reserve error = %v, want ErrBudgetExceeded", err)
	}
	if reservations != nil {
		t.Fatalf("reservations = %#v, want nil", reservations)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Reserved != (BudgetVector{}) {
		t.Fatalf("reserved budget leaked after rejection: %+v", snapshot.Run.Reserved)
	}
	failure := verifierReservationFailure(err)
	if failure.Code != FailureBudget {
		t.Fatalf("failure code = %q, want %q", failure.Code, FailureBudget)
	}
}
