package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

func drainRequestBody(r *http.Request) {
	if r == nil || r.Body == nil {
		return
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
}

// fakeStreamServer returns an SSE test server for /chat/completions.
// Each events entry becomes one data line in the stream.
// finish optionally sets the last chunk's finish_reason.
func fakeStreamServer(t *testing.T, events []streamEvent) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i, e := range events {
			choice := streamChoiceJS{
				Delta:        streamDeltaJS{Content: e.content, ReasoningContent: e.reasoning},
				FinishReason: e.finish,
			}
			// Only set index for tool calls (not used in these tests).
			chunk := streamChunkJS{Choices: []streamChoiceJS{choice}}
			data, _ := json.Marshal(chunk)
			if i < len(events)-1 || e.finish == "" {
				w.Write([]byte("data: " + string(data) + "\n\n"))
			} else {
				w.Write([]byte("data: " + string(data) + "\n\n"))
				w.Write([]byte("data: [DONE]\n\n"))
			}
			w.(http.Flusher).Flush()
		}
	}))
}

func partialAnswerServer(t *testing.T, content string, calls *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: content}}}}
		data, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
}

type streamChunkJS struct {
	Choices []streamChoiceJS `json:"choices"`
}
type streamChoiceJS struct {
	Delta        streamDeltaJS `json:"delta"`
	FinishReason string        `json:"finish_reason"`
}
type streamDeltaJS struct {
	Content          string `json:"content"`
	ReasoningContent string `json:"reasoning_content"`
}

// streamEvent is one line in a fake SSE stream.
type streamEvent struct {
	content   string
	reasoning string // reasoning_content delta (reasoning models' thinking channel)
	finish    string
}

func newTestAgent(t *testing.T, serverURL string) *Agent {
	t.Helper()
	client := llm.NewLLMClientWithHTTP(serverURL, "k", "test", 100, &http.Client{})
	return NewAgent(client, nil, AgentConfig{
		MaxSteps:          5,
		AnswerMaxTokens:   100,
		MaxContinueRounds: 2,
		Timeout:           10 * time.Second,
		AnswerReserve:     5 * time.Second,
	}, nil, nil)
}

func TestMaxStepsForQuestion(t *testing.T) {
	agent := &Agent{cfg: AgentConfig{MaxSteps: 5}}
	cases := []struct {
		question string
		want     int
	}{
		{"what does this service do", 2},
		{"review this architecture", 3},
		{"trace the caller call chain", 5},
		{"why did this request timeout", 5},
	}
	for _, tc := range cases {
		if got := agent.MaxStepsFor(tc.question); got != tc.want {
			t.Errorf("MaxStepsFor(%q) = %d, want %d", tc.question, got, tc.want)
		}
	}
}

func TestMaxStepsForWebPlanAllowsFetchTurn(t *testing.T) {
	agent := &Agent{cfg: AgentConfig{MaxSteps: 5}}
	plan := domain.EvidencePlan{Sources: domain.Web}
	if got := agent.MaxStepsForPlan("school information", plan); got != 3 {
		t.Fatalf("MaxStepsForPlan() = %d, want 3", got)
	}
}

func TestExtendWebStepLimitOnlyAfterUnusableEvidenceAtBoundary(t *testing.T) {
	if got := extendWebStepLimit(3, 3, 5, true, false); got != 4 {
		t.Fatalf("failed boundary attempt extended to %d, want 4", got)
	}
	for _, tc := range []struct {
		name                 string
		step, current, max   int
		attempted, succeeded bool
	}{
		{name: "before boundary", step: 2, current: 3, max: 5, attempted: true},
		{name: "usable evidence", step: 3, current: 3, max: 5, attempted: true, succeeded: true},
		{name: "no web attempt", step: 3, current: 3, max: 5},
		{name: "configured limit", step: 5, current: 5, max: 5, attempted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extendWebStepLimit(tc.step, tc.current, tc.max, tc.attempted, tc.succeeded); got != tc.current {
				t.Fatalf("extendWebStepLimit() = %d, want %d", got, tc.current)
			}
		})
	}
}

func TestExtendEvidenceStepLimitOnceAtBoundary(t *testing.T) {
	if got := extendEvidenceStepLimit(2, 2, 5, true, false); got != 3 {
		t.Fatalf("boundary evidence extended to %d, want 3", got)
	}
	for _, tc := range []struct {
		name                      string
		step, current, configured int
		produced, alreadyExtended bool
	}{
		{name: "before boundary", step: 1, current: 2, configured: 5, produced: true},
		{name: "no evidence", step: 2, current: 2, configured: 5},
		{name: "already extended", step: 2, current: 2, configured: 5, produced: true, alreadyExtended: true},
		{name: "configured limit", step: 5, current: 5, configured: 5, produced: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extendEvidenceStepLimit(tc.step, tc.current, tc.configured, tc.produced, tc.alreadyExtended); got != tc.current {
				t.Fatalf("extendEvidenceStepLimit() = %d, want %d", got, tc.current)
			}
		})
	}
}

