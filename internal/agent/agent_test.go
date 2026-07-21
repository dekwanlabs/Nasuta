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
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/llm"
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

// TestReasoningContent_ParsedStreamedCounted verifies reasoning_content handling.
// Deltas must be parsed, accumulated, counted, and streamed to OnReasoning.
// This guards the old bug where reasoning tokens were silently dropped.
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
	if res.ReasoningTokens != 2 {
		t.Fatalf("reasoning tokens = %d, want 2", res.ReasoningTokens)
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

func (c *captureObserver) OnStep(_ context.Context, _ string, s StepRecord) {
	c.steps = append(c.steps, s)
}
func (c *captureObserver) OnToken(_ context.Context, _ string, tok string) {
	c.tokens = append(c.tokens, tok)
}
func (c *captureObserver) OnReasoning(_ context.Context, _ string, tok string) {
	c.reasoning = append(c.reasoning, tok)
}

func TestRunPersistsFullToolOutputBeforeSendingCompressedModelContent(t *testing.T) {
	fullContent := `{"records":[` + strings.Repeat(`{"name":"other","payload":"`+strings.Repeat("x", 200)+`"},`, 180) +
		`{"name":"target","payload":"needle"}]}`
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
		t.Context(),
		"run_tool_compression",
		"needle 对应的记录是什么？",
		nil,
		nil,
		domain.EvidencePlan{Sources: domain.Internal},
		false,
	)
	if err != nil {
		t.Fatalf("RunWithPlan() error = %v", err)
	}
	if result.Err != nil || result.Answer != "已找到目标记录。" {
		t.Fatalf("result = %+v", result)
	}

	var persisted string
	for _, step := range observer.steps {
		if step.Kind == StepKindToolResult {
			persisted = step.Content
			break
		}
	}
	if persisted != fullContent {
		t.Fatalf("persisted tool output changed: got %d chars, want %d", len(persisted), len(fullContent))
	}
	if modelToolContent == "" || modelToolContent == fullContent {
		t.Fatalf("model tool content was not compressed")
	}
	if !strings.Contains(modelToolContent, `"_nasuta"`) ||
		!strings.Contains(modelToolContent, `"compressed":true`) ||
		!strings.Contains(modelToolContent, "needle") {
		t.Fatalf("model tool content missing envelope or relevant evidence: %s", modelToolContent)
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
	res, err := agent.forceConclusion(t.Context(), "run_test", nil, &seq, time.Now())
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
	res, err := agent.forceConclusion(t.Context(), "run_protocol", nil, &seq, time.Now())
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
	// Reserve larger than timeout must be halved so the loop always keeps room.
	cfg := AgentConfig{Timeout: 10 * time.Second, AnswerReserve: 30 * time.Second}.withDefaults()
	if cfg.AnswerReserve >= cfg.Timeout {
		t.Fatalf("reserve %s should be < timeout %s after clamping", cfg.AnswerReserve, cfg.Timeout)
	}
	if cfg.AnswerReserve != 5*time.Second {
		t.Fatalf("reserve = %s, want 5s (half of timeout)", cfg.AnswerReserve)
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
	agent := NewAgent(client, NewToolExecutor(nil), AgentConfig{
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
