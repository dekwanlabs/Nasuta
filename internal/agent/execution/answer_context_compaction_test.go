package execution

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestCompactRunContextBeforeAnswerWaitsForHighWater(t *testing.T) {
	agent, state, _, _, _ := answerCompactionFixture(t)
	observer := &answerCompactionObserver{}
	agent.observer = observer
	projected := estimateMessagesTokens(state.messages) + agent.outputTokenReserve()
	agent.cfg.ContextWindow = projected * 2
	before := append([]llm.Message(nil), state.messages...)

	result, err := agent.compactRunContextBeforeAnswer(state, nil, "test")
	if err != nil {
		t.Fatalf("compactRunContextBeforeAnswer: %v", err)
	}
	if result.Triggered || result.Applied {
		t.Fatalf("result = %+v, want no compaction below high water", result)
	}
	if !reflect.DeepEqual(state.messages, before) {
		t.Fatal("messages changed below high water")
	}
	if len(observer.contextUsage) != 1 ||
		observer.contextUsage[0].ProjectedBeforeTokens != projected ||
		observer.contextUsage[0].CompactionTriggered {
		t.Fatalf("context usage events = %+v", observer.contextUsage)
	}
}

func TestEnsureTurnBudgetCompactsBeforePostToolModelCall(t *testing.T) {
	agent, state, oldContent, recentContent, contractMessage := answerCompactionFixture(t)
	observer := &answerCompactionObserver{}
	agent.observer = observer

	if err := agent.ensureTurnBudget(state, 2); err != nil {
		t.Fatalf("ensureTurnBudget: %v", err)
	}
	if state.messages[3].Content == oldContent {
		t.Fatal("older tool result was not compacted")
	}
	if state.messages[5].Content != recentContent {
		t.Fatalf("recent tool result changed: %q", state.messages[5].Content)
	}
	if !reflect.DeepEqual(state.messages[6], contractMessage) {
		t.Fatalf("answer contract changed: %#v", state.messages[6])
	}
	if state.messages[1].Content != state.input.Question {
		t.Fatalf("current question changed: %q", state.messages[1].Content)
	}
	if len(state.messages[2].ToolCalls) != 1 ||
		!completeToolResultGroup(state.messages[2].ToolCalls, state.messages[3:4]) {
		t.Fatalf("older tool protocol group is invalid: %#v", state.messages[2:4])
	}
	if len(state.messages[4].ToolCalls) != 1 ||
		!completeToolResultGroup(state.messages[4].ToolCalls, state.messages[5:6]) {
		t.Fatalf("recent tool protocol group is invalid: %#v", state.messages[4:6])
	}
	if state.result.SessionMessages[1].Content != oldContent ||
		state.result.SessionMessages[3].Content != recentContent {
		t.Fatal("session messages were modified by runtime compaction")
	}
	if len(observer.phases) != 1 ||
		observer.phases[0] != "上下文已压缩，正在继续处理问题" {
		t.Fatalf("compaction phases = %#v", observer.phases)
	}
	if len(observer.contextUsage) != 1 ||
		!observer.contextUsage[0].CompactionTriggered ||
		!observer.contextUsage[0].CompactionApplied ||
		observer.contextUsage[0].ProjectedAfterTokens >= observer.contextUsage[0].ProjectedBeforeTokens {
		t.Fatalf("context usage events = %+v", observer.contextUsage)
	}
}

