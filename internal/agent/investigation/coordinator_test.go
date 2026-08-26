package investigation

import (
	"context"
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
	if !ok || verifier.Status != TaskBlocked || verifier.Failure == nil || verifier.Failure.Message != "required dependency failed" {
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
