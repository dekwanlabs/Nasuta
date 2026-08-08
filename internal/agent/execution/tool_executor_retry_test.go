package execution

import (
	"context"
	"errors"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestToolExecutorAllowsRetryAfterFailure(t *testing.T) {
	tries := 0
	registry := testRegistry(t, testAgentTool("unstable", ToolKindRead, func(context.Context, tool.Arguments) (string, error) {
		tries++
		if tries == 1 {
			return "", errors.New("temporary failure")
		}
		return "ok", nil
	}))
	executor := NewToolExecutor(registry)
	seen := map[string]bool{}
	call := llm.ToolCall{ID: "1", Function: llm.ToolFunction{Name: "unstable", Arguments: `{}`}}
	policy := ToolPolicyForRun(true)
	first := executor.ExecuteWithPolicy(context.Background(), policy, call, seen)
	second := executor.ExecuteWithPolicy(context.Background(), policy, call, seen)
	if first.AuthoritativeContent != "error: temporary failure" || first.Evidence ||
		second.AuthoritativeContent != "ok" || !second.Evidence || tries != 2 {
		t.Fatalf(
			"retry behavior = first:%q second:%q tries:%d",
			first.AuthoritativeContent,
			second.AuthoritativeContent,
			tries,
		)
	}
}
