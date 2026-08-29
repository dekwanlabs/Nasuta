package execution

import (
	"errors"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
)

func TestOutcomeForPreservesBudgetErrorCode(t *testing.T) {
	wrapped := errors.Join(errors.New("turn stopped"), agentapi.ErrBudgetExceeded)
	outcome := OutcomeFor(&RunResult{}, nil, wrapped)
	if outcome.Status != agentrun.StatusFailed || outcome.ErrorCode != "budget_exhausted" {
		t.Fatalf("outcome = %+v, want failed budget_exhausted", outcome)
	}
	if !errors.Is(outcome.Err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("outcome error = %v, want canonical budget cause", outcome.Err)
	}
}

func TestOutcomeForKeepsNonBudgetRuntimeError(t *testing.T) {
	err := errors.New("provider unavailable")
	outcome := OutcomeFor(&RunResult{}, nil, err)
	if outcome.Status != agentrun.StatusFailed || outcome.ErrorCode != "runtime_failed" {
		t.Fatalf("outcome = %+v, want failed runtime_failed", outcome)
	}
}