func TestRunExtendsOneToolCapableTurnAfterBoundaryEvidence(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		call := atomic.AddInt32(&calls, 1)
		if call <= 2 {
			data := fmt.Sprintf(`{"choices":[{"delta":{"content":"继续核实。","tool_calls":[{"index":0,"id":"call-%d","type":"function","function":{"name":"evidence","arguments":"{\"step\":%d}"}}]},"finish_reason":"tool_calls"}]}`, call, call)
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"证据已补全。\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	registry := testRegistry(t, Tool{
		ID: "evidence", Description: "test evidence", Kind: ToolKindRead,
		InputSchema: objectSchema(map[string]any{"step": propInt("step")}, []string{"step"}),
		Handler:     stringHandler(func(context.Context, tool.Arguments) (string, error) { return `{"found":true}`, nil }),
	})
	client := llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{})
	agent := NewAgent(client, NewToolExecutor(registry), AgentConfig{
		MaxSteps: 3, AnswerMaxTokens: 100, Timeout: 5 * time.Second, AnswerReserve: time.Second,
	}, nil, nil)
	result, err := agent.RunWithPlan(t.Context(), "run_evidence_extension", "这个服务做什么？", nil, nil,
		domain.EvidencePlan{Sources: domain.Internal}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err != nil || result.Steps != 3 || result.Answer != "证据已补全。" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunAllowsDirectAnswerWithoutPreferredTool(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		atomic.AddInt32(&calls, 1)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"已有上下文足够，直接回答。\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	var runtimeAttempts int32
	registry := testRegistry(t, Tool{
		ID: "runtime_evidence", Description: "runtime evidence", Kind: ToolKindRead,
		InputSchema: objectSchema(nil, nil),
		Handler: stringHandler(func(context.Context, tool.Arguments) (string, error) {
			atomic.AddInt32(&runtimeAttempts, 1)
			return `{"count":42}`, nil
		}),
	})
	client := llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{})
	agent := NewAgent(client, NewToolExecutor(registry), AgentConfig{
		MaxSteps: 2, AnswerMaxTokens: 100, Timeout: 5 * time.Second, AnswerReserve: time.Second,
	}, nil, nil)
	plan := domain.EvidencePlan{Sources: domain.Internal}
	policy := ToolPolicyForPlan(plan, false)
	result, err := agent.runWithSnapshot(
		t.Context(), "run_preferred_tool", "继续", ConversationContext{Instructions: []llm.Message{{
			Role: "system", Content: preferredToolsInstruction([]string{"runtime_evidence"}),
		}}}, nil, plan, policy, agent.executor.Snapshot(policy),
	)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&runtimeAttempts) != 0 {
		t.Fatalf("runtime attempts = %d, want 0", runtimeAttempts)
	}
	if atomic.LoadInt32(&calls) != 1 || result.Err != nil || result.Steps != 1 || result.Answer != "已有上下文足够，直接回答。" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunForcesConclusionAfterToolFailure(t *testing.T) {
	var calls int32
	var sawToolError atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-runtime\",\"type\":\"function\",\"function\":{\"name\":\"runtime_evidence\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		sawToolError.Store(strings.Contains(string(body), "backend unavailable"))
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"运行时查询失败，当前只能根据已有资料说明处理方案。\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	var attempts int32
	registry := testRegistry(t, Tool{
		ID: "runtime_evidence", Description: "runtime evidence", Kind: ToolKindRead,
		InputSchema: objectSchema(nil, nil),
		Handler: stringHandler(func(context.Context, tool.Arguments) (string, error) {
			atomic.AddInt32(&attempts, 1)
			return "", errors.New("backend unavailable")
		}),
	})
	client := llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, server.Client())
	observer := &captureObserver{}
	agent := NewAgent(client, NewToolExecutor(registry), AgentConfig{
		MaxSteps: 1, AnswerMaxTokens: 100, Timeout: 5 * time.Second, AnswerReserve: time.Second,
	}, observer, nil)
	plan := domain.EvidencePlan{Sources: domain.Internal}
	policy := ToolPolicyForPlan(plan, false)
	result, err := agent.runWithSnapshot(
		t.Context(), "run_tool_failure", "当前有多少用户？", ConversationContext{}, nil,
		plan, policy, agent.executor.Snapshot(policy),
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err != nil || result.Answer != "运行时查询失败，当前只能根据已有资料说明处理方案。" {
		t.Fatalf("result = %#v", result)
	}
	if atomic.LoadInt32(&attempts) != 1 || !sawToolError.Load() {
		t.Fatalf("attempts=%d sawToolError=%v", attempts, sawToolError.Load())
	}
	if got := strings.Join(observer.tokens, ""); got != result.Answer {
		t.Fatalf("streamed answer = %q, want %q", got, result.Answer)
	}
	var failedResult *StepRecord
	for index := range observer.steps {
		step := &observer.steps[index]
		if step.Kind == StepKindToolResult {
			failedResult = step
			break
		}
	}
	if failedResult == nil || !failedResult.Failed || !strings.Contains(failedResult.Content, "backend unavailable") {
		t.Fatalf("failed tool result step = %#v", failedResult)
	}
}

