package investigation

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFaultMatrixToolUnavailableFailsTask(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1, Duration: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		return TaskExecutionResult{Failure: &RunFailure{
			Code: FailureToolUnavailable, Message: "tool unavailable", Retryable: false,
		}}, nil
	})
	task := testExecutableTask("tool-unavailable", nil)
	task.Budget.MaxAttempts = 1
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger}).Execute(
		context.Background(), []ExecutableTask{task}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskFailed ||
		results[0].Failure == nil || results[0].Failure.Code != FailureToolUnavailable {
		t.Fatalf("tool unavailable result = %#v", results)
	}
}

func TestFaultMatrixReasoningTruncationUsesDeterministicFallback(t *testing.T) {
	report := InvestigationReport{
		Evidence: []EvidenceUnit{{
			ID: "evidence-1", SourceKind: "code", Target: "svc-a",
			Content: "model client", ContentHash: "hash-1",
		}},
		Claims: []VerifiedClaim{{
			ID: "claim-1", GoalID: "g1", Text: "the entrypoint exists",
			Status: ClaimSupported,
			EvidenceRefs: []EvidenceRef{{
				EvidenceID: "evidence-1", SourceKind: "code", Target: "svc-a", ContentHash: "hash-1",
			}},
		}},
		Coverage: []GoalCoverage{{GoalID: "g1", Required: true, Status: GoalCovered, ClaimIDs: []string{"claim-1"}}},
	}
	result := (DeliveryGate{}).Deliver(context.Background(), InvestigationContract{
		Version: InvestigationContractVersion,
		ID:      "run-truncated", Question: "trace entrypoint",
		EvidenceGoals: []EvidenceGoal{{ID: "g1", Kind: GoalKindEntrypoint, Required: true}},
	}, report, ComposerFunc(func(context.Context, InvestigationContract, InvestigationReport) (AnswerDraft, error) {
		return AnswerDraft{}, errors.New("reasoning truncated")
	}))
	if result.Status != DeliverySucceeded || result.Text == "" || result.Failure == nil ||
		result.Failure.Code != FailureComposer {
		t.Fatalf("truncation delivery = %#v", result)
	}
}

func TestFaultMatrixProviderTimeoutCancelsTask(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	executor := TaskExecutorFunc(func(ctx context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		select {
		case <-time.After(20 * time.Millisecond):
			return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
		case <-ctx.Done():
			return TaskExecutionResult{}, ctx.Err()
		}
	})
	task := testExecutableTask("timeout-task", nil)
	task.Budget.MaxAttempts = 1
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	results, err := (Scheduler{Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger}).Execute(
		ctx, []ExecutableTask{task}, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskCancelled ||
		results[0].Failure == nil || results[0].Failure.Code != FailureTimeout {
		code := FailureExecution
		if results[0].Failure != nil {
			code = results[0].Failure.Code
		}
		t.Fatalf("timeout result status=%q code=%q result=%#v", results[0].Status, code, results)
	}
}