func TestCompactRunContextBeforeAnswerCanTightenExistingProjection(t *testing.T) {
	agent, state, _, _, _ := answerCompactionFixture(t)

	first, err := agent.compactRunContextBeforeAnswer(state, nil, "first")
	if err != nil {
		t.Fatalf("first compaction: %v", err)
	}
	firstProjection := state.messages[3].Content
	firstTokens := estimateMessagesTokens(state.messages)
	if !first.Applied {
		t.Fatalf("first result = %+v, want applied compaction", first)
	}

	nextCall := llm.ToolCall{
		ID: "call-third", Type: "function",
		Function: llm.ToolFunction{Name: "search_third", Arguments: `{"query":"third"}`},
	}
	thirdContent := strings.Repeat("third diagnostic payload\n", 4000)
	state.messages = append(state.messages,
		llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{nextCall}},
		toolMessage(nextCall.ID, nextCall.Function.Name, thirdContent),
	)

	second, err := agent.compactRunContextBeforeAnswer(state, nil, "second")
	if err != nil {
		t.Fatalf("second compaction: %v", err)
	}
	if !second.Applied {
		t.Fatalf("second result = %+v, want applied compaction", second)
	}
	if state.messages[3].Content == firstProjection {
		t.Fatal("previously compacted tool result was not tightened")
	}
	if got := estimateMessagesTokens(state.messages); got >= firstTokens+estimateMessagesTokens(state.messages[7:]) {
		t.Fatalf("second projection did not reclaim tokens: got=%d first=%d", got, firstTokens)
	}
	if len(second.ToolResults) == 0 || second.ToolResults[0].ToolCallID != "call-old" {
		t.Fatalf("second compaction details = %#v", second.ToolResults)
	}
	if second.ToolResults[0].OriginalTokens <= second.ToolResults[0].BeforeTokens {
		t.Fatalf("second compaction did not reuse authoritative source: %#v", second.ToolResults[0])
	}
}

func TestCompactRunContextBeforeAnswerRecordsTraceDetails(t *testing.T) {
	agent, state, _, _, _ := answerCompactionFixture(t)
	scope := executiontrace.NewScope(executiontrace.Evaluation, nil)
	state.ctx = executiontrace.WithScope(t.Context(), scope)

	result, err := agent.compactRunContextBeforeAnswer(state, nil, "trace_test")
	if err != nil {
		t.Fatalf("compactRunContextBeforeAnswer: %v", err)
	}
	if !result.Applied {
		t.Fatalf("result = %+v, want applied compaction", result)
	}
	events := scope.Snapshot()
	if len(events) != 1 || events[0].Node != "answer_context_compaction" {
		t.Fatalf("events = %#v", events)
	}
	if got := events[0].Output["phase"]; got != "trace_test" {
		t.Fatalf("trace phase = %#v", got)
	}
	details, ok := events[0].Output["tool_results"].([]answerToolResultCompaction)
	if !ok || len(details) == 0 || details[0].ToolCallID != "call-old" ||
		details[0].RetainedTokens >= details[0].BeforeTokens {
		t.Fatalf("trace tool results = %#v", events[0].Output["tool_results"])
	}
}

func TestFinishCompiledLoopCompactsBeforeForcedConclusion(t *testing.T) {
	agent, state, oldContent, recentContent, contractMessage := answerCompactionFixture(t)
	var requestMessages []llm.Message
	agent.llm = llm.NewLLMClientWithHTTP(
		"http://answer-compaction.test",
		"k",
		"test",
		agent.cfg.ConclusionMaxTokens,
		&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			defer request.Body.Close()
			var payload struct {
				Messages []llm.Message `json:"messages"`
			}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				t.Fatalf("decode model request: %v", err)
			}
			requestMessages = payload.Messages
			body := `data: {"choices":[{"delta":{"content":"final SN-required"},"finish_reason":"stop"}]}` +
				"\n\ndata: [DONE]\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    request,
			}, nil
		})},
	)

	agent.finishCompiledLoop(state)

	if state.result.Err != nil {
		t.Fatalf("finishCompiledLoop result error: %v", state.result.Err)
	}
	if state.result.Answer != "final SN-required" || !state.result.ForcedConclusion {
		t.Fatalf("result = %+v", state.result)
	}
	if len(requestMessages) == 0 {
		t.Fatal("forced conclusion model was not called")
	}
	if requestMessages[3].Content == oldContent {
		t.Fatal("forced conclusion received the uncompressed older tool result")
	}
	if requestMessages[5].Content != recentContent {
		t.Fatalf("forced conclusion lost recent evidence: %q", requestMessages[5].Content)
	}
	if !reflect.DeepEqual(requestMessages[6], contractMessage) {
		t.Fatalf("forced conclusion answer contract changed: %#v", requestMessages[6])
	}
	if state.result.SessionMessages[1].Content != oldContent {
		t.Fatal("forced conclusion compaction changed persisted tool output")
	}
}

