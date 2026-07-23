package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/platform/httpclient"
	"github.com/go-resty/resty/v2"
)

// anthropicProvider adapts the shared chat API to Anthropic's wire format.
type anthropicProvider struct {
	baseURL   string
	apiKey    string
	model     string
	maxTokens int
	rc        *resty.Client
}

const anthropicVersion = "2023-06-01"

type anthropicRequest struct {
	Model      string               `json:"model"`
	System     string               `json:"system,omitempty"`
	Messages   []anthropicMessage   `json:"messages"`
	MaxTokens  int                  `json:"max_tokens"`
	Stream     bool                 `json:"stream"`
	Tools      []anthropicTool      `json:"tools,omitempty"`
	ToolChoice *anthropicToolChoice `json:"tool_choice,omitempty"`
}

type anthropicMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// translateMessages converts shared messages to Anthropic message blocks.
func translateMessages(messages []Message) (system string, out []anthropicMessage) {
	var systemParts []string
	for _, item := range messages {
		switch item.Role {
		case "system":
			if item.Content != "" {
				systemParts = append(systemParts, item.Content)
			}
			continue
		case "tool":
			// Anthropic expects consecutive tool results to share one user turn.
			if len(out) > 0 && out[len(out)-1].Role == "user" && isToolResultTurn(out[len(out)-1]) {
				out[len(out)-1].Content = append(out[len(out)-1].Content, toolResultBlock(item))
			} else {
				out = append(out, anthropicMessage{Role: "user", Content: []anthropicContentBlock{toolResultBlock(item)}})
			}
		case "assistant":
			out = append(out, anthropicMessage{Role: "assistant", Content: assistantBlocks(item)})
		default: // "user" or anything else
			out = append(out, anthropicMessage{Role: "user", Content: []anthropicContentBlock{{Type: "text", Text: item.Content}}})
		}
	}
	return strings.Join(systemParts, "\n\n"), out
}

func isToolResultTurn(item1 anthropicMessage) bool {
	if len(item1.Content) == 0 {
		return false
	}
	for _, buf := range item1.Content {
		if buf.Type != "tool_result" {
			return false
		}
	}
	return true
}

func toolResultBlock(item2 Message) anthropicContentBlock {
	return anthropicContentBlock{Type: "tool_result", ToolUseID: item2.ToolCallID, Text: item2.Content}
}

func assistantBlocks(item3 Message) []anthropicContentBlock {
	var blocks []anthropicContentBlock
	if item3.Content != "" {
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: item3.Content})
	}
	for _, tc := range item3.ToolCalls {
		blocks = append(blocks, anthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: parseArgsInput(tc.Function.Arguments),
		})
	}
	return blocks
}

// parseArgsInput decodes tool-call arguments into a generic map.
func parseArgsInput(arguments string) map[string]any {
	out := map[string]any{}
	text := strings.TrimSpace(arguments)
	if text == "" {
		return out
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return map[string]any{}
	}
	return out
}

func translateToolDefs(tools []ToolDef) []anthropicTool {
	out := make([]anthropicTool, 0, len(tools))
	for _, term := range tools {
		out = append(out, anthropicTool{
			Name:        term.Function.Name,
			Description: term.Function.Description,
			InputSchema: term.Function.Parameters,
		})
	}
	return out
}

// normalizeStopReason maps Anthropic stop reasons onto shared finish reasons.
func normalizeStopReason(reason string) string {
	switch reason {
	case "max_tokens":
		return FinishLength
	case "tool_use":
		return FinishToolCalls
	case "end_turn", "stop_sequence", "pause_turn", "refusal", "":
		return FinishStop
	default:
		return FinishStop
	}
}

func (anthropic anthropicProvider) ChatMax(ctx context.Context, system, user string, maxTokens int) (string, error) {
	content, _, err := anthropic.chatMessages(ctx, []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}, maxTokens)
	return content, err
}

