// Package llm provides the OpenAI-compatible chat client used by QA.
package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httpclient"
	"github.com/go-resty/resty/v2"
)

// Message is an OpenAI-compatible chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// ToolCall is one model-requested function call.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction carries the function name and JSON-encoded arguments.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // raw JSON string, per OpenAI spec
}

// ToolDef is the OpenAI-compatible function tool declaration.
type ToolDef struct {
	Type     string          `json:"type"` // "function"
	Function ToolFunctionDef `json:"function"`
}

// ToolFunctionDef describes a function and its JSON schema parameters.
type ToolFunctionDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// LLMClient calls an OpenAI-compatible chat completions endpoint.
type LLMClient struct {
	baseURL   string
	apiKey    string
	model     string
	maxTokens int
	rc        *resty.Client
	provider  string // "openai" (default) | "anthropic"
}

func (lc *LLMClient) anthropic() anthropicProvider {
	return anthropicProvider{
		baseURL:   lc.baseURL,
		apiKey:    lc.apiKey,
		model:     lc.model,
		maxTokens: lc.maxTokens,
		rc:        lc.rc,
	}
}

func newRestyClient(apiKey string, hc *http.Client) *resty.Client {
	headers := map[string]string{
		"Content-Type": "application/json",
	}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	return httpclient.NewWithHTTP(hc, 300*time.Second, headers)
}

// NewLLMClientWithHTTP builds an LLMClient with a custom http.Transport.
func NewLLMClientWithHTTP(baseURL, apiKey, model string, maxTokens int, httpCli *http.Client) *LLMClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	apiKey = strings.TrimSpace(apiKey)
	model = strings.TrimSpace(model)
	rc := newRestyClient(apiKey, httpCli)
	return &LLMClient{
		baseURL:   baseURL,
		apiKey:    apiKey,
		model:     model,
		maxTokens: maxTokens,
		rc:        rc,
		provider:  "openai",
	}
}

// NewLLMClientWithHTTPAndProvider builds an LLM client with explicit provider.
func NewLLMClientWithHTTPAndProvider(baseURL, apiKey, model, provider string, maxTokens int, httpCli *http.Client) *LLMClient {
	lc := NewLLMClientWithHTTP(baseURL, apiKey, model, maxTokens, httpCli)
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic":
		lc.provider = "anthropic"
	default:
		lc.provider = "openai"
	}
	return lc
}