func answerCompactionFixture(
	t *testing.T,
) (*Agent, *compiledLoop, string, string, llm.Message) {
	t.Helper()
	const requiredLiteral = "SN-required"
	oldContent := strings.Repeat("old unrelated diagnostic payload\n", 5000) + requiredLiteral
	recentContent := `{"status":"recent-target","value":"current evidence"}`
	contractMessage, ok := answerContractMessage(tool.AnswerContract{
		RequiredLiterals: []string{requiredLiteral},
	})
	if !ok {
		t.Fatal("answerContractMessage returned no contract")
	}
	oldCall := llm.ToolCall{
		ID: "call-old", Type: "function",
		Function: llm.ToolFunction{Name: "search_old", Arguments: `{"query":"old"}`},
	}
	recentCall := llm.ToolCall{
		ID: "call-recent", Type: "function",
		Function: llm.ToolFunction{Name: "search_recent", Arguments: `{"query":"recent-target"}`},
	}
	question := "What does recent-target report?"
	messages := []llm.Message{
		{Role: "system", Content: "system policy"},
		{Role: "user", Content: question},
		{Role: "assistant", Content: "I will inspect older evidence first.", ToolCalls: []llm.ToolCall{oldCall}},
		toolMessage(oldCall.ID, oldCall.Function.Name, oldContent),
		{Role: "assistant", Content: "I will verify the latest evidence.", ToolCalls: []llm.ToolCall{recentCall}},
		toolMessage(recentCall.ID, recentCall.Function.Name, recentContent),
		contractMessage,
	}
	reserve := 512
	projected := estimateMessagesTokens(messages) + reserve
	window := projected * 100 / answerContextHighWaterPercent
	contract := &exactAnswerContract{}
	contract.Add(tool.AnswerContract{RequiredLiterals: []string{requiredLiteral}})
	sessionMessages := append([]llm.Message(nil), messages[2:6]...)
	agent := &Agent{
		observer: NoopObserver(),
		cfg: AgentConfig{
			AnswerMaxTokens:     reserve,
			ConclusionMaxTokens: reserve,
			ContextWindow:       window,
		},
	}
	state := &compiledLoop{
		ctx:                 t.Context(),
		runCtx:              t.Context(),
		runID:               "run-answer-compaction",
		input:               Input{Question: question},
		runStarted:          time.Now(),
		answerContract:      contract,
		messages:            messages,
		initialMessageCount: 2,
		answerToolSources:   make(map[int]string),
		result:              &RunResult{SessionMessages: sessionMessages},
	}
	return agent, state, oldContent, recentContent, contractMessage
}

type answerCompactionObserver struct {
	phases       []string
	contextUsage []agentrun.ContextUsageEvent
}

func (*answerCompactionObserver) OnStep(
	context.Context,
	string,
	StepRecord,
) error {
	return nil
}

func (*answerCompactionObserver) OnToken(context.Context, string, string) {}

func (*answerCompactionObserver) OnReasoning(context.Context, string, string) {}

func (observer *answerCompactionObserver) EmitPhase(_ string, text string) {
	observer.phases = append(observer.phases, text)
}

func (observer *answerCompactionObserver) OnContextUsage(
	_ context.Context,
	_ string,
	event agentrun.ContextUsageEvent,
) {
	observer.contextUsage = append(observer.contextUsage, event)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