func (anthropic anthropicProvider) chatMessages(ctx context.Context, messages []Message, maxTokens int) (string, Usage, error) {
	if maxTokens <= 0 {
		maxTokens = anthropic.maxTokens
	}
	system, wireMessages := translateMessages(messages)
	body, err := json.Marshal(anthropicRequest{
		Model:     anthropic.model,
		System:    system,
		Messages:  wireMessages,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", Usage{}, fmt.Errorf("marshal request: %w", err)
	}
	resp, err := httpclient.Request(ctx, anthropic.rc).
		SetHeader("x-api-key", anthropic.apiKey).
		SetHeader("anthropic-version", anthropicVersion).
		SetHeader("Content-Type", "application/json").
		SetBody(body).
		Post(anthropic.baseURL + "/v1/messages")
	if err != nil {
		return "", Usage{}, &CallError{Kind: ErrKindNetwork, Err: fmt.Errorf("http request: %w", err)}
	}
	if resp.StatusCode() != http.StatusOK {
		return "", Usage{}, &CallError{
			Kind:       ErrKindStatus,
			Status:     resp.StatusCode(),
			Body:       boundedErrorBody(resp.Body()),
			RetryAfter: parseRetryAfter(resp.Header(), maxBackoff),
		}
	}

	var result anthropicMessageResponse
	if err := json.Unmarshal(resp.Body(), &result); err != nil {
		return "", Usage{}, &CallError{Kind: ErrKindEnvelope, Err: err}
	}
	content := concatTextBlocks(result.Content)
	if strings.TrimSpace(content) == "" {
		return "", result.Usage.shared(), &CallError{Kind: ErrKindEmpty}
	}
	return content, result.Usage.shared(), nil
}

type anthropicMessageResponse struct {
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

func concatTextBlocks(blocks []anthropicContentBlock) string {
	var sb strings.Builder
	for _, buf2 := range blocks {
		if buf2.Type == "text" {
			sb.WriteString(buf2.Text)
		}
	}
	return sb.String()
}

func (anthropic anthropicProvider) ChatWithToolsMax(ctx context.Context, messages []Message, tools []ToolDef, item4 StreamHandler, maxTokens int) (*ChatStreamResult, error) {
	if maxTokens <= 0 {
		maxTokens = anthropic.maxTokens
	}
	system, wireMsgs := translateMessages(messages)
	req := anthropicRequest{
		Model:     anthropic.model,
		System:    system,
		Messages:  wireMsgs,
		MaxTokens: maxTokens,
		Stream:    true,
		Tools:     translateToolDefs(tools),
	}
	if len(tools) > 0 {
		req.ToolChoice = &anthropicToolChoice{Type: "auto"}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := postStream(ctx, func() (*resty.Response, error) {
		return httpclient.Request(ctx, anthropic.rc).
			SetHeader("x-api-key", anthropic.apiKey).
			SetHeader("anthropic-version", anthropicVersion).
			SetHeader("Content-Type", "application/json").
			SetHeader("Accept", "text/event-stream").
			SetBody(body).
			SetDoNotParseResponse(true).
			Post(anthropic.baseURL + "/v1/messages")
	}, func(duration time.Duration, attemptErr error) {
		recordCallUsageWithDuration(ctx, "anthropic", anthropic.model, maxTokens, duration, Usage{}, attemptErr)
	})
	if err != nil {
		return nil, err
	}
	raw := resp.RawBody()
	if raw == nil {
		return nil, fmt.Errorf("empty response body")
	}
	defer raw.Close()

	result := &ChatStreamResult{}
	var rh reasoningHandler
	if item4 != nil {
		rh, _ = item4.(reasoningHandler)
	}

	// Anthropic streams event/data pairs instead of OpenAI-style flat data lines.
	acc := newInputJSONAccumulator()
	scanner := bufio.NewScanner(raw)
	// Tool arguments can exceed the default scanner token limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var eventType string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			if err := anthropic.handleSSEEvent(eventType, data, result, item4, rh, acc); err != nil {
				return result, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	for _, call := range acc.flush() {
		result.ToolCalls = append(result.ToolCalls, call)
		if item4 != nil {
			item4.OnToolCall(call)
		}
	}
	return result, nil
}

// handleSSEEvent applies one Anthropic streaming event to the result state.
func (anthropic anthropicProvider) handleSSEEvent(eventType, data string, result *ChatStreamResult, item5 StreamHandler, rh reasoningHandler, acc *inputJSONAccumulator) error {
	switch eventType {
	case "message_start":
		var ev messageStartEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil
		}
		result.Usage = ev.Message.Usage.shared()
	case "content_block_start":
		var ev contentBlockStartEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil // skip malformed
		}
		if ev.ContentBlock.Type == "tool_use" {
			if tcs, ok := item5.(toolCallDeltaHandler); ok {
				tcs.OnToolCallDelta()
			}
		}
		acc.startBlock(ev.Index, ev.ContentBlock)
	case "content_block_delta":
		var ev contentBlockDeltaEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				result.Content += ev.Delta.Text
				if item5 != nil {
					item5.OnToken(ev.Delta.Text)
				}
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				result.Reasoning += ev.Delta.Thinking
				if rh != nil {
					rh.OnReasoning(ev.Delta.Thinking)
				}
			}
		case "input_json_delta":
			acc.appendJSON(ev.Index, ev.Delta.PartialJSON)
		}
	case "content_block_stop":
		var ev contentBlockStopEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil
		}
		if call, ok := acc.stopBlock(ev.Index); ok {
			result.ToolCalls = append(result.ToolCalls, call)
			if item5 != nil {
				item5.OnToolCall(call)
			}
		}
	case "message_delta":
		var ev messageDeltaEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return nil
		}
		if ev.Delta.StopReason != "" {
			result.FinishReason = normalizeStopReason(ev.Delta.StopReason)
		}
		result.Usage.OutputTokens = ev.Usage.OutputTokens
		result.Usage.TotalTokens = result.Usage.InputTokens + result.Usage.OutputTokens
		result.ReasoningTokens = result.Usage.ReasoningTokens
	case "error":
		var ev anthropicErrorEvent
		if err := json.Unmarshal([]byte(data), &ev); err == nil && ev.Error.Message != "" {
			return fmt.Errorf("anthropic stream error: %s", ev.Error.Message)
		}
	}
	return nil
}

