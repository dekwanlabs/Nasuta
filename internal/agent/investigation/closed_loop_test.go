package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestWorkflowDeliversReadableInsufficiencyWhenVerifierFindsNoClaims(t *testing.T) {
	catalog := closedLoopCatalog(t)
	evidence := EvidenceCandidate{
		SourceKind: "code",
		Target:     "service-a",
		Content:    "the request reaches service-a before the downstream call",
	}
	verifierCalled := false
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			switch task.Executor {
			case ExecutorInvestigator:
				return TaskExecutionResult{
					EvidenceCandidates: []EvidenceCandidate{evidence},
					Failure: &RunFailure{
						Code: FailureReasoning, Message: "investigator stopped after collecting evidence",
					},
				}, nil
			case ExecutorVerifier:
				verifierCalled = true
				return TaskExecutionResult{Output: json.RawMessage(`{"verified":true}`)}, nil
			default:
				t.Fatalf("unexpected executor: %s", task.Executor)
				return TaskExecutionResult{}, nil
			}
		})),
		MaxRounds: 1,
	})

	run, err := coordinator.ExecuteWithProposal(
		t.Context(),
		testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true}),
		closedLoopProposal(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !verifierCalled || run.Status != RunDelivered || run.Delivery == nil {
		t.Fatalf("run = %#v, verifier_called=%v", run, verifierCalled)
	}
	if run.Delivery.Status != DeliveryEvidenceInsufficient || len(run.Report.Gaps) == 0 {
		t.Fatalf("delivery = %#v, report = %#v", run.Delivery, run.Report)
	}
	if len(run.Report.Evidence) != 1 || len(run.Report.Claims) != 0 {
		t.Fatalf("report = %#v", run.Report)
	}
	if text := run.Delivery.Text; strings.TrimSpace(text) == "" || strings.Contains(text, "Investigation limits") || containsOpaqueIdentifier(text) {
		t.Fatalf("delivery text is not user-readable: %q", text)
	}
}

func TestWorkflowPreservesInvestigatorBudgetFailureAsTerminalCause(t *testing.T) {
	catalog := closedLoopCatalog(t)
	verifierCalled := false
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			if task.Executor == ExecutorVerifier {
				verifierCalled = true
			}
			return TaskExecutionResult{Failure: &RunFailure{
				Code: FailureBudget, Message: "model call budget exhausted before investigator could produce evidence",
			}}, nil
		})),
		MaxRounds: 1,
	})

	run, err := coordinator.ExecuteWithProposal(
		t.Context(),
		testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true}),
		closedLoopProposal(),
	)
	if err == nil {
		t.Fatal("budget failure returned success")
	}
	if verifierCalled || run.Status != RunBudgetExhausted || run.Failure == nil || run.Failure.Code != FailureBudget {
		t.Fatalf("run = %#v, verifier_called=%v, err=%v", run, verifierCalled, err)
	}
	if !strings.Contains(run.Failure.Message, "model call budget exhausted") || strings.Contains(run.Failure.Message, "required dependency failed") {
		t.Fatalf("terminal failure lost root cause: %#v", run.Failure)
	}
	verifier := run.Results["evidence.verify"]
	if verifier.Status != TaskBlocked || verifier.Failure == nil || verifier.Failure.Code != FailureBudget ||
		!strings.Contains(verifier.Failure.Message, "model call budget exhausted") {
		t.Fatalf("blocked verifier = %#v", verifier)
	}
}

func TestWorkflowRejectsMachineClaimWithoutMaskingVerifierFailure(t *testing.T) {
	catalog := closedLoopCatalog(t)
	evidence := EvidenceCandidate{
		SourceKind: "code",
		Target:     "service-a",
		Content:    "service-a calls the downstream client",
	}
	normalized, err := normalizeEvidence("", evidence)
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			if task.Executor == ExecutorInvestigator {
				return TaskExecutionResult{
					Output:             json.RawMessage(`{"summary":"evidence collected"}`),
					EvidenceCandidates: []EvidenceCandidate{evidence},
				}, nil
			}
			return TaskExecutionResult{
				Output: json.RawMessage(`{"verified":true}`),
				Claims: []ClaimCandidate{{
					GoalID: "g1",
					Text:   `{"service":"service-a","truncated":false}`,
					Status: ClaimSupported,
					EvidenceRefs: []EvidenceRef{{
						EvidenceID: normalized.ID, SourceKind: normalized.SourceKind,
						Target: normalized.Target, ContentHash: normalized.ContentHash,
					}},
				}},
			}, nil
		})),
		MaxRounds: 1,
	})

	run, err := coordinator.ExecuteWithProposal(
		t.Context(),
		testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true}),
		closedLoopProposal(),
	)
	if err == nil {
		t.Fatal("invalid verifier claim returned success")
	}
	if run.Status != RunFailed || run.Failure == nil || run.Failure.Code != FailureVerifier || run.Delivery != nil {
		t.Fatalf("run = %#v, err=%v", run, err)
	}
	if len(run.Claims) != 0 || strings.Contains(run.Failure.Message, "required dependency failed") ||
		!strings.Contains(run.Failure.Message, "opaque identifier") {
		t.Fatalf("verifier failure was masked or invalid claim admitted: run=%#v", run)
	}
	verifier := run.Results["evidence.verify"]
	if verifier.Status != TaskFailed || verifier.Failure == nil || verifier.Failure.Code != FailureVerifier {
		t.Fatalf("verifier result = %#v", verifier)
	}
}

