package agent

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/llm"
)

func TestPersistentSummaryTranscriptIncludesBoundedToolEvidence(t *testing.T) {
	transcript := persistentSummaryTranscript([]llm.Message{
		{Role: "user", Content: "查 trace abc123"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call-1", Function: llm.ToolFunction{Name: "observe_logs", Arguments: `{"trace_id":"abc123"}`},
		}}},
		{Role: "tool", ToolCallID: "call-1", Name: "observe_logs", Content: strings.Repeat("x", sessionToolResultLimit+100)},
		{Role: "assistant", Content: "查询失败，无法确认线上状态。"},
	})

	for _, want := range []string{"trace abc123", "assistant tool_call observe_logs", `"trace_id":"abc123"`, "tool observe_logs", "查询失败"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript missing %q: %s", want, transcript)
		}
	}
	if strings.Count(transcript, "x") > sessionToolResultLimit {
		t.Fatalf("tool result was not bounded: %d chars", strings.Count(transcript, "x"))
	}
}