func TestContinueIfNeeded_NoTruncation(t *testing.T) {
	srv := fakeStreamServer(t, []streamEvent{
		{content: "完整回答", finish: "stop"},
	})
	defer srv.Close()

	agent := newTestAgent(t, srv.URL)
	res, err := agent.llm.ChatWithToolsMax(t.Context(), nil, nil, nil, 100)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	res, cerr := agent.continueIfNeeded(t.Context(), nil, res, 100, nil)
	if cerr != nil {
		t.Fatalf("continueIfNeeded: unexpected error: %v", cerr)
	}
	if res.FinishReason != "stop" {
		t.Fatalf("expected stop, got %q", res.FinishReason)
	}
	if res.Content != "完整回答" {
		t.Fatalf("content should be unchanged, got %q", res.Content)
	}
}

func TestContinueIfNeeded_LengthTruncation(t *testing.T) {
	// First chunk: truncated. Continuation chunk: the rest.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		callCount++
		var events []streamEvent
		if callCount <= 1 {
			// First call: partial answer, length truncation.
			events = []streamEvent{
				{content: "这是一段很长的架构总结，包含四层架构和多个", finish: "length"},
			}
		} else {
			// Continuation call: the remainder.
			events = []streamEvent{
				{content: "表格数据，现在补全最后的部分。", finish: "stop"},
			}
		}
		for i, e := range events {
			chunk := streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: e.content}, FinishReason: e.finish}}}
			data, _ := json.Marshal(chunk)
			w.Write([]byte("data: " + string(data) + "\n\n"))
			if i == len(events)-1 {
				w.Write([]byte("data: [DONE]\n\n"))
			}
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	agent := newTestAgent(t, srv.URL)
	res, err := agent.llm.ChatWithToolsMax(t.Context(), nil, nil, nil, 100)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if res.FinishReason != "length" {
		t.Fatalf("first call should be truncated, got %q", res.FinishReason)
	}
	res, cerr := agent.continueIfNeeded(t.Context(), nil, res, 100, nil)
	if cerr != nil {
		t.Fatalf("continueIfNeeded: unexpected error: %v", cerr)
	}
	if res.FinishReason != "stop" {
		t.Fatalf("after continuation expected stop, got %q", res.FinishReason)
	}
	if !strings.Contains(res.Content, "架构总结") || !strings.Contains(res.Content, "补全最后") {
		t.Fatalf("content not concatenated, got %q", res.Content)
	}
	if callCount != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", callCount)
	}
}

