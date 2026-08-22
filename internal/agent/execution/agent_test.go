package execution

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
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
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
	return NewAgent(client, nil, Config{
		MaxSteps:          5,
		AnswerMaxTokens:   100,
		MaxContinueRounds: 2,
		Timeout:           10 * time.Second,
		AnswerReserve:     5 * time.Second,
	}, nil, nil)
}

func TestAgentModelTurnTraceContract(t *testing.T) {
	server := fakeStreamServer(t, []streamEvent{
		{reasoning: "分析"},
		{content: "结论", finish: "stop"},
	})
	defer server.Close()

	var events []domain.EvaluationTrace
	ctx := runtrace.WithScope(t.Context(), runtrace.NewScope(runtrace.Evaluation, func(event domain.EvaluationTrace) {
		events = append(events, event)
	}))
	result, err := newTestAgent(t, server.URL).RunWithPlan(ctx, "trace_model_turn", "问题", nil, nil, domain.EvidencePlan{}, false)
	if err != nil || result.Err != nil {
		t.Fatalf("RunWithPlan() error = %v, result error = %v", err, result.Err)
	}

	event := traceEventByNode(t, events, "agent_model_turn")
	if event.Status != "completed" || event.DurationMS < 0 {
		t.Fatalf("agent_model_turn status = %q duration = %d", event.Status, event.DurationMS)
	}
	if _, ok := event.Input["step"].(int); !ok {
		t.Fatalf("step = %#v, want int", event.Input["step"])
	}
	if _, ok := event.Input["messages"].(int); !ok {
		t.Fatalf("messages = %#v, want int", event.Input["messages"])
	}
	if _, ok := event.Input["tools"].(int); !ok {
		t.Fatalf("tools = %#v, want int", event.Input["tools"])
	}
	if event.Output["finish_reason"] != "stop" || event.Output["content_chars"] != 2 {
		t.Fatalf("agent_model_turn output = %#v", event.Output)
	}
	for _, field := range []string{
		"reasoning_tokens", "first_event_ms", "first_reasoning_ms", "first_content_ms", "first_tool_delta_ms", "first_tool_call_ms",
	} {
		if _, ok := event.Output[field].(int64); !ok && field != "reasoning_tokens" {
			t.Fatalf("%s = %#v, want int64", field, event.Output[field])
		}
	}
	if _, ok := event.Output["reasoning_tokens"].(int); !ok {
		t.Fatalf("reasoning_tokens = %#v, want int", event.Output["reasoning_tokens"])
	}
	if _, ok := event.Output["tool_calls"].([]string); !ok {
		t.Fatalf("tool_calls = %#v, want []string", event.Output["tool_calls"])
	}
}

