package investigation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

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
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog:     catalog,
		Schemas:     testSchemas(),
		Store:       NewMemoryRunStore(),
		BudgetLimit: BudgetVector{},
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
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