func TestContinueIfNeeded_MaxRoundsCap(t *testing.T) {
	// Simulate a model that always truncates (never reaches stop).
	// After MaxContinueRounds, continueIfNeeded should give up.
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		callCount++
		chunk := streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: "x"}, FinishReason: "length"}}}
		data, _ := json.Marshal(chunk)
		w.Write([]byte("data: " + string(data) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	agent := newTestAgent(t, srv.URL)
	agent.cfg.MaxContinueRounds = 2
	res, err := agent.llm.ChatWithToolsMax(t.Context(), nil, nil, nil, 100)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	res, cerr := agent.continueIfNeeded(t.Context(), nil, res, 100, nil)
	// Hit the round cap with non-empty content → "still truncated" path, no error.
	if cerr != nil {
		t.Fatalf("continueIfNeeded: unexpected error at round cap: %v", cerr)
	}
	if res.FinishReason != "length" {
		t.Fatalf("should still be length after round cap, got %q", res.FinishReason)
	}
	// 1 initial + 2 continuation = 3 total calls
	if callCount != 3 {
		t.Fatalf("expected 3 calls (1 + MaxContinueRounds=2), got %d", callCount)
	}
	// Content should have 3 "x"s (one per call).
	if want := "xxx"; res.Content != want {
		t.Fatalf("expected %q, got %q", want, res.Content)
	}
}

// TestReasoningContent_ParsedAndStreamed verifies reasoning_content handling.
// Token accounting remains zero when the Provider omits usage.
func TestReasoningContent_ParsedStreamedCounted(t *testing.T) {
	srv := fakeStreamServer(t, []streamEvent{
		{reasoning: "先思考", finish: ""},
		{reasoning: "再思考", finish: ""},
		{content: "答案是", finish: ""},
		{content: "这样", finish: "stop"},
	})
	defer srv.Close()

	agent := newTestAgent(t, srv.URL)
	h := &captureHandler{}
	res, err := agent.llm.ChatWithToolsMax(t.Context(), nil, nil, h, 100)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if res.Reasoning != "先思考再思考" {
		t.Fatalf("reasoning = %q, want 先思考再思考", res.Reasoning)
	}
	if res.ReasoningTokens != 0 {
		t.Fatalf("reasoning tokens = %d, want provider-reported zero", res.ReasoningTokens)
	}
	if res.Content != "答案是这样" {
		t.Fatalf("content = %q, want 答案是这样", res.Content)
	}
	// Reasoning deltas must stream live through OnReasoning, not OnToken.
	if got, want := strings.Join(h.reasoning, ""), "先思考再思考"; got != want {
		t.Fatalf("streamed reasoning = %q, want %q", got, want)
	}
	if len(h.tokens) != 2 || strings.Join(h.tokens, "") != "答案是这样" {
		t.Fatalf("tokens = %v, want only the 2 visible deltas", h.tokens)
	}
}

// TestContinueIfNeeded_ReasoningStageTruncation covers the empty-output bug.
// If max_tokens is spent entirely on reasoning_content, continuation has no seed.
// continueIfNeeded must return ErrReasoningTruncated, not an empty nil-error answer.
func TestContinueIfNeeded_ReasoningStageTruncation(t *testing.T) {
	srv := fakeStreamServer(t, []streamEvent{
		{reasoning: "思考了很多但没产出可见内容", finish: "length"},
	})
	defer srv.Close()

	agent := newTestAgent(t, srv.URL)
	res, err := agent.llm.ChatWithToolsMax(t.Context(), nil, nil, nil, 100)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if res.FinishReason != "length" || res.Content != "" {
		t.Fatalf("precondition: want length+empty, got %q/%q", res.FinishReason, res.Content)
	}
	res, cerr := agent.continueIfNeeded(t.Context(), nil, res, 100, nil)
	if !errors.Is(cerr, ErrReasoningTruncated) {
		t.Fatalf("expected ErrReasoningTruncated, got %v", cerr)
	}
	// The (empty) partial answer is preserved; no continuation call was made.
	if res.Content != "" {
		t.Fatalf("content should remain empty, got %q", res.Content)
	}
}

func TestContinueIfNeeded_EmptyStopIsNotReasoningTruncation(t *testing.T) {
	srv := fakeStreamServer(t, []streamEvent{{finish: "stop"}})
	defer srv.Close()

	agent := newTestAgent(t, srv.URL)
	res, err := agent.llm.ChatWithToolsMax(t.Context(), nil, nil, nil, 100)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, cerr := agent.continueIfNeeded(t.Context(), nil, res, 100, nil)
	if !errors.Is(cerr, ErrEmptyModelResponse) {
		t.Fatalf("expected ErrEmptyModelResponse, got %v", cerr)
	}
	if errors.Is(cerr, ErrReasoningTruncated) {
		t.Fatalf("empty stop response was misclassified as reasoning truncation")
	}
}

func TestRunRecoversFromEmptyStopWithForcedConclusion(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		w.Header().Set("Content-Type", "text/event-stream")
		content := ""
		if atomic.AddInt32(&calls, 1) == 2 {
			content = "根据已有证据给出结论。"
		}
		chunk := streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: content}, FinishReason: "stop"}}}
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	defer srv.Close()

	client := llm.NewLLMClientWithHTTP(srv.URL, "k", "test", 100, &http.Client{})
	agent := NewAgent(client, nil, AgentConfig{
		MaxSteps: 2, AnswerMaxTokens: 100, Timeout: 5 * time.Second, AnswerReserve: time.Second,
	}, nil, nil)
	result, err := agent.RunWithPlan(t.Context(), "run_empty_stop", "请分析当前问题", nil, nil, domain.EvidencePlan{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err != nil || result.Answer != "根据已有证据给出结论。" {
		t.Fatalf("result = %#v", result)
	}
	if !result.ForcedConclusion || !result.Evidence.ForcedConclusion || result.Evidence.Status != EvidenceNotRequired {
		t.Fatalf("forced conclusion evidence = %#v", result.Evidence)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("LLM calls = %d, want 2", calls)
	}
}

func TestGenerateWithContinue_NoTruncation(t *testing.T) {
	srv := fakeStreamServer(t, []streamEvent{
		{content: "短回答", finish: "stop"},
	})
	defer srv.Close()

	agent := newTestAgent(t, srv.URL)
	res, err := agent.generateWithContinue(t.Context(), nil, 100, nil)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.FinishReason != "stop" {
		t.Fatalf("expected stop, got %q", res.FinishReason)
	}
	if res.Content != "短回答" {
		t.Fatalf("got %q", res.Content)
	}
}

// captureHandler records streamed answer deltas for assertions.
// It also captures reasoning-channel deltas separately.
// Tests use it to verify live streaming rather than post-hoc blobs.
type captureHandler struct {
	tokens    []string
	reasoning []string
}

func (c *captureHandler) OnToken(token string)    { c.tokens = append(c.tokens, token) }
func (c *captureHandler) OnToolCall(llm.ToolCall) {}

// OnReasoning implements the optional reasoningHandler interface so
// captureHandler picks up reasoning_content deltas in tests.
func (c *captureHandler) OnReasoning(token string) { c.reasoning = append(c.reasoning, token) }

func TestGenerateWithContinue_StreamsTokens(t *testing.T) {
	srv := fakeStreamServer(t, []streamEvent{
		{content: "最终", finish: ""},
		{content: "结论", finish: "stop"},
	})
	defer srv.Close()

	agent := newTestAgent(t, srv.URL)
	h := &captureHandler{}
	if _, err := agent.generateWithContinue(t.Context(), nil, 100, h); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if got, want := strings.Join(h.tokens, ""), "最终结论"; got != want {
		t.Fatalf("streamed tokens = %q, want %q", got, want)
	}
}

// captureObserver records OnStep/OnToken/OnReasoning calls for assertions
// about what the hub (and thus SSE clients) would see during a run.
type captureObserver struct {
	steps     []StepRecord
	tokens    []string
	reasoning []string
}

func (c *captureObserver) OnStep(_ context.Context, _ string, s StepRecord) error {
	c.steps = append(c.steps, s)
	return nil
}
func (c *captureObserver) OnToken(_ context.Context, _ string, tok string) {
	c.tokens = append(c.tokens, tok)
}
func (c *captureObserver) OnReasoning(_ context.Context, _ string, tok string) {
	c.reasoning = append(c.reasoning, tok)
}

func TestRunDeliversFreshToolOutputWithoutLoss(t *testing.T) {
	fullContent := `{"records":[` + strings.Repeat(`{"name":"other","payload":"`+strings.Repeat("x", 200)+`"},`, 180) +
		`{"name":"target","payload":"needle"}],"next_cursor":"cursor-final"}`
	var modelToolContent string
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&calls, 1) == 1 {
			data := `{"choices":[{"delta":{"content":"查找目标记录。","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"large_read","arguments":"{\"target\":\"needle\"}"}}]},"finish_reason":"tool_calls"}]}`
			_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
			return
		}
		for _, message := range request.Messages {
			if message.Role == "tool" && message.ToolCallID == "call-1" {
				modelToolContent = message.Content
			}
		}
		data := `{"choices":[{"delta":{"content":"已找到目标记录。"},"finish_reason":"stop"}]}`
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	defer srv.Close()

	registry := testRegistry(t, testAgentTool("large_read", ToolKindRead, func(context.Context, tool.Arguments) (string, error) {
		return fullContent, nil
	}))
	client := llm.NewLLMClientWithHTTP(srv.URL, "k", "test", 100, &http.Client{})
	observer := &captureObserver{}
	agent := NewAgent(client, NewToolExecutor(registry), AgentConfig{
		MaxSteps: 2, AnswerMaxTokens: 100, MaxContinueRounds: 0,
		Timeout: 5 * time.Second, AnswerReserve: time.Second,
	}, observer, nil)

	result, err := agent.RunWithPlan(
		t.Context(), "run_tool_delivery", "needle 对应的记录是什么？", nil, nil,
		domain.EvidencePlan{Sources: domain.Internal}, false,
	)
	if err != nil {
		t.Fatalf("RunWithPlan() error = %v", err)
	}
	if result.Err != nil || result.Answer != "已找到目标记录。" {
		t.Fatalf("result = %+v", result)
	}

	var trace *StepRecord
	for i := range observer.steps {
		if observer.steps[i].Kind == StepKindToolResult {
			trace = &observer.steps[i]
			break
		}
	}
	if trace == nil || trace.Content != fullContent || trace.PromptContent != fullContent {
		t.Fatalf("tool trace = %#v", trace)
	}
	if trace.AuthoritativeSHA256 == "" || trace.AuthoritativeSHA256 != trace.PromptSHA256 || trace.SizeBytes != int64(len(fullContent)) {
		t.Fatalf("tool trace hashes/size = %#v", trace)
	}
	if modelToolContent != fullContent {
		t.Fatalf("model tool content changed: got %d bytes, want %d", len(modelToolContent), len(fullContent))
	}
	if len(result.SessionMessages) != 2 || result.SessionMessages[1].Content != fullContent {
		t.Fatalf("session messages = %#v", result.SessionMessages)
	}
	if !strings.Contains(modelToolContent, `"name":"target"`) || !strings.Contains(modelToolContent, `"next_cursor":"cursor-final"`) {
		t.Fatalf("JSON tail was lost: %s", modelToolContent[len(modelToolContent)-200:])
	}
}

