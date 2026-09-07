package qa

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

// spyManagedRun records Finish calls while pretending to execute successfully.
// It is a narrow test double for the managed-run boundary QA owns after the
// lifecycle split, so the assertion can focus on the terminal being written
// exactly once regardless of what happens in the best-effort PostEffects step.
type spyManagedRun struct {
	finishCalls int
	lastError   *agentapi.RunError
	outcome     run.Outcome
	result      agentapi.RunResult
	executeErr  error
}

func (spy *spyManagedRun) Context(ctx context.Context) context.Context { return ctx }

func (spy *spyManagedRun) Execute(context.Context, agentapi.RunRequest) (agentapi.RunResult, error) {
	if spy.executeErr != nil {
		return agentapi.RunResult{}, spy.executeErr
	}
	return spy.result, nil
}

func (spy *spyManagedRun) Finish(runError *agentapi.RunError) error {
	spy.finishCalls++
	spy.lastError = runError
	return nil
}

func (spy *spyManagedRun) Outcome() run.Outcome { return spy.outcome }

// TestExecuteSubmittedRunPostEffectsFailureKeepsSingleTerminal proves that a
// memory consolidation failure during PostEffects never calls Finish again and
// never changes the already-written terminal. PostEffects is best-effort and
// runs strictly after finalization.
func TestExecuteSubmittedRunPostEffectsFailureKeepsSingleTerminal(t *testing.T) {
	// The helper LLM is reachable only for the memory probe and it fails there,
	// forcing PostEffects to take its error path after finalization.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "memory backend unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	svc := &Service{
		helperLLM: llm.NewLLMClientWithHTTP(server.URL, "key", "model", 256, server.Client()),
		// A non-nil store is required to enter the PostEffects memory path; it
		// deliberately has no semantic backend so recall would also fail.
		memory: memory.NewMemoryStore(db, nil, nil, 0),
	}

	prepared := &preparation{
		request:    Request{Question: "What causes a rainbow?", UserID: 42},
		definition: agentapi.Definition{},
	}
	conversation := execution.ConversationContext{}
	request := agentapi.RunRequest{RunID: "post-effects-run", Actor: agentapi.Actor{UserID: 42}}

	spy := &spyManagedRun{
		outcome: run.Outcome{Status: run.StatusDone, Answer: "refraction"},
		result:  agentapi.RunResult{Text: "refraction", Status: agentapi.RunSucceeded},
	}

	svc.executeSubmittedRun(context.Background(), spy, prepared, conversation, request)

	if spy.finishCalls != 1 {
		t.Fatalf("Finish calls = %d, want exactly 1", spy.finishCalls)
	}
	if spy.lastError != nil {
		t.Fatalf("Finish was called with error %+v after a successful execution", spy.lastError)
	}
	if !strings.Contains(spy.outcome.Answer, "refraction") {
		t.Fatalf("outcome was mutated during PostEffects: %+v", spy.outcome)
	}
}

// TestExecuteManagedRunBudgetExhaustedFinishesFailedOnce proves that when the
// managed run returns the canonical budget-exhausted error, QA classifies it
// as budget_exhausted and finishes the run exactly once without running
// finalization or post-effects.
func TestExecuteManagedRunBudgetExhaustedFinishesFailedOnce(t *testing.T) {
	svc := &Service{}
	prepared := &preparation{request: Request{Question: "q"}}
	request := agentapi.RunRequest{RunID: "budget-exhausted-run"}

	spy := &spyManagedRun{executeErr: agentapi.ErrBudgetExceeded}

	result, _, ok := svc.executeManagedRun(context.Background(), spy, prepared, request)
	if ok {
		t.Fatal("executeManagedRun reported success for budget exhaustion")
	}
	if result.Status != agentapi.RunFailed && result.Status != "" {
		t.Fatalf("result status = %s, want empty or failed", result.Status)
	}
	if spy.finishCalls != 1 {
		t.Fatalf("Finish calls = %d, want 1", spy.finishCalls)
	}
	if spy.lastError == nil || spy.lastError.Code != "budget_exhausted" {
		t.Fatalf("Finish error = %+v, want budget_exhausted", spy.lastError)
	}
}
