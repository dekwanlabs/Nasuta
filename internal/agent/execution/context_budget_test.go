package execution

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/llm"
)

func TestEnsureInputBudgetRejectsBeforeProviderCall(t *testing.T) {
	agent := &Agent{cfg: AgentConfig{
		ContextWindow: 2048, AnswerMaxTokens: 700, ConclusionMaxTokens: 800,
	}}
	err := agent.ensureInputBudget([]llm.Message{{Role: "user", Content: strings.Repeat("上下文", 2000)}}, nil)
	if err == nil || !strings.Contains(err.Error(), "before provider call") {
		t.Fatalf("error = %v", err)
	}
}