func TestForceConclusion_StreamsLiveAndRecordsAnswer(t *testing.T) {
	srv := fakeStreamServer(t, []streamEvent{
		{content: "答", finish: ""},
		{content: "案", finish: "stop"},
	})
	defer srv.Close()

	client := llm.NewLLMClientWithHTTP(srv.URL, "k", "test", 100, &http.Client{})
	obs := &captureObserver{}
	agent := NewAgent(client, nil, AgentConfig{
		MaxSteps: 1, AnswerMaxTokens: 100, MaxContinueRounds: 2,
		Timeout: 5 * time.Second, AnswerReserve: 4 * time.Second,
	}, obs, nil)

	seq := 0
	res, err := agent.forceConclusion(t.Context(), "run_test", nil, nil, &seq, time.Now())
	if err != nil {
		t.Fatalf("forceConclusion: %v", err)
	}
	if res.Content != "答案" {
		t.Fatalf("content = %q, want 答案", res.Content)
	}
	// The validated conclusion is published to the observer after generation.
	if got, want := strings.Join(obs.tokens, ""), "答案"; got != want {
		t.Fatalf("observed tokens = %q, want %q", got, want)
	}
	// And it must be recorded as an answer step.
	var sawAnswer bool
	for _, s := range obs.steps {
		if s.Kind == StepKindAnswer {
			sawAnswer = true
		}
	}
	if !sawAnswer {
		t.Fatalf("no answer step recorded; steps=%v", obs.steps)
	}
}

