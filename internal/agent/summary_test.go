package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

func TestGenerateTurnCompactionSummariesBatchesLargeRanges(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages  []llm.Message `json:"messages"`
			MaxTokens int           `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		calls.Add(1)
		if request.MaxTokens != turnSummaryBatchMaxTokens {
			t.Errorf("max tokens = %d, want %d", request.MaxTokens, turnSummaryBatchMaxTokens)
		}
		user := request.Messages[len(request.Messages)-1].Content
		if strings.Contains(user, "cmp-") {
			t.Errorf("opaque refs leaked into summary prompt")
		}
		var input []struct {
			Item int `json:"item"`
		}
		payload := user[strings.IndexByte(user, '['):]
		if err := json.Unmarshal([]byte(payload), &input); err != nil {
			t.Errorf("decode summary input: %v", err)
			return
		}
		items := make([]map[string]any, 0, len(input))
		for _, item := range input {
			items = append(items, map[string]any{"item": item.Item, "text": fmt.Sprintf("summary %d", item.Item)})
		}
		content, err := json.Marshal(map[string]any{"items": items})
		if err != nil {
			t.Errorf("marshal content: %v", err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": string(content)}}},
		})
	}))
	defer server.Close()

	records := make([]memory.TurnContextRecord, turnSummaryBatchSize*2+1)
	for i := range records {
		records[i] = memory.TurnContextRecord{
			Ref: "cmp-" + string(rune('a'+i)), TurnNumber: i + 1,
			DetailJSON: []byte(fmt.Sprintf(`{"version":1,"turn":%d}`, i+1)),
		}
	}
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 100, server.Client())
	summaries, err := GenerateTurnCompactionSummaries(t.Context(), client, records)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || len(summaries) != len(records) {
		t.Fatalf("calls = %d, summaries = %d", calls.Load(), len(summaries))
	}
}

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

func TestParseTurnSummariesRequiresExactItemSet(t *testing.T) {
	records := []memory.TurnContextRecord{
		{Ref: "cmp-1", SessionID: "session-1", UserID: 42, TurnNumber: 1},
		{Ref: "cmp-2", SessionID: "session-1", UserID: 42, TurnNumber: 2},
	}
	if _, err := parseTurnSummaries(`{"items":[{"item":1,"text":"first"}]}`, records); err == nil {
		t.Fatal("missing item was accepted")
	}
	if _, err := parseTurnSummaries(`{"items":[{"item":1,"text":"first"},{"item":3,"text":"other"}]}`, records); err == nil {
		t.Fatal("unknown item was accepted")
	}
	got, err := parseTurnSummaries(`{"items":[{"item":1,"text":"first"},{"item":2,"text":"second"}]}`, records)
	if err != nil {
		t.Fatal(err)
	}
	if got["cmp-1"] != "first" || got["cmp-2"] != "second" {
		t.Fatalf("summaries = %#v", got)
	}
}

func TestParseTurnSummariesBoundsModelTextLocally(t *testing.T) {
	records := []memory.TurnContextRecord{{Ref: "cmp-1", TurnNumber: 1}}
	raw, err := json.Marshal(turnSummaryResponse{Items: []turnSummaryItem{{
		Item: 1, Text: strings.Repeat("压缩摘要内容", 100),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseTurnSummaries(string(raw), records)
	if err != nil {
		t.Fatal(err)
	}
	if tokens := tooloutput.EstimateTokens(got["cmp-1"]); tokens > turnSummaryTokenLimit {
		t.Fatalf("bounded summary uses %d tokens, limit %d", tokens, turnSummaryTokenLimit)
	}
}

func TestGenerateSessionStateProducesBoundedCanonicalJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.MaxTokens != 5120 {
			t.Errorf("max tokens = %d, want 5120", request.MaxTokens)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": `{"version":2,"updatedThroughTurn":2,"goals":[{"text":"ship history recall","refs":["cmp-new"]}],"constraints":[],"decisions":[],"activeEntities":["session_history"],"openItems":[]}`}}}})
	}))
	defer server.Close()
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 100, server.Client())
	previous := `{"version":2,"updatedThroughTurn":1,"goals":[{"text":"old goal","refs":["cmp-old"]}],"constraints":[],"decisions":[],"activeEntities":[],"openItems":[]}`
	got, err := generateSessionState(t.Context(), client, previous, 1, []memory.TurnContextRecord{
		{Ref: "cmp-new", TurnNumber: 2, SummaryText: "new finding"},
	}, 5120)
	if err != nil {
		t.Fatal(err)
	}
	var state sessionState
	if err := json.Unmarshal([]byte(got), &state); err != nil {
		t.Fatal(err)
	}
	if state.UpdatedThroughTurn != 2 || len(state.Goals) != 1 || state.Goals[0].Refs[0] != "cmp-new" {
		t.Fatalf("state = %+v", state)
	}
	if strings.Contains(got, "instruction") {
		t.Fatalf("behavior instruction leaked into stored JSON: %s", got)
	}
}

func TestCanonicalizeSessionStateBoundsModelOutput(t *testing.T) {
	items := make([]sessionStateItem, sessionStateCategoryLimit+2)
	for i := range items {
		items[i] = sessionStateItem{Text: strings.Repeat("detail ", 100), Refs: []string{" cmp-1 ", "cmp-2", "cmp-3", "cmp-4"}}
	}
	state := sessionState{Goals: items, Constraints: items, Decisions: items, OpenItems: items,
		ActiveEntities: make([]string, sessionStateEntityLimit+2)}
	canonicalizeSessionState(&state)
	if len(state.Goals) != sessionStateGoalLimit || len(state.Constraints) != sessionStateCategoryLimit ||
		len(state.Decisions) != sessionStateCategoryLimit || len(state.OpenItems) != sessionStateCategoryLimit ||
		len(state.ActiveEntities) != sessionStateEntityLimit {
		t.Fatalf("state was not bounded: %+v", state)
	}
	if tooloutput.EstimateTokens(state.Goals[0].Text) > sessionStateTextTokenLimit ||
		len(state.Goals[0].Refs) != 3 || state.Goals[0].Refs[0] != "cmp-1" {
		t.Fatalf("item was not canonicalized: %+v", state.Goals[0])
	}
}

func TestFallbackSessionStateAdvancesBoundaryAndPreservesState(t *testing.T) {
	previous := `{"version":2,"updatedThroughTurn":4,"goals":[{"text":"goal","refs":["cmp-old"]}],"constraints":[],"decisions":[],"activeEntities":["service"],"openItems":[]}`
	got, err := fallbackSessionState(previous, 4, 24, 512)
	if err != nil {
		t.Fatal(err)
	}
	var state sessionState
	if err := json.Unmarshal([]byte(got), &state); err != nil {
		t.Fatal(err)
	}
	if state.UpdatedThroughTurn != 24 || len(state.Goals) != 1 || state.Goals[0].Text != "goal" {
		t.Fatalf("fallback state = %+v", state)
	}

	empty, err := fallbackSessionState("", 0, 24, 512)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(empty, "null") {
		t.Fatalf("empty fallback must use canonical arrays: %s", empty)
	}

	reset, err := fallbackSessionState(previous, 4, 24, tooloutput.EstimateTokens(empty))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(reset), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Goals) != 0 {
		t.Fatalf("over-budget previous state was retained: %+v", state)
	}
}
