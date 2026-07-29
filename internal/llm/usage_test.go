package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestOpenAIEmptyResponseReportsTokenExhaustion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"content": "", "reasoning_content": "hidden"},
				"finish_reason": "length",
			}},
			"usage": map[string]any{
				"prompt_tokens": 4214, "completion_tokens": 4000, "total_tokens": 8214,
				"completion_tokens_details": map[string]any{"reasoning_tokens": 4000},
			},
		})
	}))
	defer server.Close()

	client := NewLLMClientWithHTTPAndProvider(server.URL, "key", "model", "openai", 4000, server.Client())
	_, err := client.ChatMax(t.Context(), "system", "user", 4000)
	var callErr *CallError
	if !errors.As(err, &callErr) || callErr.Kind != ErrKindEmpty ||
		callErr.FinishReason != "length" || callErr.OutputTokens != 4000 || callErr.ReasoningTokens != 4000 {
		t.Fatalf("error=%v callError=%+v", err, callErr)
	}
}

type captureUsageRecorder struct {
	calls []CallUsage
}

type captureLifecycleObserver struct {
	events []CallLifecycle
}

func (observer *captureLifecycleObserver) OnLLMCall(_ context.Context, _ string, event CallLifecycle) {
	observer.events = append(observer.events, event)
}

func (recorder *captureUsageRecorder) RecordLLMCall(_ context.Context, call CallUsage) error {
	recorder.calls = append(recorder.calls, call)
	return nil
}

func TestOpenAINonStreamingUsageIsRecorded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "answer"}}},
			"usage": map[string]any{
				"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18,
				"prompt_tokens_details":     map[string]any{"cached_tokens": 3},
				"completion_tokens_details": map[string]any{"reasoning_tokens": 2},
			},
		})
	}))
	defer server.Close()

	recorder := &captureUsageRecorder{}
	ctx := WithUsageRecorder(t.Context(), "run-1", recorder)
	ctx = WithUsagePhase(ctx, PhaseRoute)
	client := NewLLMClientWithHTTPAndProvider(server.URL, "key", "model", "openai", 100, nil)
	if _, err := client.ChatText(ctx, "system", "user", CallOptions{MaxTokens: 50}); err != nil {
		t.Fatalf("ChatText: %v", err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("recorded calls = %d, want 1", len(recorder.calls))
	}
	call := recorder.calls[0]
	if call.RunID != "run-1" || call.Phase != PhaseRoute || call.MaxOutputTokens != 50 {
		t.Fatalf("call metadata = %+v", call)
	}
	want := (Usage{InputTokens: 11, CachedInputTokens: 3, OutputTokens: 7, ReasoningTokens: 2, TotalTokens: 18})
	if call.Usage != want {
		t.Fatalf("usage = %+v, want %+v", call.Usage, want)
	}
}

func TestOpenAIStreamingUsageAndRequestOption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request chatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.StreamOptions == nil || !request.StreamOptions.IncludeUsage {
			t.Errorf("stream_options = %+v", request.StreamOptions)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":13,\"completion_tokens\":5,\"total_tokens\":18,\"completion_tokens_details\":{\"reasoning_tokens\":1}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := NewLLMClientWithHTTPAndProvider(server.URL, "key", "model", "openai", 100, nil)
	result, err := client.ChatWithToolsMax(t.Context(), []Message{{Role: "user", Content: "q"}}, nil, nil, 40)
	if err != nil {
		t.Fatalf("ChatWithToolsMax: %v", err)
	}
	want := (Usage{InputTokens: 13, OutputTokens: 5, ReasoningTokens: 1, TotalTokens: 18})
	if result.Usage != want || result.ReasoningTokens != 1 {
		t.Fatalf("result usage = %+v reasoning=%d, want %+v", result.Usage, result.ReasoningTokens, want)
	}
}

func TestLogicalCallLifecyclePublishesBoundaries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	observer := &captureLifecycleObserver{}
	ctx := WithCallLifecycleObserver(t.Context(), "run-live", observer)
	ctx = WithUsagePhase(ctx, PhaseAgentStep)
	client := NewLLMClientWithHTTPAndProvider(server.URL, "key", "model", "openai", 100, nil)
	if _, err := client.ChatWithToolsMax(ctx, []Message{{Role: "user", Content: "q"}}, nil, nil, 40); err != nil {
		t.Fatalf("ChatWithToolsMax: %v", err)
	}
	if len(observer.events) != 2 {
		t.Fatalf("events = %+v, want start and finish", observer.events)
	}
	started, finished := observer.events[0], observer.events[1]
	if started.CallSeq != 1 || started.Phase != PhaseAgentStep || started.Status != CallLifecycleStarted {
		t.Fatalf("started = %+v", started)
	}
	if finished.CallSeq != 1 || finished.Phase != PhaseAgentStep || finished.Status != CallLifecycleFinished {
		t.Fatalf("finished = %+v", finished)
	}
}

func TestOpenAIStreamingRetryRecordsPhysicalCalls(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	recorder := &captureUsageRecorder{}
	ctx := WithUsagePhase(WithUsageRecorder(t.Context(), "run-retry", recorder), PhaseAgentStep)
	client := NewLLMClientWithHTTPAndProvider(server.URL, "key", "model", "openai", 100, nil)
	if _, err := client.ChatWithToolsMax(ctx, []Message{{Role: "user", Content: "q"}}, nil, nil, 40); err != nil {
		t.Fatalf("ChatWithToolsMax: %v", err)
	}
	if len(recorder.calls) != 2 {
		t.Fatalf("recorded calls = %d, want failed attempt and successful retry", len(recorder.calls))
	}
	if recorder.calls[0].Status != CallStatusFailed || recorder.calls[0].Usage != (Usage{}) {
		t.Fatalf("first call = %+v, want failed zero-usage attempt", recorder.calls[0])
	}
	if recorder.calls[1].Status != CallStatusSucceeded || recorder.calls[1].Usage.TotalTokens != 5 {
		t.Fatalf("second call = %+v, want successful retry usage", recorder.calls[1])
	}
}

func TestAnthropicStreamingUsageIsMerged(t *testing.T) {
	result := &ChatStreamResult{}
	provider := anthropicProvider{}
	acc := newInputJSONAccumulator()
	start := `{"message":{"usage":{"input_tokens":17,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}}`
	if err := provider.handleSSEEvent("message_start", start, result, nil, nil, acc); err != nil {
		t.Fatalf("message_start: %v", err)
	}
	delta := `{"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`
	if err := provider.handleSSEEvent("message_delta", delta, result, nil, nil, acc); err != nil {
		t.Fatalf("message_delta: %v", err)
	}
	want := (Usage{InputTokens: 22, CachedInputTokens: 5, OutputTokens: 9, TotalTokens: 31})
	if result.Usage != want {
		t.Fatalf("usage = %+v, want %+v", result.Usage, want)
	}
}