func TestForceConclusion_RetriesToolProtocolWithoutStreamingIt(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		content := "最终答案"
		if atomic.AddInt32(&calls, 1) == 1 {
			content = `<｜｜DSML｜｜tool_calls><｜｜DSML｜｜invoke name="web_fetch"></｜｜DSML｜｜invoke></｜｜DSML｜｜tool_calls>`
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: content}, FinishReason: "stop"}}}
		data, _ := json.Marshal(chunk)
		w.Write([]byte("data: " + string(data) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := llm.NewLLMClientWithHTTP(srv.URL, "k", "test", 100, &http.Client{})
	obs := &captureObserver{}
	agent := NewAgent(client, nil, AgentConfig{
		MaxSteps: 1, AnswerMaxTokens: 100, MaxContinueRounds: 0,
		Timeout: 5 * time.Second, AnswerReserve: 4 * time.Second,
	}, obs, nil)

	seq := 0
	res, err := agent.forceConclusion(t.Context(), "run_protocol", nil, nil, &seq, time.Now())
	if err != nil {
		t.Fatalf("forceConclusion() error = %v", err)
	}
	if res.Content != "最终答案" {
		t.Fatalf("content = %q, want 最终答案", res.Content)
	}
	if got := strings.Join(obs.tokens, ""); got != "最终答案" {
		t.Fatalf("streamed tokens = %q, want only repaired answer", got)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("LLM calls = %d, want 2", calls)
	}
}

func TestWithDefaults_AnswerReserveClamped(t *testing.T) {
	// Reserve at or beyond timeout must be halved so the loop always keeps room.
	cfg := AgentConfig{Timeout: 10 * time.Second, AnswerReserve: 30 * time.Second}.withDefaults()
	if cfg.AnswerReserve >= cfg.Timeout {
		t.Fatalf("reserve %s should be < timeout %s after clamping", cfg.AnswerReserve, cfg.Timeout)
	}
	if cfg.AnswerReserve != 5*time.Second {
		t.Fatalf("reserve = %s, want 5s (half of timeout)", cfg.AnswerReserve)
	}
	cfg = AgentConfig{Timeout: 10 * time.Second, AnswerReserve: 10 * time.Second}.withDefaults()
	if cfg.AnswerReserve != 5*time.Second {
		t.Fatalf("equal reserve = %s, want 5s (half of timeout)", cfg.AnswerReserve)
	}

	// ConclusionMaxTokens falls back to AnswerMaxTokens when unset (0). Timeout
	// and AnswerReserve are NOT defaulted here — config.Config provides those at
	// the call site (see AgentConfig doc comment).
	cfg = AgentConfig{AnswerMaxTokens: 6000}.withDefaults()
	if cfg.ConclusionMaxTokens != 6000 {
		t.Fatalf("ConclusionMaxTokens = %d, want 6000 (fallback to AnswerMaxTokens)", cfg.ConclusionMaxTokens)
	}

	// An explicit ConclusionMaxTokens is preserved.
	cfg = AgentConfig{AnswerMaxTokens: 6000, ConclusionMaxTokens: 8000}.withDefaults()
	if cfg.ConclusionMaxTokens != 8000 {
		t.Fatalf("ConclusionMaxTokens = %d, want 8000 (explicit value preserved)", cfg.ConclusionMaxTokens)
	}
}

// TestRun_LoopExhaustedFallsThroughToConclusion verifies the split time budget.
// If loopCtx expires, the run should still answer using the reserved runCtx tail.
// This guards the old empty-answer timeout failure.
func TestRun_LoopExhaustedFallsThroughToConclusion(t *testing.T) {
	// First LLM call (the loop turn) blocks until its request context is
	// cancelled, simulating the loop budget expiring mid-turn. The second call
	// (the forced conclusion, under the reserved runCtx) returns a real answer.
	var callCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		n := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// Hold the connection open until the loop context cancels the request.
			<-r.Context().Done()
			return // client already saw a context error; close the response
		}
		w.WriteHeader(http.StatusOK)
		chunk := streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: "结论"}, FinishReason: "stop"}}}
		data, _ := json.Marshal(chunk)
		w.Write([]byte("data: " + string(data) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	client := llm.NewLLMClientWithHTTP(srv.URL, "k", "test", 100, &http.Client{})
	obs := &captureObserver{}
	// Timeout 1.2s with 1.0s reserved → the loop gets only ~0.2s before its ctx
	// expires, then the conclusion has ~1.0s to finish.
	agent := NewAgent(client, NewToolExecutor(tool.NewRegistry()), AgentConfig{
		MaxSteps: 3, AnswerMaxTokens: 100, MaxContinueRounds: 0,
		Timeout: 1200 * time.Millisecond, AnswerReserve: 1000 * time.Millisecond,
	}, obs, nil)

	res, err := agent.RunWithPlan(t.Context(), "run_reserve", "q", nil, &retrieval.RetrievedContext{}, domain.EvidencePlan{Sources: domain.AllEvidence}, true)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Err != nil {
		t.Fatalf("expected no conclusion error, got %v", res.Err)
	}
	if res.Answer != "结论" {
		t.Fatalf("answer = %q, want 结论", res.Answer)
	}
	if atomic.LoadInt32(&callCount) < 2 {
		t.Fatalf("expected a second (conclusion) LLM call, got %d", callCount)
	}
}

func TestRun_PreservesPartialAnswerWhenLoopDeadlineExpires(t *testing.T) {
	var calls int32
	srv := partialAnswerServer(t, "回答进行到这里", &calls)
	defer srv.Close()

	observer := &captureObserver{}
	client := llm.NewLLMClientWithHTTP(srv.URL, "k", "test", 100, srv.Client())
	agent := NewAgent(client, nil, AgentConfig{
		MaxSteps: 1, AnswerMaxTokens: 100, MaxContinueRounds: 0,
		Timeout: 800 * time.Millisecond, AnswerReserve: 500 * time.Millisecond,
	}, observer, nil)

	result, err := agent.RunWithPlan(t.Context(), "run_partial_loop", "q", nil, &retrieval.RetrievedContext{}, domain.EvidencePlan{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err != nil || result.Answer != "回答进行到这里" {
		t.Fatalf("result = %#v", result)
	}
	if result.ForcedConclusion {
		t.Fatal("partial visible answer should not trigger a second conclusion")
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("LLM calls = %d, want 1", calls)
	}
	if got := strings.Join(observer.tokens, ""); got != result.Answer {
		t.Fatalf("streamed answer = %q, want %q", got, result.Answer)
	}
	if outcome := outcomeFor(result, nil); outcome.Status != RunStatusDone {
		t.Fatalf("outcome = %#v, want completed answer", outcome)
	}
}

func TestRun_PreservesPartialForcedConclusionWhenDeadlineExpires(t *testing.T) {
	var calls int32
	srv := partialAnswerServer(t, "结论尚未完成", &calls)
	defer srv.Close()

	observer := &captureObserver{}
	client := llm.NewLLMClientWithHTTP(srv.URL, "k", "test", 100, srv.Client())
	agent := NewAgent(client, nil, AgentConfig{
		MaxSteps: 0, AnswerMaxTokens: 100, MaxContinueRounds: 0,
		Timeout: 300 * time.Millisecond, AnswerReserve: 100 * time.Millisecond,
	}, observer, nil)

	result, err := agent.RunWithPlan(t.Context(), "run_partial_conclusion", "q", nil, &retrieval.RetrievedContext{}, domain.EvidencePlan{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err != nil || result.Answer != "结论尚未完成" || !result.ForcedConclusion {
		t.Fatalf("result = %#v", result)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("LLM calls = %d, want 1", calls)
	}
	if got := strings.Join(observer.tokens, ""); got != result.Answer {
		t.Fatalf("streamed conclusion = %q, want %q", got, result.Answer)
	}
	if outcome := outcomeFor(result, nil); outcome.Status != RunStatusDone {
		t.Fatalf("outcome = %#v, want completed answer", outcome)
	}
}

func TestRunRetriesOnlyCurrentRunAnswerContract(t *testing.T) {
	const (
		priorSerial = "SN-prior-round-complete"
		serial      = "SN-prefix-0123456789-suffix"
	)
	var calls int32
	var repairPrompt string
	var historicalContractVisible bool
	var currentContractVisible bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var request struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		call := atomic.AddInt32(&calls, 1)
		for _, message := range request.Messages {
			if message.Role != "system" || !strings.HasPrefix(message.Content, exactAnswerContractPrefix) {
				continue
			}
			historicalContractVisible = historicalContractVisible || strings.Contains(message.Content, priorSerial)
			currentContractVisible = currentContractVisible || strings.Contains(message.Content, serial)
		}
		content := "设备 SN：…0123456789…"
		if call == 1 {
			writeTestSSE(t, w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-exact","type":"function","function":{"name":"exact_read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if call == 3 {
			for _, message := range request.Messages {
				if message.Role == "user" && strings.Contains(message.Content, "exact-output validator") {
					repairPrompt = message.Content
				}
			}
			content = "本轮 SN：" + serial
		}
		encoded, _ := json.Marshal(streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: content}, FinishReason: "stop"}}})
		writeTestSSE(t, w, string(encoded))
	}))
	defer server.Close()

	registry := testRegistry(t, Tool{
		ID: "exact_read", Description: "test exact output", Kind: ToolKindRead,
		InputSchema: objectSchema(map[string]any{}, nil),
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{
				Content: `{"sn":"` + serial + `"}`,
				AnswerContract: tool.AnswerContract{
					RequiredLiterals: []string{serial},
				},
			}, nil
		}),
	})
	observer := &captureObserver{}
	agent := NewAgent(
		llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{}),
		NewToolExecutor(registry),
		AgentConfig{MaxSteps: 2, AnswerMaxTokens: 100, MaxContinueRounds: 0, Timeout: 5 * time.Second, AnswerReserve: time.Second},
		observer,
		nil,
	)

	priorMessage, ok := answerContractMessage(tool.AnswerContract{RequiredLiterals: []string{priorSerial}})
	if !ok {
		t.Fatal("prior answer contract was empty")
	}
	result, err := agent.RunWithContext(
		t.Context(), "run_exact_retry", "继续列出完整 SN",
		ConversationContext{Recent: []llm.Message{priorMessage}}, nil,
		domain.EvidencePlan{Sources: domain.Internal}, false,
	)
	if err != nil {
		t.Fatalf("RunWithPlan() error = %v", err)
	}
	if result.Err != nil || result.Answer != "本轮 SN："+serial {
		t.Fatalf("result = %#v", result)
	}
	if got := strings.Join(observer.tokens, ""); got != result.Answer {
		t.Fatalf("visible tokens = %q, want only validated answer %q", got, result.Answer)
	}
	if !strings.Contains(repairPrompt, serial) || strings.Contains(repairPrompt, priorSerial) || !strings.Contains(repairPrompt, "Never abbreviate") {
		t.Fatalf("repair prompt = %q", repairPrompt)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("LLM calls = %d, want 3", calls)
	}
	if historicalContractVisible || !currentContractVisible {
		t.Fatalf("contract visibility historical=%v current=%v", historicalContractVisible, currentContractVisible)
	}
	if len(result.SessionMessages) != 2 {
		t.Fatalf("session messages = %#v", result.SessionMessages)
	}
}

