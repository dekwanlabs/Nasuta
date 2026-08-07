package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseLine builds one Anthropic SSE frame: an `event:` line followed by a
// `data:` line, terminated with a blank line.
func sseLine(event string, payload any) string {
	data, _ := json.Marshal(payload)
	return "event: " + event + "\ndata: " + string(data) + "\n\n"
}

// newAnthropicFakeServer serves the given SSE frames on POST /v1/messages,
// and records every request body received (for translation assertions).
func newAnthropicFakeServer(term *testing.T, frames []string) (*httptest.Server, func() []byte) {
	term.Helper()
	var mu sync.Mutex
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, row *http.Request) {
		body, _ := io.ReadAll(row.Body)
		mu.Lock()
		lastBody = body
		mu.Unlock()

		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		for _, file := range frames {
			writer.Write([]byte(file))
			writer.(http.Flusher).Flush()
		}
	}))
	return srv, func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return lastBody
	}
}

func newAnthropicClient(term1 *testing.T, url string) *LLMClient {
	term1.Helper()
	return NewLLMClientWithHTTPAndProvider(url, "k", "claude-opus-4-8", "anthropic", 100, &http.Client{})
}

// TestAnthropicChat_NonStreaming verifies Chat parses a non-streaming
// /v1/messages response by concatenating text blocks.
func TestAnthropicChat_NonStreaming(term2 *testing.T) {
	// Chat uses a non-streaming JSON body, not SSE. Serve the full message JSON.
	srv := httptest.NewServer(http.HandlerFunc(func(writer1 http.ResponseWriter, row1 *http.Request) {
		resp := map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "Hello, "},
				{"type": "text", "text": "world"},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens": 12, "output_tokens": 4,
				"cache_creation_input_tokens": 2, "cache_read_input_tokens": 3,
			},
		}
		writer1.Header().Set("Content-Type", "application/json")
		json.NewEncoder(writer1).Encode(resp)
	}))
	defer srv.Close()

	client := newAnthropicClient(term2, srv.URL)
	recorder := &captureUsageRecorder{}
	ctx := WithUsagePhase(WithUsageRecorder(term2.Context(), "run-anthropic", recorder), PhaseRoute)
	out, err := client.Chat(ctx, "you are helpful", "hi")
	if err != nil {
		term2.Fatalf("Chat: %v", err)
	}
	if out != "Hello, world" {
		term2.Fatalf("content = %q, want %q", out, "Hello, world")
	}
	wantUsage := Usage{InputTokens: 17, CachedInputTokens: 5, OutputTokens: 4, TotalTokens: 21}
	if len(recorder.calls) != 1 || recorder.calls[0].Usage != wantUsage {
		term2.Fatalf("recorded usage = %+v, want %+v", recorder.calls, wantUsage)
	}
	if recorder.calls[0].MaxOutputTokens != 100 {
		term2.Fatalf("max output tokens = %d, want client default 100", recorder.calls[0].MaxOutputTokens)
	}
}

