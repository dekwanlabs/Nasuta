package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

func TestGenerateTurnCompactionSummariesBatchesLargeRanges(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages  []llm.Message `json:"messages"`
			MaxTokens int           `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		calls++
		if request.MaxTokens != turnSummaryBatchMaxTokens {
			t.Errorf("max tokens = %d, want %d", request.MaxTokens, turnSummaryBatchMaxTokens)
		}
		user := request.Messages[len(request.Messages)-1].Content
		if strings.Contains(user, "cmp-") {
			t.Errorf("opaque refs leaked into summary prompt")
		}
		items := make([]map[string]any, 0, turnSummaryBatchSize)
		for _, line := range strings.Split(user, "\n") {
			if !strings.HasPrefix(line, "ITEM ") {
				continue
			}
			item := strings.Fields(line)[1]
			items = append(items, map[string]any{"item": len(items) + 1, "text": "summary " + item})
		}
		content, err := json.Marshal(items)
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
			Ref: "cmp-" + string(rune('a'+i)), TurnNumber: i + 1, Text: "turn detail",
		}
	}
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 100, server.Client())
	summaries, err := GenerateTurnCompactionSummaries(t.Context(), client, records)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(summaries) != len(records) {
		t.Fatalf("calls = %d, summaries = %d", calls, len(summaries))
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
	if _, err := parseTurnSummaries(`[{"item":1,"text":"first"}]`, records); err == nil {
		t.Fatal("missing item was accepted")
	}
	if _, err := parseTurnSummaries(`[{"item":1,"text":"first"},{"item":3,"text":"other"}]`, records); err == nil {
		t.Fatal("unknown item was accepted")
	}
	got, err := parseTurnSummaries(`[{"item":1,"text":"first"},{"item":2,"text":"second"}]`, records)
	if err != nil {
		t.Fatal(err)
	}
	if got["cmp-1"] != "first" || got["cmp-2"] != "second" {
		t.Fatalf("summaries = %#v", got)
	}
}

func TestBuildRollingSummaryKeepsOneInstruction(t *testing.T) {
	previous := "ref=cmp-old, text=old finding\n" + rollingSummaryInstruction
	got := buildRollingSummary(previous, []memory.TurnContextRecord{
		{Ref: "cmp-new", SummaryText: "new finding"},
	})
	for _, want := range []string{"ref=cmp-old, text=old finding", "ref=cmp-new, text=new finding", rollingSummaryInstruction} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q: %s", want, got)
		}
	}
	if strings.Count(got, rollingSummaryInstructionPrefix) != 1 {
		t.Fatalf("instruction count mismatch: %s", got)
	}
}
