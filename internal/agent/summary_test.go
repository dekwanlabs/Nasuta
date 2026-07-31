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
	recordCount := turnSummaryBatchSize*2 + 1
	batchCount := (recordCount + turnSummaryBatchSize - 1) / turnSummaryBatchSize
	expectedPeak := int32(min(batchCount, turnSummaryBatchWorkers))
	var calls atomic.Int32
	var inFlight atomic.Int32
	var peakInFlight atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages  []llm.Message `json:"messages"`
			MaxTokens int           `json:"max_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			peak := peakInFlight.Load()
			if current <= peak || peakInFlight.CompareAndSwap(peak, current) {
				break
			}
		}
		if calls.Add(1) == expectedPeak {
			close(release)
		}
		<-release
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
		if len(input) > turnSummaryBatchSize {
			t.Errorf("summary batch contains %d turns, limit %d", len(input), turnSummaryBatchSize)
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

	records := make([]memory.TurnContextRecord, recordCount)
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
	if calls.Load() != int32(batchCount) || len(summaries) != len(records) {
		t.Fatalf("calls = %d, summaries = %d", calls.Load(), len(summaries))
	}
	if peakInFlight.Load() != expectedPeak {
		t.Fatalf("peak concurrent batches = %d, want %d", peakInFlight.Load(), expectedPeak)
	}
}

func TestPersistentSummaryTranscriptIncludesBoundedToolEvidence(t *testing.T) {
	transcript := persistentSummaryTranscript([]llm.Message{
		{Role: "user", Content: "查 trace abc123"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call-1", Function: llm.ToolFunction{Name: "observe_logs", Arguments: `{"trace_id":"abc123"}`},
		}}},
		{Role: "tool", ToolCallID: "call-1", Name: "observe_logs", Content: strings.Repeat("x", summaryToolProjectionRunes+100)},
		{Role: "assistant", Content: "查询失败，无法确认线上状态。"},
	})

	for _, want := range []string{"trace abc123", "assistant tool_call observe_logs", `"trace_id":"abc123"`, "tool observe_logs", "查询失败"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript missing %q: %s", want, transcript)
		}
	}
	if strings.Count(transcript, "x") > summaryToolProjectionRunes {
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
