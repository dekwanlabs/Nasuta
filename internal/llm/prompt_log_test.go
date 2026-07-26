package llm

import (
	"strings"
	"testing"
)

func TestJoinDynamicPromptMessagesOmitsBasePromptAndPreservesRuntimeMessages(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "base system rules"},
		{Role: "system", Content: "retrieved runtime context"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call-1", Type: "function",
			Function: ToolFunction{Name: "search_code", Arguments: `{"query":"RunOutcome"}`},
		}}},
		{Role: "tool", Name: "search_code", ToolCallID: "call-1", Content: "tool result"},
		{Role: "user", Content: "question"},
	}

	got := joinDynamicPromptMessages(messages)
	wants := []string{
		"message 2 role=system", "retrieved runtime context",
		"message 3 role=assistant", `"name":"search_code"`,
		"message 4 role=tool name=search_code tool_call_id=call-1", "tool result",
		"message 5 role=user", "question",
	}
	position := -1
	for _, want := range wants {
		next := strings.Index(got[position+1:], want)
		if next < 0 {
			t.Fatalf("joined prompt missing %q:\n%s", want, got)
		}
		position += next + 1
	}
	if strings.Contains(got, "base system rules") || strings.Contains(got, "message 1 role=system") {
		t.Fatalf("dynamic prompt contains base system prompt:\n%s", got)
	}
}

func TestJoinDynamicPromptMessagesHandlesNoDynamicMessages(t *testing.T) {
	for _, messages := range [][]Message{nil, {{Role: "system", Content: "base prompt"}}} {
		if got := joinDynamicPromptMessages(messages); got != "(no dynamic messages)" {
			t.Fatalf("joinDynamicPromptMessages(%#v) = %q", messages, got)
		}
	}
}

func TestJoinDynamicPromptMessagesKeepsFirstNonSystemMessage(t *testing.T) {
	got := joinDynamicPromptMessages([]Message{{Role: "user", Content: "question"}})
	if !strings.Contains(got, "message 1 role=user") || !strings.Contains(got, "question") {
		t.Fatalf("dynamic prompt = %q", got)
	}
}