// chatRequest is the request body for /chat/completions.
type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []Message      `json:"messages"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Tools         []ToolDef      `json:"tools,omitempty"`
	ToolChoice    string         `json:"tool_choice,omitempty"` // "auto" | "none"
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Chat runs a single non-streaming LLM call with the client's default token budget.
func (lc *LLMClient) Chat(ctx context.Context, system, user string) (string, error) {
	return lc.ChatMax(ctx, system, user, 0)
}

// ChatMax runs a retrying non-streaming LLM call with an optional token override.
// maxTokens <= 0 leaves the budget unset (OpenAI omits max_tokens; Anthropic uses
// the client default). Use it for cheap structured helpers that must not let a
// reasoning model burn an unbounded budget on invisible thinking.
func (lc *LLMClient) ChatMax(ctx context.Context, system, user string, maxTokens int) (string, error) {
	return chatTextWith(ctx, lc.chatMessages, system, user, CallOptions{MaxTokens: maxTokens})
}

// chatMessages is the single non-streaming provider dispatcher.
func (lc *LLMClient) chatMessages(ctx context.Context, messages []Message, maxTokens int) (content string, callErr error) {
	recordedMaxTokens := maxTokens
	if lc.provider == "anthropic" && recordedMaxTokens <= 0 {
		recordedMaxTokens = lc.maxTokens
	}
	lc.logPrompt(ctx, messages, 0, recordedMaxTokens)
	finishLifecycle := beginCallLifecycle(ctx)
	defer func() { finishLifecycle(callErr) }()
	started := time.Now()
	var usage Usage
	defer func() {
		recordCallUsage(ctx, lc.provider, lc.model, recordedMaxTokens, started, usage, callErr)
	}()
	switch lc.provider {
	case "openai":
		content, usage, callErr = lc.chatMessagesOpenAI(ctx, messages, maxTokens)
	case "anthropic":
		content, usage, callErr = lc.anthropic().chatMessages(ctx, messages, recordedMaxTokens)
	default:
		callErr = fmt.Errorf("unsupported LLM provider %q", lc.provider)
	}
	return content, callErr
}

// chatMessagesOpenAI performs one OpenAI-compatible non-streaming call.
func (lc *LLMClient) chatMessagesOpenAI(ctx context.Context, messages []Message, maxTokens int) (string, Usage, error) {
	body := chatRequest{Model: lc.model, Messages: messages, MaxTokens: maxTokens, Stream: false}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage openAIUsage `json:"usage"`
	}
	resp, err := httpclient.Request(ctx, lc.rc).
		SetBody(body).
		Post(lc.baseURL + "/chat/completions")
	if err != nil {
		return "", Usage{}, &CallError{Kind: ErrKindNetwork, Err: err}
	}
	if resp.StatusCode() != http.StatusOK {
		return "", Usage{}, &CallError{
			Kind:       ErrKindStatus,
			Status:     resp.StatusCode(),
			Body:       boundedErrorBody(resp.Body()),
			RetryAfter: parseRetryAfter(resp.Header(), maxBackoff),
		}
	}
	if err := json.Unmarshal(resp.Body(), &result); err != nil {
		return "", Usage{}, &CallError{Kind: ErrKindEnvelope, Err: err}
	}
	if len(result.Choices) == 0 {
		return "", result.Usage.shared(), &CallError{Kind: ErrKindEmpty}
	}
	content := result.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" {
		return "", result.Usage.shared(), &CallError{Kind: ErrKindEmpty}
	}
	return content, result.Usage.shared(), nil
}

// StreamChat streams chat-completion deltas to tokenCh.
func (lc *LLMClient) StreamChat(ctx context.Context, messages []Message, tokenCh chan<- string, errCh chan<- error) {
	go func() {
		defer close(tokenCh)
		defer close(errCh)
		t0 := time.Now()
		if err := lc.streamChat(ctx, messages, tokenCh); err != nil {
			log.InfofCtx(ctx, "[qa] LLM stream error after %.1fs: %v", time.Since(t0).Seconds(), err)
			select {
			case errCh <- err:
			default:
			}
		} else {
			log.InfofCtx(ctx, "[qa] LLM stream done in %.1fs", time.Since(t0).Seconds())
		}
	}()
}

func (lc *LLMClient) streamChat(ctx context.Context, messages []Message, tokenCh chan<- string) error {
	lc.logPrompt(ctx, messages, 0, lc.maxTokens)
	body, err := json.Marshal(chatRequest{
		Model: lc.model, Messages: messages, MaxTokens: lc.maxTokens, Stream: true,
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	resp, err := httpclient.Request(ctx, lc.rc).
		SetHeader("Accept", "text/event-stream").
		SetBody(body).
		SetDoNotParseResponse(true).
		Post(lc.baseURL + "/chat/completions")
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	raw := resp.RawBody()
	if raw == nil {
		return fmt.Errorf("empty response body")
	}
	defer raw.Close()

	if resp.StatusCode() != http.StatusOK {
		b, _ := io.ReadAll(raw)
		return fmt.Errorf("LLM API %d: %s", resp.StatusCode(), strings.TrimSpace(string(b)))
	}

	scanner := bufio.NewScanner(raw)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			return nil
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case tokenCh <- ch.Delta.Content:
				}
			}
		}
	}
	return scanner.Err()
}

// ChatStreamResult is the aggregated outcome of one tool-aware streaming turn.
type ChatStreamResult struct {
	Content         string
	Reasoning       string // concatenated reasoning_content deltas
	ReasoningTokens int    // provider-reported output-token detail
	Usage           Usage
	ToolCalls       []ToolCall
	FinishReason    string
}

const (
	// Shared finish-reason vocabulary used across providers.
	FinishStop      = "stop"
	FinishLength    = "length"
	FinishToolCalls = "tool_calls"
)

// reasoningHandler is the optional hook for live reasoning_content deltas.
type reasoningHandler interface {
	OnReasoning(token string)
}

// toolCallDeltaHandler fires on the first tool-call delta of a turn,
// so the streaming handler can distinguish final answer from tool-call preamble.
type toolCallDeltaHandler interface {
	OnToolCallDelta()
}

// StreamHandler receives streamed answer tokens and completed tool calls.
type StreamHandler interface {
	OnToken(token string)     // text delta
	OnToolCall(call ToolCall) // a complete tool call assembled from fragments
}

// ChatWithTools runs one streaming turn with the client's default max_tokens.
func (lc *LLMClient) ChatWithTools(ctx context.Context, messages []Message, tools []ToolDef, h StreamHandler) (*ChatStreamResult, error) {
	return lc.ChatWithToolsMax(ctx, messages, tools, h, 0)
}

// ChatWithToolsMax runs one streaming turn with an optional max token override.
func (lc *LLMClient) ChatWithToolsMax(ctx context.Context, messages []Message, tools []ToolDef, h StreamHandler, maxTokens int) (result *ChatStreamResult, callErr error) {
	if maxTokens <= 0 {
		maxTokens = lc.maxTokens
	}
	lc.logPrompt(ctx, messages, len(tools), maxTokens)
	finishLifecycle := beginCallLifecycle(ctx)
	defer func() { finishLifecycle(callErr) }()
	started := time.Now()
	defer func() {
		var usage Usage
		if result != nil {
			usage = result.Usage
		}
		recordCallUsage(ctx, lc.provider, lc.model, maxTokens, started, usage, callErr)
	}()
	if lc.provider == "anthropic" {
		result, callErr = lc.anthropic().ChatWithToolsMax(ctx, messages, tools, h, maxTokens)
		return result, callErr
	}
	req := chatRequest{
		Model: lc.model, Messages: messages, MaxTokens: maxTokens,
		Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}, Tools: tools,
	}
	if len(tools) > 0 {
		req.ToolChoice = "auto"
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := postStream(ctx, func() (*resty.Response, error) {
		return httpclient.Request(ctx, lc.rc).
			SetHeader("Accept", "text/event-stream").
			SetBody(body).
			SetDoNotParseResponse(true).
			Post(lc.baseURL + "/chat/completions")
	}, func(duration time.Duration, attemptErr error) {
		recordCallUsageWithDuration(ctx, lc.provider, lc.model, maxTokens, duration, Usage{}, attemptErr)
	})
	if err != nil {
		return nil, err
	}
	raw := resp.RawBody()
	if raw == nil {
		return nil, fmt.Errorf("empty response body")
	}
	defer raw.Close()

	acc := newToolCallAccumulator()
	result = &ChatStreamResult{}
	var rh reasoningHandler
	if h != nil {
		rh, _ = h.(reasoningHandler)
	}

	scanner := bufio.NewScanner(raw)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			result.Usage = chunk.Usage.shared()
			result.ReasoningTokens = result.Usage.ReasoningTokens
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != "" {
				result.Reasoning += ch.Delta.ReasoningContent
				if rh != nil {
					rh.OnReasoning(ch.Delta.ReasoningContent)
				}
			}
			if ch.Delta.Content != "" {
				result.Content += ch.Delta.Content
				if h != nil {
					h.OnToken(ch.Delta.Content)
				}
			}
			if len(ch.Delta.ToolCalls) > 0 {
				// First tool-call delta — signal handler that this turn is calling tools.
				if tcs, ok := h.(toolCallDeltaHandler); ok {
					tcs.OnToolCallDelta()
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc.merge(tc)
			}
			if ch.FinishReason != "" {
				result.FinishReason = ch.FinishReason
				for _, call := range acc.flush() {
					result.ToolCalls = append(result.ToolCalls, call)
					if h != nil {
						h.OnToolCall(call)
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	for _, call := range acc.flush() {
		result.ToolCalls = append(result.ToolCalls, call)
		if h != nil {
			h.OnToolCall(call)
		}
	}
	return result, nil
}

func (lc *LLMClient) logPrompt(ctx context.Context, messages []Message, toolCount, maxTokens int) {
	log.InfofCtx(ctx, "[llm] request provider=%s model=%s max_tokens=%d messages=%d tools=%d dynamic_prompt:\n%s",
		lc.provider, lc.model, maxTokens, len(messages), toolCount, joinDynamicPromptMessages(messages))
}

func joinDynamicPromptMessages(messages []Message) string {
	start := 0
	// Request assembly reserves the first system message for fixed instructions.
	if len(messages) > 0 && messages[0].Role == "system" {
		start = 1
	}
	if start == len(messages) {
		return "(no dynamic messages)"
	}
	var joined strings.Builder
	for i := start; i < len(messages); i++ {
		message := messages[i]
		if i > start {
			joined.WriteByte('\n')
		}
		fmt.Fprintf(&joined, "----- message %d role=%s", i+1, message.Role)
		if message.Name != "" {
			fmt.Fprintf(&joined, " name=%s", message.Name)
		}
		if message.ToolCallID != "" {
			fmt.Fprintf(&joined, " tool_call_id=%s", message.ToolCallID)
		}
		joined.WriteString(" -----\n")
		joined.WriteString(message.Content)
		if len(message.ToolCalls) > 0 {
			encoded, _ := json.Marshal(message.ToolCalls)
			if message.Content != "" {
				joined.WriteByte('\n')
			}
			joined.WriteString("tool_calls=")
			joined.Write(encoded)
		}
		joined.WriteString("\n----- end message -----")
	}
	return joined.String()
}

// streamDelta is one incremental delta in a streamed choice.
type streamDelta struct {
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content"`
	ToolCalls        []streamToolCall `json:"tool_calls,omitempty"`
}
type streamChoice struct {
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}
type streamChunk struct {
	Choices []streamChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

// streamToolCall is one streamed fragment of a tool call.
type streamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolCallAccumulator merges streamed tool-call fragments by index.
type toolCallAccumulator struct {
	pending map[int]*ToolCall
	order   []int
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{pending: map[int]*ToolCall{}}
}

func (a *toolCallAccumulator) merge(f streamToolCall) {
	tc, ok := a.pending[f.Index]
	if !ok {
		tc = &ToolCall{Type: "function"}
		a.pending[f.Index] = tc
		a.order = append(a.order, f.Index)
	}
	if f.ID != "" {
		tc.ID = f.ID
	}
	if f.Type != "" {
		tc.Type = f.Type
	}
	if f.Function.Name != "" {
		tc.Function.Name = f.Function.Name
	}
	if f.Function.Arguments != "" {
		tc.Function.Arguments += f.Function.Arguments
	}
}

func (a *toolCallAccumulator) flush() []ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		if tc := a.pending[idx]; tc != nil && (tc.ID != "" || tc.Function.Name != "") {
			out = append(out, *tc)
		}
	}
	a.pending = map[int]*ToolCall{}
	a.order = nil
	return out
}