func TestRunFailsWhenRequiredLiteralStillMissing(t *testing.T) {
	const serial = "SN-must-remain-complete"
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			writeTestSSE(t, w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-exact","type":"function","function":{"name":"exact_read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		encoded, _ := json.Marshal(streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: "SN：…complete"}, FinishReason: "stop"}}})
		writeTestSSE(t, w, string(encoded))
	}))
	defer server.Close()

	registry := testRegistry(t, Tool{
		ID: "exact_read", Description: "test exact output", Kind: ToolKindRead,
		InputSchema: objectSchema(map[string]any{}, nil),
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{Content: serial, AnswerContract: tool.AnswerContract{RequiredLiterals: []string{serial}}}, nil
		}),
	})
	observer := &captureObserver{}
	agent := NewAgent(
		llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{}),
		NewToolExecutor(registry),
		AgentConfig{MaxSteps: 2, AnswerMaxTokens: 100, MaxContinueRounds: 0, Timeout: 5 * time.Second, AnswerReserve: time.Second},
		observer,
		nil,
	)

	result, err := agent.RunWithPlan(t.Context(), "run_exact_failure", "列出完整 SN", nil, nil, domain.EvidencePlan{Sources: domain.Internal}, false)
	if err != nil {
		t.Fatalf("RunWithPlan() error = %v", err)
	}
	if !errors.Is(result.Err, ErrAnswerContractViolation) || result.Answer != "" {
		t.Fatalf("result = %#v", result)
	}
	if got := strings.Join(observer.tokens, ""); got != "" {
		t.Fatalf("invalid answer leaked to client: %q", got)
	}
	if atomic.LoadInt32(&calls) != 4 {
		t.Fatalf("LLM calls = %d, want initial tool call + answer + 2 retries", calls)
	}
}