type messageStartEvent struct {
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
}

type contentBlockStartEvent struct {
	Index        int                    `json:"index"`
	ContentBlock contentBlockStartBlock `json:"content_block"`
}

type contentBlockStartBlock struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type contentBlockDeltaEvent struct {
	Index int               `json:"index"`
	Delta contentBlockDelta `json:"delta"`
}

type contentBlockDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

type contentBlockStopEvent struct {
	Index int `json:"index"`
}

type messageDeltaEvent struct {
	Delta struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage anthropicUsage `json:"usage"`
}

type anthropicErrorEvent struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// inputJSONAccumulator rebuilds streamed tool-call JSON fragments.
type inputJSONAccumulator struct {
	blocks map[int]*inputBlockState
}

type inputBlockState struct {
	id      string
	name    string
	partial strings.Builder
}

func newInputJSONAccumulator() *inputJSONAccumulator {
	return &inputJSONAccumulator{blocks: map[int]*inputBlockState{}}
}

func (arg *inputJSONAccumulator) startBlock(index int, buf4 contentBlockStartBlock) {
	if buf4.Type != "tool_use" {
		return
	}
	arg.blocks[index] = &inputBlockState{id: buf4.ID, name: buf4.Name}
}

func (arg1 *inputJSONAccumulator) appendJSON(index int, fragment string) {
	st, ok := arg1.blocks[index]
	if !ok {
		return
	}
	st.partial.WriteString(fragment)
}

func (arg2 *inputJSONAccumulator) stopBlock(index int) (ToolCall, bool) {
	st, ok := arg2.blocks[index]
	if !ok {
		return ToolCall{}, false
	}
	delete(arg2.blocks, index)
	raw := st.partial.String()
	if raw == "" {
		raw = "{}"
	}
	// Keep the raw JSON string so downstream tool parsing matches the OpenAI path.
	return ToolCall{
		ID:       st.id,
		Type:     "function",
		Function: ToolFunction{Name: st.name, Arguments: raw},
	}, true
}

func (arg3 *inputJSONAccumulator) flush() []ToolCall {
	if len(arg3.blocks) == 0 {
		return nil
	}
	indices := make([]int, 0, len(arg3.blocks))
	for idx := range arg3.blocks {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	out := make([]ToolCall, 0, len(indices))
	for _, idx3 := range indices {
		if call, ok := arg3.stopBlock(idx3); ok {
			out = append(out, call)
		}
	}
	return out
}
