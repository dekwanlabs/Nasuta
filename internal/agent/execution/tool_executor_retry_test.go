package execution

import (
	"context"
	"errors"
	"strings"
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

func TestBoundedToolPromptPreservesAuthoritativeResultAndRequiredLiterals(t *testing.T) {
	content := "BEGIN\n" + strings.Repeat("large result ", 2000) + "SERIAL-42\nEND"
	prompt, artifactID := boundedToolPrompt(
		"run-1",
		"call-1",
		content,
		tool.AnswerContract{RequiredLiterals: []string{"SERIAL-42"}},
		512,
	)
	if artifactID == "" {
		t.Fatal("artifact id is empty")
	}
	if len(artifactID) > 64 {
		t.Fatalf("artifact id length = %d, want <= 64: %q", len(artifactID), artifactID)
	}
	if want := toolResultArtifactID("run-1", "call-1"); artifactID != want {
		t.Fatalf("artifact id = %q, want %q", artifactID, want)
	}
	_, otherRunArtifactID := boundedToolPrompt(
		"run-2",
		"call-1",
		content,
		tool.AnswerContract{RequiredLiterals: []string{"SERIAL-42"}},
		512,
	)
	if otherRunArtifactID == artifactID {
		t.Fatalf("artifact id must be scoped by run: %q", artifactID)
	}
	if len(prompt) > 512 {
		t.Fatalf("prompt bytes = %d, want <= 512", len(prompt))
	}
	if !strings.Contains(prompt, "SERIAL-42") {
		t.Fatalf("prompt omitted required literal: %q", prompt)
	}
	if !strings.Contains(prompt, "_nasuta_truncated") {
		t.Fatalf("prompt is not marked truncated: %q", prompt)
	}
}