func TestAgentModelTurnTraceClassifiesCancellation(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := runtrace.WithScope(t.Context(), runtrace.NewScope(runtrace.Evaluation, func(event domain.EvaluationTrace) {
		events = append(events, event)
	}))
	stream := newStreamPipe(NoopObserver(), "cancelled_turn", 1, time.Now(), nil)
	_, err := runtrace.Invoke(ctx, agentModelTurnSpec, agentModelTurnInput{Step: 1, Stream: stream},
		func(context.Context, agentModelTurnInput) (agentModelTurnOutput, error) {
			return agentModelTurnOutput{Timing: stream.Timings()}, context.Canceled
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke() error = %v, want context.Canceled", err)
	}
	event := traceEventByNode(t, events, "agent_model_turn")
	if event.Status != "cancelled" || event.Output["error"] != context.Canceled.Error() {
		t.Fatalf("cancelled event = %#v", event)
	}
}

func TestForceConclusionTraceContractAndTokenOrder(t *testing.T) {
	server := fakeStreamServer(t, []streamEvent{{content: "最终结论", finish: "stop"}})
	defer server.Close()

	var events []domain.EvaluationTrace
	ctx := runtrace.WithScope(t.Context(), runtrace.NewScope(runtrace.Evaluation, func(event domain.EvaluationTrace) {
		events = append(events, event)
	}))
	agent := newTestAgent(t, server.URL)
	agent.cfg.MaxSteps = 0
	result, err := agent.RunWithPlan(ctx, "trace_force_conclusion", "问题", nil, nil, domain.EvidencePlan{}, false)
	if err != nil || result.Err != nil || result.Answer != "最终结论" {
		t.Fatalf("result = %#v, error = %v", result, err)
	}

	conclusion := traceEventByNode(t, events, "force_conclusion")
	firstToken := traceEventByNode(t, events, "first_answer_token")
	if conclusion.Status != "completed" || conclusion.Sequence >= firstToken.Sequence {
		t.Fatalf("conclusion = %#v, first token = %#v", conclusion, firstToken)
	}
	if conclusion.Output["finish_reason"] != "stop" || conclusion.Output["content_chars"] != 4 {
		t.Fatalf("force_conclusion output = %#v", conclusion.Output)
	}
	if firstToken.Output["step"] != "force_conclusion" {
		t.Fatalf("first_answer_token output = %#v", firstToken.Output)
	}
}

func traceEventByNode(t *testing.T, events []domain.EvaluationTrace, node string) domain.EvaluationTrace {
	t.Helper()
	for _, event := range events {
		if event.Node == node {
			return event
		}
	}
	t.Fatalf("trace node %q not found in %#v", node, events)
	return domain.EvaluationTrace{}
}

func TestMaxStepsForQuestion(t *testing.T) {
	agent := &Agent{cfg: Config{MaxSteps: 5}}
	for _, question := range []string{
		"what does this service do",
		"review this architecture",
		"trace the caller call chain",
		"why did this request timeout",
	} {
		if got := agent.MaxStepsFor(question); got != 5 {
			t.Errorf("MaxStepsFor(%q) = %d, want configured limit 5", question, got)
		}
	}
}

func TestMaxStepsForWebPlanUsesConfiguredLimit(t *testing.T) {
	agent := &Agent{cfg: Config{MaxSteps: 5}}
	plan := domain.EvidencePlan{Sources: domain.Web}
	if got := agent.MaxStepsForPlan("school information", plan); got != 5 {
		t.Fatalf("MaxStepsForPlan() = %d, want 5", got)
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
	agent := NewAgent(client, NewToolExecutor(registry), Config{
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
	agent := NewAgent(client, NewToolExecutor(registry), Config{
		MaxSteps: 2, AnswerMaxTokens: 100, Timeout: 5 * time.Second, AnswerReserve: time.Second,
	}, nil, nil)
	plan := domain.EvidencePlan{Sources: domain.Internal}
	policy := ToolPolicyForRun(false)
	result, err := agent.runWithSnapshot(
		t.Context(), "run_preferred_tool", "继续", ConversationContext{Instructions: []llm.Message{{
			Role: "system", Content: "Prefer runtime_evidence when it is useful, but answer directly when existing context is sufficient.",
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

func TestRunForcesConclusionWhenParallelToolCallsExceedBudget(t *testing.T) {
	var modelCalls int32
	var conclusionUsedTools atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		call := atomic.AddInt32(&modelCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			writeTestSSE(t, w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"evidence","arguments":"{}"}},{"index":1,"id":"call-2","type":"function","function":{"name":"evidence","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode conclusion request: %v", err)
		}
		_, conclusionUsedToolsValue := request["tools"]
		conclusionUsedTools.Store(conclusionUsedToolsValue)
		writeTestSSE(t, w, `{"choices":[{"delta":{"content":"已根据现有证据完成总结。"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	var toolCalls int32
	registry := testRegistry(t, Tool{
		ID: "evidence", Description: "test evidence", Kind: ToolKindRead,
		InputSchema: objectSchema(nil, nil),
		Handler: stringHandler(func(context.Context, tool.Arguments) (string, error) {
			atomic.AddInt32(&toolCalls, 1)
			return `{"found":true}`, nil
		}),
	})
	client := llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{})
	agent := NewAgent(client, NewToolExecutor(registry), Config{
		MaxSteps: 2, MaxToolCalls: 1, AnswerMaxTokens: 100,
		Timeout: 5 * time.Second, AnswerReserve: time.Second,
	}, nil, nil)
	result, err := agent.RunWithPlan(
		t.Context(),
		"run_tool_budget",
		"查找证据",
		nil,
		nil,
		domain.EvidencePlan{Sources: domain.Internal},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err != nil || result.Answer != "已根据现有证据完成总结。" ||
		!result.ForcedConclusion || !result.Evidence.ForcedConclusion {
		t.Fatalf("result = %#v", result)
	}
	if result.Evidence.ToolCallCount != 1 ||
		result.Evidence.ResultCount != 1 ||
		atomic.LoadInt32(&toolCalls) != 1 {
		t.Fatalf(
			"evidence calls=%d results=%d executed calls=%d",
			result.Evidence.ToolCallCount,
			result.Evidence.ResultCount,
			atomic.LoadInt32(&toolCalls),
		)
	}
	if atomic.LoadInt32(&modelCalls) != 2 || conclusionUsedTools.Load() {
		t.Fatalf(
			"model calls = %d, conclusion used tools = %t",
			atomic.LoadInt32(&modelCalls),
			conclusionUsedTools.Load(),
		)
	}
	if len(result.SessionMessages) != 3 ||
		result.SessionMessages[1].ToolCallID != "call-1" ||
		result.SessionMessages[2].ToolCallID != "call-2" ||
		!strings.Contains(result.SessionMessages[2].Content, "tool_call_budget_exhausted") {
		t.Fatalf("session messages = %#v", result.SessionMessages)
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
	agent := NewAgent(client, NewToolExecutor(registry), Config{
		MaxSteps: 1, AnswerMaxTokens: 100, Timeout: 5 * time.Second, AnswerReserve: time.Second,
	}, observer, nil)
	plan := domain.EvidencePlan{Sources: domain.Internal}
	policy := ToolPolicyForRun(false)
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

func TestContinueIfNeeded_StructuredOutputUsesJSONSuffix(t *testing.T) {
	var calls int32
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		_ = r.Body.Close()
		requests = append(requests, string(body))
		call := atomic.AddInt32(&calls, 1)
		content := `{"summary":"partial","findings":[`
		finish := llm.FinishLength
		if call > 1 {
			content = `{"claim":"ok"}],"gaps":[]}`
			finish = llm.FinishStop
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(streamChunkJS{
			Choices: []streamChoiceJS{{
				Delta:        streamDeltaJS{Content: content},
				FinishReason: finish,
			}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()

	agent := newTestAgent(t, server.URL)
	agent.cfg.StructuredOutput = true
	res, err := agent.llm.ChatWithToolsMax(t.Context(), nil, nil, nil, 100)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	res, err = agent.continueIfNeeded(t.Context(), nil, res, 100, nil)
	if err != nil {
		t.Fatalf("structured continuation: %v", err)
	}
	if want := `{"summary":"partial","findings":[{"claim":"ok"}],"gaps":[]}`; res.Content != want {
		t.Fatalf("structured content = %q, want %q", res.Content, want)
	}
	if !json.Valid([]byte(res.Content)) {
		t.Fatalf("structured content is not valid JSON: %q", res.Content)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("LLM calls = %d, want 2", got)
	}
	if len(requests) != 2 ||
		!strings.Contains(requests[1], "Return only the missing JSON suffix") ||
		strings.Contains(requests[1], "Continue from where you left off") {
		t.Fatalf("continuation request did not use the structured instruction: %q", requests)
	}
}

func TestContinueIfNeeded_StructuredOutputDoesNotContinueCompleteJSON(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(streamChunkJS{Choices: []streamChoiceJS{{
			Delta:        streamDeltaJS{Content: `{"summary":"complete","findings":[]}`},
			FinishReason: llm.FinishLength,
		}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()

	agent := newTestAgent(t, server.URL)
	agent.cfg.StructuredOutput = true
	res, err := agent.llm.ChatWithToolsMax(t.Context(), nil, nil, nil, 100)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	res, err = agent.continueIfNeeded(t.Context(), nil, res, 100, nil)
	if err != nil {
		t.Fatalf("structured continuation: %v", err)
	}
	if res.Content != `{"summary":"complete","findings":[]}` {
		t.Fatalf("content = %q", res.Content)
	}
	if res.FinishReason != llm.FinishLength {
		t.Fatalf("finish reason = %q, want original length marker", res.FinishReason)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("LLM calls = %d, want 1", got)
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
	if !errors.Is(cerr, ErrAnswerTruncated) {
		t.Fatalf("continueIfNeeded: got %v, want ErrAnswerTruncated", cerr)
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
	agent := NewAgent(client, nil, Config{
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

type failingStepObserver struct {
	steps []StepRecord
	err   error
}

func (observer *failingStepObserver) OnStep(
	_ context.Context,
	_ string,
	step StepRecord,
) error {
	observer.steps = append(observer.steps, step)
	return observer.err
}

func (*failingStepObserver) OnToken(context.Context, string, string) {}

func (*failingStepObserver) OnReasoning(context.Context, string, string) {}

func TestExecuteToolTurnStopsWhenToolCallCannotBePersisted(t *testing.T) {
	observer := &failingStepObserver{err: errors.New("database unavailable")}
	agent := &Agent{observer: observer}
	state := &compiledLoop{
		ctx:     t.Context(),
		runCtx:  t.Context(),
		loopCtx: t.Context(),
		runID:   "run-tool-call-persist-failed",
		result:  &RunResult{},
	}

	agent.executeToolTurn(state, []llm.ToolCall{{
		ID: "call-persist-failed",
		Function: llm.ToolFunction{
			Name:      "observe",
			Arguments: `{"query":"checkout"}`,
		},
	}})

	if state.result.Err == nil || !strings.Contains(state.result.Err.Error(), "persist tool call") {
		t.Fatalf("result error = %v", state.result.Err)
	}
	if len(observer.steps) != 1 ||
		observer.steps[0].Kind != StepKindToolCall ||
		observer.steps[0].ToolCallID != "call-persist-failed" {
		t.Fatalf("steps = %#v", observer.steps)
	}
}

func TestRecordThinkTurnPersistsToolReasoning(t *testing.T) {
	observer := &captureObserver{}
	agent := &Agent{observer: observer}
	state := &compiledLoop{
		runCtx: t.Context(),
		runID:  "run-tool-reasoning",
		result: &RunResult{},
	}
	turn := modelTurn{
		step: 2,
		result: &llm.ChatStreamResult{
			Content:         "I will inspect the runtime evidence.",
			Reasoning:       "The request depends on current runtime state, so I need the observe tool.",
			ReasoningTokens: 17,
			ToolCalls: []llm.ToolCall{{
				ID: "call-observe",
				Function: llm.ToolFunction{
					Name:      "observe",
					Arguments: `{"query":"checkout"}`,
				},
			}},
		},
		stream:   newStreamPipe(observer, state.runID, 2, time.Now(), nil),
		started:  time.Now(),
		duration: 25 * time.Millisecond,
	}

	if err := agent.recordThinkTurn(state, turn); err != nil {
		t.Fatalf("recordThinkTurn: %v", err)
	}
	if len(observer.steps) != 1 {
		t.Fatalf("steps = %#v", observer.steps)
	}
	step := observer.steps[0]
	if step.Kind != StepKindThink ||
		step.Content != turn.result.Reasoning ||
		step.PromptContent != turn.result.Content ||
		step.AuthoritativeSHA256 != toolContentSHA256(turn.result.Reasoning) ||
		step.PromptSHA256 != toolContentSHA256(turn.result.Content) ||
		step.SizeBytes != int64(len(turn.result.Reasoning)) ||
		step.TokenDelta != utf8.RuneCountInString(turn.result.Reasoning) ||
		step.ReasoningTokens != 17 ||
		step.DurationMs != 25 {
		t.Fatalf("tool reasoning step = %#v", step)
	}
	if len(state.messages) != 1 || len(state.messages[0].ToolCalls) != 1 ||
		state.messages[0].Content != turn.result.Content ||
		len(state.result.SessionMessages) != 1 {
		t.Fatalf("state messages = %#v, session = %#v", state.messages, state.result.SessionMessages)
	}
}

func TestRecordThinkTurnStopsWhenReasoningCannotBePersisted(t *testing.T) {
	observer := &failingStepObserver{err: errors.New("database unavailable")}
	agent := &Agent{observer: observer}
	state := &compiledLoop{
		runCtx: t.Context(),
		runID:  "run-tool-reasoning-persist-failed",
		result: &RunResult{},
	}

	err := agent.recordThinkTurn(state, modelTurn{
		step: 1,
		result: &llm.ChatStreamResult{
			Reasoning: "The tool is required.",
			ToolCalls: []llm.ToolCall{{
				ID: "call-observe",
				Function: llm.ToolFunction{
					Name: "observe",
				},
			}},
		},
		stream:  newStreamPipe(observer, state.runID, 1, time.Now(), nil),
		started: time.Now(),
	})

	if err == nil || !strings.Contains(err.Error(), "persist tool reasoning") {
		t.Fatalf("error = %v", err)
	}
	if len(observer.steps) != 1 || observer.steps[0].Content != "The tool is required." {
		t.Fatalf("steps = %#v", observer.steps)
	}
	if len(state.messages) != 0 || len(state.result.SessionMessages) != 0 {
		t.Fatalf("messages were appended after persistence failure: %#v", state.messages)
	}
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
	agent := NewAgent(client, NewToolExecutor(registry), Config{
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

	var callTrace, resultTrace *StepRecord
	for i := range observer.steps {
		switch observer.steps[i].Kind {
		case StepKindToolCall:
			callTrace = &observer.steps[i]
		case StepKindToolResult:
			resultTrace = &observer.steps[i]
		}
	}
	if callTrace == nil || callTrace.ToolCallID != "call-1" ||
		callTrace.Tool != "large_read" ||
		callTrace.Args != `{"target":"needle"}` {
		t.Fatalf("tool call trace = %#v", callTrace)
	}
	if resultTrace == nil || resultTrace.ToolCallID != callTrace.ToolCallID ||
		resultTrace.Content != fullContent || resultTrace.PromptContent != fullContent {
		t.Fatalf("tool result trace = %#v", resultTrace)
	}
	if resultTrace.AuthoritativeSHA256 == "" ||
		resultTrace.AuthoritativeSHA256 != resultTrace.PromptSHA256 ||
		resultTrace.SizeBytes != int64(len(fullContent)) {
		t.Fatalf("tool trace hashes/size = %#v", resultTrace)
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
	agent := NewAgent(client, nil, Config{
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
	agent := NewAgent(client, nil, Config{
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

func TestWithDefaults_DoesNotRewriteInvalidAnswerReserve(t *testing.T) {
	cfg := Config{Timeout: 10 * time.Second, AnswerReserve: 30 * time.Second}.withDefaults()
	if cfg.AnswerReserve != 30*time.Second {
		t.Fatalf("reserve = %s, want configured value preserved", cfg.AnswerReserve)
	}

	// ConclusionMaxTokens falls back to AnswerMaxTokens when unset (0). Timeout
	// and AnswerReserve are NOT defaulted here — config.Config provides those at
	// the call site (see Config doc comment).
	cfg = Config{AnswerMaxTokens: 6000}.withDefaults()
	if cfg.ConclusionMaxTokens != 6000 {
		t.Fatalf("ConclusionMaxTokens = %d, want 6000 (fallback to AnswerMaxTokens)", cfg.ConclusionMaxTokens)
	}
	if cfg.ConclusionRetryMaxTokens != 1024 {
		t.Fatalf("ConclusionRetryMaxTokens = %d, want 1024 (capped quarter-budget retry)", cfg.ConclusionRetryMaxTokens)
	}

	// An explicit ConclusionMaxTokens is preserved.
	cfg = Config{AnswerMaxTokens: 6000, ConclusionMaxTokens: 8000}.withDefaults()
	if cfg.ConclusionMaxTokens != 8000 {
		t.Fatalf("ConclusionMaxTokens = %d, want 8000 (explicit value preserved)", cfg.ConclusionMaxTokens)
	}
	if cfg.ConclusionRetryMaxTokens != 1024 {
		t.Fatalf("ConclusionRetryMaxTokens = %d, want 1024 (capped retry budget)", cfg.ConclusionRetryMaxTokens)
	}

	cfg = Config{
		ConclusionMaxTokens:      100,
		ConclusionRetryMaxTokens: 200,
	}.withDefaults()
	if cfg.ConclusionRetryMaxTokens != 100 {
		t.Fatalf("ConclusionRetryMaxTokens = %d, want 100 (bounded by conclusion budget)", cfg.ConclusionRetryMaxTokens)
	}
}

func TestForceConclusion_UsesSmallBudgetForReasoningRetry(t *testing.T) {
	var calls int32
	var budgets []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		_ = r.Body.Close()
		var request struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		budgets = append(budgets, request.MaxTokens)
		n := atomic.AddInt32(&calls, 1)
		content := ""
		reasoning := "继续分析"
		finish := llm.FinishLength
		if n == 2 {
			content = "直接结论"
			reasoning = ""
			finish = llm.FinishStop
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(streamChunkJS{Choices: []streamChoiceJS{{
			Delta:        streamDeltaJS{Content: content, ReasoningContent: reasoning},
			FinishReason: finish,
		}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()

	agent := NewAgent(
		llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{}),
		nil,
		Config{ConclusionMaxTokens: 12000, MaxContinueRounds: 0},
		nil,
		nil,
	)
	seq := 0
	res, err := agent.forceConclusion(t.Context(), "run_small_retry", nil, nil, &seq, time.Now())
	if err != nil {
		t.Fatalf("forceConclusion() error = %v", err)
	}
	if res == nil || res.Content != "直接结论" {
		t.Fatalf("result = %#v", res)
	}
	if got, want := atomic.LoadInt32(&calls), int32(2); got != want {
		t.Fatalf("LLM calls = %d, want %d", got, want)
	}
	if len(budgets) != 2 || budgets[0] != 12000 || budgets[1] != 1024 {
		t.Fatalf("max_tokens = %v, want [12000 1024]", budgets)
	}
}

func TestForceConclusion_ReasoningRetryRunsOnlyOnce(t *testing.T) {
	var calls int32
	var budgets []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
			return
		}
		_ = r.Body.Close()
		var request struct {
			MaxTokens int `json:"max_tokens"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		budgets = append(budgets, request.MaxTokens)
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(streamChunkJS{Choices: []streamChoiceJS{{
			Delta:        streamDeltaJS{ReasoningContent: "思考"},
			FinishReason: llm.FinishLength,
		}}})
		_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()

	agent := NewAgent(
		llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{}),
		nil,
		Config{ConclusionMaxTokens: 800, MaxContinueRounds: 0},
		nil,
		nil,
	)
	seq := 0
	_, err := agent.forceConclusion(t.Context(), "run_retry_once", nil, nil, &seq, time.Now())
	if !errors.Is(err, ErrReasoningTruncated) {
		t.Fatalf("forceConclusion() error = %v, want ErrReasoningTruncated", err)
	}
	if got, want := atomic.LoadInt32(&calls), int32(2); got != want {
		t.Fatalf("LLM calls = %d, want %d", got, want)
	}
	if len(budgets) != 2 || budgets[0] != 800 || budgets[1] != 200 {
		t.Fatalf("max_tokens = %v, want [800 200]", budgets)
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
	agent := NewAgent(client, NewToolExecutor(tool.NewRegistry()), Config{
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
	agent := NewAgent(client, nil, Config{
		MaxSteps: 1, AnswerMaxTokens: 100, MaxContinueRounds: 0,
		Timeout: 800 * time.Millisecond, AnswerReserve: 500 * time.Millisecond,
	}, observer, nil)

	result, err := agent.RunWithPlan(t.Context(), "run_partial_loop", "q", nil, &retrieval.RetrievedContext{}, domain.EvidencePlan{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err == nil || result.Answer != "回答进行到这里" {
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
}

func TestRun_PreservesPartialForcedConclusionWhenDeadlineExpires(t *testing.T) {
	var calls int32
	srv := partialAnswerServer(t, "结论尚未完成", &calls)
	defer srv.Close()

	observer := &captureObserver{}
	client := llm.NewLLMClientWithHTTP(srv.URL, "k", "test", 100, srv.Client())
	agent := NewAgent(client, nil, Config{
		MaxSteps: 0, AnswerMaxTokens: 100, MaxContinueRounds: 0,
		Timeout: 300 * time.Millisecond, AnswerReserve: 100 * time.Millisecond,
	}, observer, nil)

	result, err := agent.RunWithPlan(t.Context(), "run_partial_conclusion", "q", nil, &retrieval.RetrievedContext{}, domain.EvidencePlan{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Err == nil || result.Answer != "结论尚未完成" || !result.ForcedConclusion {
		t.Fatalf("result = %#v", result)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("LLM calls = %d, want 1", calls)
	}
	if got := strings.Join(observer.tokens, ""); got != result.Answer {
		t.Fatalf("streamed conclusion = %q, want %q", got, result.Answer)
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
				if message.Role == "user" && strings.Contains(message.Content, "final-answer validator") {
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
		Config{MaxSteps: 2, AnswerMaxTokens: 100, MaxContinueRounds: 0, Timeout: 5 * time.Second, AnswerReserve: time.Second},
		observer,
		nil,
	)

	priorMessage, ok := contractMessage(tool.AnswerContract{RequiredLiterals: []string{priorSerial}})
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
		Config{MaxSteps: 2, AnswerMaxTokens: 100, MaxContinueRounds: 0, Timeout: 5 * time.Second, AnswerReserve: time.Second},
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
	message, ok := contractMessage(tool.AnswerContract{RequiredLiterals: []string{"SN-history"}})
	if !ok {
		t.Fatal("answerContractMessage() returned no message")
	}
	agent := &Agent{}
	messages := agent.buildMessages("current question", domain.QueryPlan{Kind: domain.QueryFocusedFact}, ConversationContext{Recent: []llm.Message{message}}, nil, domain.EvidencePlan{})
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
		Config{ConclusionMaxTokens: 100, MaxContinueRounds: 0},
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

func TestContinueIfNeededPreservesPartialContentOnProviderError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			writeTestSSE(t, w, `{"choices":[{"delta":{"content":"partial answer"},"finish_reason":"length"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"continuation rejected"}}`))
	}))
	defer srv.Close()

	agent := newTestAgent(t, srv.URL)
	agent.cfg.MaxContinueRounds = 1
	res, err := agent.llm.ChatWithToolsMax(t.Context(), nil, nil, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	got, continuationErr := agent.continueIfNeeded(t.Context(), nil, res, 100, nil)
	if continuationErr == nil || !strings.Contains(continuationErr.Error(), "continuation round 1") {
		t.Fatalf("continuation error = %v", continuationErr)
	}
	if got != res || got.Content != "partial answer" || got.FinishReason != llm.FinishLength {
		t.Fatalf("partial result = %+v", got)
	}
}