func TestAnthropicChatJSONUsesProviderAndReprompt(t *testing.T) {
	var requests []anthropicRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		var request anthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requests = append(requests, request)
		content := "not json"
		if len(requests) == 2 {
			content = `{"value":2}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": content}},
		})
	}))
	defer server.Close()

	client := newAnthropicClient(t, server.URL)
	var output map[string]any
	err := client.ChatJSON(t.Context(), "system", "user", &output, CallOptions{
		MaxAttempts: 2,
		Backoff:     time.Millisecond,
	})
	if err != nil {
		t.Fatalf("ChatJSON: %v", err)
	}
	if output["value"] != float64(2) || len(requests) != 2 {
		t.Fatalf("output = %#v requests = %d", output, len(requests))
	}
	if len(requests[1].Messages) != 3 || requests[1].Messages[1].Role != "assistant" {
		t.Fatalf("reprompt messages = %#v", requests[1].Messages)
	}
}

func TestAnthropicChatTextUsesProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "answer"}},
		})
	}))
	defer server.Close()

	client := newAnthropicClient(t, server.URL)
	answer, err := client.ChatText(t.Context(), "system", "user", CallOptions{})
	if err != nil {
		t.Fatalf("ChatText: %v", err)
	}
	if answer != "answer" {
		t.Fatalf("answer = %q", answer)
	}
}

// TestAnthropicStopReasonMapping verifies Anthropic stop_reason values are
// normalized onto the shared FinishReason vocabulary.
func TestAnthropicStopReasonMapping(term3 *testing.T) {
	cases := []struct{ anthropic, want string }{
		{"max_tokens", FinishLength},
		{"end_turn", FinishStop},
		{"tool_use", FinishToolCalls},
		{"stop_sequence", FinishStop},
	}
	for _, tc := range cases {
		term3.Run(tc.anthropic, func(term4 *testing.T) {
			frames := []string{
				sseLine("message_start", map[string]any{"type": "message_start"}),
				sseLine("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}}),
				sseLine("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "x"}}),
				sseLine("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
				sseLine("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": tc.anthropic}}),
				sseLine("message_stop", map[string]any{"type": "message_stop"}),
			}
			srv, _ := newAnthropicFakeServer(term4, frames)
			defer srv.Close()

			client := newAnthropicClient(term4, srv.URL)
			res, err := client.ChatWithToolsMax(term4.Context(), []Message{{Role: "user", Content: "q"}}, nil, nil, 100)
			if err != nil {
				term4.Fatalf("ChatWithToolsMax: %v", err)
			}
			if res.FinishReason != tc.want {
				term4.Fatalf("stop_reason %q -> FinishReason = %q, want %q", tc.anthropic, res.FinishReason, tc.want)
			}
		})
	}
}

// TestAnthropicToolUseInputJSONDeltaAccumulation verifies tool_use input arrives
// as input_json_delta fragments that are concatenated and parsed into one
// complete ToolCall, fired once via OnToolCall.
func TestAnthropicToolUseInputJSONDeltaAccumulation(term5 *testing.T) {
	frames := []string{
		sseLine("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "get_weather"}}),
		sseLine("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"city":`}}),
		sseLine("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `"Paris"`}}),
		sseLine("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `}`}}),
		sseLine("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sseLine("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}}),
		sseLine("message_stop", map[string]any{"type": "message_stop"}),
	}
	srv, _ := newAnthropicFakeServer(term5, frames)
	defer srv.Close()

	client := newAnthropicClient(term5, srv.URL)
	item := &captureHandler{}
	res, err := client.ChatWithToolsMax(term5.Context(), []Message{{Role: "user", Content: "weather?"}}, nil, item, 100)
	if err != nil {
		term5.Fatalf("ChatWithToolsMax: %v", err)
	}
	if len(res.ToolCalls) != 1 {
		term5.Fatalf("ToolCalls = %d, want 1", len(res.ToolCalls))
	}
	call := res.ToolCalls[0]
	if call.ID != "toolu_1" || call.Function.Name != "get_weather" {
		term5.Fatalf("tool call = %+v", call)
	}
	want := `{"city":"Paris"}`
	if call.Function.Arguments != want {
		term5.Fatalf("arguments = %q, want %q", call.Function.Arguments, want)
	}
	if len(item.toolCalls) != 1 {
		term5.Fatalf("OnToolCall fired %d times, want 1", len(item.toolCalls))
	}
}