func TestBuildAgentMessagesDropsHistoricalAnswerContract(t *testing.T) {
	message, ok := answerContractMessage(tool.AnswerContract{RequiredLiterals: []string{"SN-history"}})
	if !ok {
		t.Fatal("answerContractMessage() returned no message")
	}
	agent := &Agent{}
	messages := agent.buildAgentMessages("current question", ConversationContext{Recent: []llm.Message{message}}, nil, domain.EvidencePlan{})
	for _, candidate := range messages {
		if strings.HasPrefix(candidate.Content, exactAnswerContractPrefix) {
			t.Fatalf("historical answer contract leaked into current run: %#v", candidate)
		}
	}
}

func writeTestSSE(t *testing.T, w http.ResponseWriter, data string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
}

func TestForceConclusionRejectsAnswerContractViolation(t *testing.T) {
	const serial = "SN-force-conclusion-complete"
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		atomic.AddInt32(&calls, 1)
		encoded, _ := json.Marshal(streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: "SN：…complete"}, FinishReason: "stop"}}})
		writeTestSSE(t, w, string(encoded))
	}))
	defer server.Close()

	observer := &captureObserver{}
	agent := NewAgent(
		llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{}),
		nil,
		AgentConfig{ConclusionMaxTokens: 100, MaxContinueRounds: 0},
		observer,
		nil,
	)
	contract := &exactAnswerContract{}
	contract.Add(tool.AnswerContract{RequiredLiterals: []string{serial}})
	seq := 0
	res, err := agent.forceConclusion(t.Context(), "run_force_exact", nil, contract, &seq, time.Now())
	if !errors.Is(err, ErrAnswerContractViolation) || res == nil {
		t.Fatalf("res=%#v err=%v", res, err)
	}
	if got := strings.Join(observer.tokens, ""); got != "" {
		t.Fatalf("invalid forced conclusion leaked to client: %q", got)
	}
	if atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("LLM calls = %d, want initial answer + 2 retries", calls)
	}
	for _, step := range observer.steps {
		if step.Kind == StepKindAnswer {
			t.Fatalf("invalid answer step was recorded: %#v", step)
		}
	}
}
