package llm

import (
	"strings"
	"testing"
)

func TestJoinPromptMessagesPreservesRequestOrderAndMetadata(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "system rules"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call-1", Type: "function",
			Function: ToolFunction{Name: "search_code", Arguments: `{"query":"RunOutcome"}`},
		}}},
		{Role: "tool", Name: "search_code", ToolCallID: "call-1", Content: "tool result"},
		{Role: "user", Content: "question"},
	}

	got := joinPromptMessages(messages)
	wants := []string{
		"message 1 role=system", "system rules",
		"message 2 role=assistant", `"name":"search_code"`,
		"message 3 role=tool name=search_code tool_call_id=call-1", "tool result",
		"message 4 role=user", "question",
	}
	position := -1
	for _, want := range wants {
		next := strings.Index(got[position+1:], want)
		if next < 0 {
			t.Fatalf("joined prompt missing %q:\n%s", want, got)
		}
		position += next + 1
	}
}

func TestJoinPromptMessagesHandlesEmptyRequest(t *testing.T) {
	if got := joinPromptMessages(nil); got != "(no messages)" {
		t.Fatalf("joinPromptMessages(nil) = %q", got)
	}
}