// TestAnthropicThinkingBlock_ReasoningStreamedCounted verifies thinking_delta
// fragments accumulate and stream live. Anthropic usage does not expose a
// separate thinking-token detail, so ReasoningTokens remains zero.
func TestAnthropicThinkingBlock_ReasoningStreamedCounted(term6 *testing.T) {
	frames := []string{
		sseLine("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking"}}),
		sseLine("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "先思考"}}),
		sseLine("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "再思考"}}),
		sseLine("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sseLine("content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text"}}),
		sseLine("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "答案"}}),
		sseLine("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1}),
		sseLine("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}),
		sseLine("message_stop", map[string]any{"type": "message_stop"}),
	}
	srv, _ := newAnthropicFakeServer(term6, frames)
	defer srv.Close()

	client := newAnthropicClient(term6, srv.URL)
	item1 := &captureHandler{}
	res, err := client.ChatWithToolsMax(term6.Context(), []Message{{Role: "user", Content: "q"}}, nil, item1, 100)
	if err != nil {
		term6.Fatalf("ChatWithToolsMax: %v", err)
	}
	if res.Reasoning != "先思考再思考" {
		term6.Fatalf("reasoning = %q, want 先思考再思考", res.Reasoning)
	}
	if res.ReasoningTokens != 0 {
		term6.Fatalf("reasoning tokens = %d, want provider-reported zero", res.ReasoningTokens)
	}
	if res.Content != "答案" {
		term6.Fatalf("content = %q, want 答案", res.Content)
	}
	if got, want := strings.Join(item1.reasoning, ""), "先思考再思考"; got != want {
		term6.Fatalf("streamed reasoning = %q, want %q", got, want)
	}
	if len(item1.tokens) != 1 || item1.tokens[0] != "答案" {
		term6.Fatalf("tokens = %v, want only the 1 visible delta", item1.tokens)
	}
}

// TestAnthropicMessageTranslation verifies translateMessages lifts system to
// top-level, converts assistant tool calls to tool_use blocks, and merges
// consecutive tool-result messages into one user turn.
func TestAnthropicMessageTranslation(term7 *testing.T) {
	frames := []string{
		sseLine("message_start", map[string]any{"type": "message_start"}),
		sseLine("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text"}}),
		sseLine("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "ok"}}),
		sseLine("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sseLine("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}),
		sseLine("message_stop", map[string]any{"type": "message_stop"}),
	}
	srv, bodyOf := newAnthropicFakeServer(term7, frames)
	defer srv.Close()

	client := newAnthropicClient(term7, srv.URL)
	messages := []Message{
		{Role: "system", Content: "you are an agent"},
		{Role: "system", Content: "extra rules"},
		{Role: "user", Content: "what's the weather?"},
		{Role: "assistant", Content: "let me check", ToolCalls: []ToolCall{
			{ID: "toolu_1", Type: "function", Function: ToolFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`}},
		}},
		{Role: "tool", ToolCallID: "toolu_1", Name: "get_weather", Content: "sunny"},
		{Role: "tool", ToolCallID: "toolu_2", Name: "get_time", Content: "noon"}, // parallel result, same turn
	}
	if _, err := client.ChatWithToolsMax(term7.Context(), messages, nil, nil, 100); err != nil {
		term7.Fatalf("ChatWithToolsMax: %v", err)
	}

	var req anthropicRequest
	if err := json.Unmarshal(bodyOf(), &req); err != nil {
		term7.Fatalf("unmarshal request: %v", err)
	}

	// Without tools the system stays a plain JSON string.
	var sys string
	if err := json.Unmarshal(req.System, &sys); err != nil {
		term7.Fatalf("system is not a string: %v", err)
	}
	if sys != "you are an agent\n\nextra rules" {
		term7.Fatalf("system = %q, want two parts joined", sys)
	}
	if len(req.Messages) != 3 {
		term7.Fatalf("messages = %d, want 3 (user, assistant, merged-tool-results user)", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		term7.Fatalf("messages[0].role = %q, want user (system must be lifted out)", req.Messages[0].Role)
	}

	// Assistant turn carries text + tool_use.
	asst := req.Messages[1]
	if asst.Role != "assistant" || len(asst.Content) != 2 {
		term7.Fatalf("assistant turn = %+v, want 2 blocks", asst)
	}
	if asst.Content[0].Type != "text" || asst.Content[0].Text != "let me check" {
		term7.Fatalf("assistant text block = %+v", asst.Content[0])
	}
	if asst.Content[1].Type != "tool_use" || asst.Content[1].ID != "toolu_1" || asst.Content[1].Name != "get_weather" {
		term7.Fatalf("assistant tool_use block = %+v", asst.Content[1])
	}
	if got := asst.Content[1].Input["city"]; got != "Paris" {
		term7.Fatalf("tool_use input = %+v, want city=Paris", asst.Content[1].Input)
	}

	// Both tool results merged into one user message with two tool_result blocks.
	merged := req.Messages[2]
	if merged.Role != "user" || len(merged.Content) != 2 {
		term7.Fatalf("merged tool-result turn = %+v, want user with 2 tool_result blocks", merged)
	}
	for idx, buf := range merged.Content {
		if buf.Type != "tool_result" {
			term7.Fatalf("block %d type = %q, want tool_result", idx, buf.Type)
		}
	}
	if merged.Content[0].ToolUseID != "toolu_1" || merged.Content[1].ToolUseID != "toolu_2" {
		term7.Fatalf("merged tool_use_ids = %q/%q", merged.Content[0].ToolUseID, merged.Content[1].ToolUseID)
	}
}

// TestAnthropicToolDefTranslation verifies OpenAI-shape ToolDefs become Anthropic
// {name, description, input_schema} (no "function" wrapper, input_schema not
// parameters), and tool_choice=auto is set when tools are present.
func TestAnthropicToolDefTranslation(term8 *testing.T) {
	frames := []string{
		sseLine("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}),
		sseLine("message_stop", map[string]any{"type": "message_stop"}),
	}
	srv, bodyOf := newAnthropicFakeServer(term8, frames)
	defer srv.Close()

	client := newAnthropicClient(term8, srv.URL)
	tools := []ToolDef{{
		Type: "function",
		Function: ToolFunctionDef{
			Name:        "get_weather",
			Description: "Get weather",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		},
	}}
	if _, err := client.ChatWithToolsMax(term8.Context(), []Message{{Role: "user", Content: "q"}}, tools, nil, 100); err != nil {
		term8.Fatalf("ChatWithToolsMax: %v", err)
	}

	var req anthropicRequest
	if err := json.Unmarshal(bodyOf(), &req); err != nil {
		term8.Fatalf("unmarshal request: %v", err)
	}
	if len(req.Tools) != 1 {
		term8.Fatalf("tools = %d, want 1", len(req.Tools))
	}
	t0 := req.Tools[0]
	if t0.Name != "get_weather" || t0.Description != "Get weather" {
		term8.Fatalf("tool = %+v", t0)
	}
	if t0.InputSchema == nil || t0.InputSchema["type"] != "object" {
		term8.Fatalf("input_schema = %+v, want the parameters map", t0.InputSchema)
	}
	if req.ToolChoice == nil || req.ToolChoice.Type != "auto" {
		term8.Fatalf("tool_choice = %+v, want auto", req.ToolChoice)
	}
	// The last tool carries the cache breakpoint, and the system prompt is a
	// single cacheable block so tools+system cache together across steps.
	if t0.CacheControl == nil || t0.CacheControl.Type != "ephemeral" {
		term8.Fatalf("tool cache_control = %+v, want ephemeral on last tool", t0.CacheControl)
	}
	var blocks []anthropicSystemBlock
	if err := json.Unmarshal(req.System, &blocks); err != nil {
		term8.Fatalf("system is not a block array: %v", err)
	}
	if len(blocks) != 1 || blocks[0].CacheControl == nil || blocks[0].CacheControl.Type != "ephemeral" {
		term8.Fatalf("system blocks = %+v, want one cacheable block", blocks)
	}
}

func TestAnthropicModelParametersUseAnthropicWireNames(t *testing.T) {
	frames := []string{
		sseLine("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"},
		}),
		sseLine("message_stop", map[string]any{"type": "message_stop"}),
	}
	srv, bodyOf := newAnthropicFakeServer(t, frames)
	defer srv.Close()

	client := newAnthropicClient(t, srv.URL)
	temperature := 0.2
	topP := 0.8
	topK := 32
	if _, err := client.ChatWithToolsMaxWithParameters(
		t.Context(), []Message{{Role: "user", Content: "q"}}, nil, nil, 100,
		ModelParameters{
			Temperature: &temperature, TopP: &topP,
			Stop: []string{"END"}, TopK: &topK,
		},
	); err != nil {
		t.Fatalf("ChatWithToolsMaxWithParameters: %v", err)
	}

	var request anthropicRequest
	if err := json.Unmarshal(bodyOf(), &request); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if request.Temperature == nil || *request.Temperature != temperature ||
		request.TopP == nil || *request.TopP != topP ||
		len(request.StopSequences) != 1 || request.StopSequences[0] != "END" ||
		request.TopK == nil || *request.TopK != topK {
		t.Fatalf("request parameters = %+v", request)
	}
}

func TestAnthropicCacheControlOnlyOnLastTool(term *testing.T) {
	frames := []string{
		sseLine("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}),
		sseLine("message_stop", map[string]any{"type": "message_stop"}),
	}
	srv, bodyOf := newAnthropicFakeServer(term, frames)
	defer srv.Close()

	client := newAnthropicClient(term, srv.URL)
	tools := []ToolDef{
		{Type: "function", Function: ToolFunctionDef{Name: "first", Description: "First", Parameters: map[string]any{"type": "object"}}},
		{Type: "function", Function: ToolFunctionDef{Name: "second", Description: "Second", Parameters: map[string]any{"type": "object"}}},
	}
	if _, err := client.ChatWithToolsMax(term.Context(), []Message{{Role: "user", Content: "q"}}, tools, nil, 100); err != nil {
		term.Fatalf("ChatWithToolsMax: %v", err)
	}
	var req anthropicRequest
	if err := json.Unmarshal(bodyOf(), &req); err != nil {
		term.Fatalf("unmarshal request: %v", err)
	}
	if len(req.Tools) != 2 {
		term.Fatalf("tools = %d, want 2", len(req.Tools))
	}
	if req.Tools[0].CacheControl != nil {
		term.Fatalf("first tool cache_control = %+v, want nil", req.Tools[0].CacheControl)
	}
	if req.Tools[1].CacheControl == nil || req.Tools[1].CacheControl.Type != "ephemeral" {
		term.Fatalf("last tool cache_control = %+v, want ephemeral", req.Tools[1].CacheControl)
	}
}

// captureHandler records streamed tokens, reasoning deltas, and tool calls.
type captureHandler struct {
	tokens    []string
	reasoning []string
	toolCalls []ToolCall
}

func (cfg *captureHandler) OnToken(token string)      { cfg.tokens = append(cfg.tokens, token) }
func (cfg1 *captureHandler) OnToolCall(call ToolCall) { cfg1.toolCalls = append(cfg1.toolCalls, call) }
func (cfg2 *captureHandler) OnReasoning(token string) { cfg2.reasoning = append(cfg2.reasoning, token) }