func closedLoopCatalog(t *testing.T) *TaskTemplateCatalog {
	t.Helper()
	catalog := NewTaskTemplateCatalog()
	for _, template := range []TaskTemplate{
		testTemplate("proposal.docs.verify", 1, []string{"flow"}, ExecutorInvestigator, nil, BudgetVector{}),
		testTemplate("evidence.verify", 1, []string{"flow"}, ExecutorVerifier, nil, BudgetVector{}),
	} {
		if err := catalog.Register(template); err != nil {
			t.Fatal(err)
		}
	}
	return catalog
}

func closedLoopProposal() *agentapi.TaskGraphProposal {
	return &agentapi.TaskGraphProposal{Tasks: []agentapi.TaskSpec{{
		ID: "docs-task", Purpose: "inspect available documentation",
		Capability: "knowledge.docs.verify", EvidenceGoalIDs: []string{"g1"},
	}}}
}

func TestWorkflowFailureReleasesCompositionProtectionBeforePersistingBudget(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	store := &failNthBudgetStore{MemoryRunStore: NewMemoryRunStore(), failAt: 4}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   store,
		Executors: testExecutors(TaskExecutorFunc(func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
		})),
		BudgetLimit:       BudgetVector{OutputTokens: 100},
		CompositionBudget: BudgetVector{OutputTokens: 100},
		MaxRounds:         1,
	})

	run, err := coordinator.Execute(t.Context(), testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true}))
	if err == nil || run.Status != RunFailed {
		t.Fatalf("run = %#v, err=%v", run, err)
	}
	if store.calls < store.failAt {
		t.Fatalf("SaveBudget calls = %d, want at least %d", store.calls, store.failAt)
	}
	if run.Budget.Run.Reserved.OutputTokens != 0 || run.Budget.Stages[StageComposition].Reserved.OutputTokens != 0 {
		t.Fatalf("failed run retained composition reservation: %#v", run.Budget)
	}
}

type failNthBudgetStore struct {
	*MemoryRunStore
	failAt int
	calls  int
}

func (store *failNthBudgetStore) SaveBudget(runID string, budget BudgetSnapshot) error {
	store.calls++
	if store.calls == store.failAt {
		return errors.New("injected budget persistence failure")
	}
	return store.MemoryRunStore.SaveBudget(runID, budget)
}

func TestWorkflowDeliversBoundedEvidenceWhenVerifierBudgetIsExhausted(t *testing.T) {
	catalog := closedLoopCatalog(t)
	evidence := EvidenceCandidate{
		SourceKind: "code",
		Target:     "service-a",
		Content:    "the request reaches service-a before the downstream call",
	}
	verifierCalled := false
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog,
		Schemas: testSchemas(),
		Store:   NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			if task.Executor == ExecutorInvestigator {
				return TaskExecutionResult{
					EvidenceCandidates: []EvidenceCandidate{evidence},
				}, nil
			}
			verifierCalled = true
			return TaskExecutionResult{Failure: &RunFailure{
				Code: FailureBudget, Message: "verifier model call exhausted the shared budget",
			}}, nil
		})),
		MaxRounds: 1,
	})

	run, err := coordinator.ExecuteWithProposal(
		t.Context(),
		testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true}),
		closedLoopProposal(),
	)
	if err != nil {
		t.Fatalf("budget-limited verifier should still produce bounded delivery: %v", err)
	}
	if !verifierCalled || run.Status != RunDelivered || run.Delivery == nil {
		t.Fatalf("run = %#v, verifier_called=%v", run, verifierCalled)
	}
	if run.Delivery.Status != DeliveryEvidenceInsufficient || !strings.Contains(run.Delivery.Text, "证据") {
		t.Fatalf("delivery = %#v", run.Delivery)
	}
	if len(run.Evidence) != 1 || run.Evidence[0].TaskID != "docs-task" {
		t.Fatalf("evidence = %#v", run.Evidence)
	}
	if len(run.Report.Failures) == 0 || run.Report.Failures[len(run.Report.Failures)-1].Code != FailureBudget {
		t.Fatalf("report failures = %#v", run.Report.Failures)
	}
	verifier := run.Results["evidence.verify"]
	if verifier.Status != TaskFailed || verifier.Failure == nil || verifier.Failure.Code != FailureBudget {
		t.Fatalf("verifier result = %#v", verifier)
	}
}
