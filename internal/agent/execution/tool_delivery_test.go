package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestPrepareToolDeliveryRejectsMissingRequiredLiteral(t *testing.T) {
	const literal = "SN-required-verbatim"
	agent := &Agent{}
	call := llm.ToolCall{ID: "call-1", Function: llm.ToolFunction{Name: "lookup", Arguments: `{}`}}
	execution := ToolExecution{
		AuthoritativeContent: `{"sn":"` + literal + `"}`,
		PromptContent:        `{"sn":"abbreviated"}`,
		Evidence:             true,
		AnswerContract:       tool.AnswerContract{RequiredLiterals: []string{literal}},
	}

	got := agent.prepareDelivery("run-1", nil, nil, nil, call, nil, execution)
	if !got.Failed || got.DeliveryError != "answer_contract_missing_from_prompt" || got.Evidence {
		t.Fatalf("delivery = %+v", got)
	}
	if got.AuthoritativeContent != execution.AuthoritativeContent {
		t.Fatalf("authoritative content changed: %q", got.AuthoritativeContent)
	}
	var failure toolDeliveryError
	if err := json.Unmarshal([]byte(got.PromptContent), &failure); err != nil {
		t.Fatalf("decode delivery failure: %v", err)
	}
	if failure.Error != got.DeliveryError || failure.MissingLiterals != 1 || failure.Tool != "lookup" {
		t.Fatalf("failure = %+v", failure)
	}
	step := newToolResultStep("run-1", 2, call, got)
	if step.Content != execution.AuthoritativeContent || step.PromptContent != got.PromptContent {
		t.Fatalf("trace step = %+v", step)
	}
	if step.AuthoritativeSHA256 == step.PromptSHA256 {
		t.Fatalf("lossy delivery failure recorded equal hashes: %+v", step)
	}
}

func TestPrepareToolDeliveryPersistsOversizedResultAsArtifactReference(t *testing.T) {
	const literal = "SN-kept-in-authoritative-result"
	agent := &Agent{cfg: Config{
		ContextWindow:       1400,
		AnswerMaxTokens:     100,
		ConclusionMaxTokens: 100,
	}}
	call := llm.ToolCall{ID: "call-large", Function: llm.ToolFunction{Name: "lookup", Arguments: `{}`}}
	content := strings.Repeat("large-payload-", 600) + literal
	execution := ToolExecution{
		AuthoritativeContent: content,
		PromptContent:        content,
		Evidence:             true,
		AnswerContract:       tool.AnswerContract{RequiredLiterals: []string{literal}},
	}

	got := agent.prepareDelivery(
		"run-large",
		[]llm.Message{{Role: "system", Content: "system"}, {Role: "user", Content: "list all"}},
		nil,
		nil,
		call,
		nil,
		execution,
	)
	if !got.Failed || got.DeliveryError != "tool_result_exceeds_context_budget" || got.ArtifactID == "" || got.Evidence {
		t.Fatalf("delivery = %+v", got)
	}
	if got.AuthoritativeContent != content {
		t.Fatalf("authoritative content size = %d, want %d", len(got.AuthoritativeContent), len(content))
	}
	var failure toolDeliveryError
	if err := json.Unmarshal([]byte(got.PromptContent), &failure); err != nil {
		t.Fatalf("decode delivery failure: %v", err)
	}
	if failure.ArtifactID != got.ArtifactID || failure.RequiredTokens <= failure.AvailableTokens {
		t.Fatalf("failure = %+v", failure)
	}
	step := newToolResultStep("run-large", 2, call, got)
	if step.ArtifactID != got.ArtifactID || step.Content != content || step.AuthoritativeSHA256 == step.PromptSHA256 {
		t.Fatalf("trace step = %+v", step)
	}

	contract := &exactAnswerContract{}
	if !got.Failed {
		contract.Add(got.AnswerContract)
	}
	if contract.Active() {
		t.Fatal("failed tool delivery activated the final answer contract")
	}
}

func TestToolDeliveryBudgetClampsNegativeAvailability(t *testing.T) {
	agent := &Agent{cfg: Config{
		ContextWindow:       1000,
		AnswerMaxTokens:     200,
		ConclusionMaxTokens: 200,
	}}
	available, required, err := agent.deliveryBudget(
		[]llm.Message{{Role: "system", Content: strings.Repeat("context ", 100)}},
		nil,
		nil,
		llm.ToolCall{ID: "call", Function: llm.ToolFunction{Name: "lookup"}},
		nil,
		ToolExecution{PromptContent: "result"},
	)
	if err != nil {
		t.Fatalf("toolDeliveryBudget: %v", err)
	}
	if available != 0 || required <= 0 {
		t.Fatalf("available=%d required=%d", available, required)
	}
}

func TestToolDeliveryBudgetIncludesNoticesAndAccumulatedAnswerContract(t *testing.T) {
	const (
		previousLiteral = "SN-previous"
		currentLiteral  = "SN-current"
	)
	previousContract := &exactAnswerContract{}
	previousContract.Add(tool.AnswerContract{RequiredLiterals: []string{previousLiteral}})
	previousMessage, ok := contractMessage(tool.AnswerContract{
		RequiredLiterals: []string{previousLiteral},
	})
	if !ok {
		t.Fatal("answerContractMessage returned no previous contract")
	}
	messages := []llm.Message{
		{Role: "system", Content: "system policy"},
		{Role: "user", Content: "find the current device"},
		previousMessage,
	}
	call := llm.ToolCall{
		ID:       "call-current",
		Function: llm.ToolFunction{Name: "lookup", Arguments: `{"id":"current"}`},
	}
	execution := ToolExecution{
		PromptContent: "current result payload",
		Notices: []string{
			"the result is paginated",
			"the answer must preserve exact identifiers",
		},
		AnswerContract: tool.AnswerContract{RequiredLiterals: []string{currentLiteral}},
	}
	agent := &Agent{cfg: Config{
		ContextWindow:       20_000,
		AnswerMaxTokens:     500,
		ConclusionMaxTokens: 700,
	}}

	available, required, err := agent.deliveryBudget(
		messages,
		nil,
		nil,
		call,
		previousContract,
		execution,
	)
	if err != nil {
		t.Fatalf("toolDeliveryBudget: %v", err)
	}
	currentInput, err := estimateInputTokens(messages, nil)
	if err != nil {
		t.Fatalf("estimate current input: %v", err)
	}
	candidateInput, err := estimateInputTokens(
		deliveryMessages(messages, nil, call, previousContract, execution),
		nil,
	)
	if err != nil {
		t.Fatalf("estimate candidate input: %v", err)
	}
	if required != candidateInput-currentInput {
		t.Fatalf("required=%d, want complete candidate delta %d", required, candidateInput-currentInput)
	}
	wantAvailable := max(0, agent.cfg.ContextWindow-currentInput-agent.outputReserve()-contextSafetyTokens(agent.cfg.ContextWindow))
	if available != wantAvailable {
		t.Fatalf("available=%d, want %d", available, wantAvailable)
	}

	withoutNotices := execution
	withoutNotices.Notices = nil
	withoutNoticeInput, err := estimateInputTokens(
		deliveryMessages(messages, nil, call, previousContract, withoutNotices),
		nil,
	)
	if err != nil {
		t.Fatalf("estimate candidate without notices: %v", err)
	}
	if required <= withoutNoticeInput-currentInput {
		t.Fatalf("required=%d did not include notice overhead beyond %d", required, withoutNoticeInput-currentInput)
	}

	combined := deliveryMessages(messages, nil, call, previousContract, execution)
	var contractCount int
	for _, message := range combined {
		if message.Role == "system" && strings.HasPrefix(message.Content, exactAnswerContractPrefix) {
			contractCount++
			if !strings.Contains(message.Content, previousLiteral) ||
				!strings.Contains(message.Content, currentLiteral) {
				t.Fatalf("combined contract lost a literal: %q", message.Content)
			}
		}
	}
	if contractCount != 1 {
		t.Fatalf("combined contract messages=%d, want 1", contractCount)
	}
}

func TestToolDeliveryBudgetIncludesDeferredPostlude(t *testing.T) {
	agent := &Agent{cfg: Config{
		ContextWindow:       20_000,
		AnswerMaxTokens:     500,
		ConclusionMaxTokens: 700,
	}}
	messages := []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call-first", Function: llm.ToolFunction{Name: "first_read"}},
			{ID: "call-second", Function: llm.ToolFunction{Name: "second_read"}},
		}},
		toolMessage("call-first", "first_read", "first result"),
	}
	pendingNotices := []string{strings.Repeat("deferred notice ", 20)}
	call := llm.ToolCall{ID: "call-second", Function: llm.ToolFunction{Name: "second_read"}}
	execution := ToolExecution{PromptContent: "second result"}

	available, required, err := agent.deliveryBudget(messages, pendingNotices, nil, call, nil, execution)
	if err != nil {
		t.Fatalf("toolDeliveryBudget: %v", err)
	}
	current := deliveryContextMessages(messages, pendingNotices, nil)
	currentInput, err := estimateInputTokens(current, nil)
	if err != nil {
		t.Fatalf("estimate current input: %v", err)
	}
	candidateInput, err := estimateInputTokens(
		deliveryMessages(messages, pendingNotices, call, nil, execution),
		nil,
	)
	if err != nil {
		t.Fatalf("estimate candidate input: %v", err)
	}
	if required != candidateInput-currentInput {
		t.Fatalf("required=%d, want deferred-postlude delta %d", required, candidateInput-currentInput)
	}
	wantAvailable := max(0, agent.cfg.ContextWindow-currentInput-agent.outputReserve()-contextSafetyTokens(agent.cfg.ContextWindow))
	if available != wantAvailable {
		t.Fatalf("available=%d, want %d", available, wantAvailable)
	}
}

func TestRunKeepsParallelToolResultsContiguousBeforePostlude(t *testing.T) {
	const serial = "SN-parallel-required"
	var calls int32
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
		if atomic.AddInt32(&calls, 1) == 1 {
			writeTestSSE(t, w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-exact","type":"function","function":{"name":"exact_read","arguments":"{}"}},{"index":1,"id":"call-other","type":"function","function":{"name":"other_read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		assertToolResultsPrecedePostlude(t, request.Messages, []string{"call-exact", "call-other"}, "", serial)
		encoded, _ := json.Marshal(streamChunkJS{Choices: []streamChoiceJS{{
			Delta: streamDeltaJS{Content: "设备 SN：" + serial}, FinishReason: "stop",
		}}})
		writeTestSSE(t, w, string(encoded))
	}))
	defer server.Close()

	registry := testRegistry(t,
		Tool{
			ID: "exact_read", Description: "return an exact identifier", Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{}, nil),
			Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
				return tool.Result{
					Content:        serial,
					AnswerContract: tool.AnswerContract{RequiredLiterals: []string{serial}},
				}, nil
			}),
		},
		Tool{
			ID: "other_read", Description: "return supporting evidence", Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{}, nil),
			Handler: stringHandler(func(context.Context, tool.Arguments) (string, error) {
				return "supporting evidence", nil
			}),
		},
	)
	agent := NewAgent(
		llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, server.Client()),
		NewToolExecutor(registry),
		Config{MaxSteps: 2, AnswerMaxTokens: 100, MaxContinueRounds: 0, Timeout: 5 * time.Second, AnswerReserve: time.Second},
		&captureObserver{},
		nil,
	)
	result, err := agent.RunWithPlan(
		t.Context(), "run-parallel-postlude", "find the exact device", nil, nil,
		domain.EvidencePlan{Sources: domain.Internal}, false,
	)
	if err != nil {
		t.Fatalf("RunWithPlan: %v", err)
	}
	if result.Err != nil || result.Answer != "设备 SN："+serial {
		t.Fatalf("result = %+v", result)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("LLM calls = %d, want 2", calls)
	}
}

func assertToolResultsPrecedePostlude(t *testing.T, messages []llm.Message, ids []string, notice, literal string) {
	t.Helper()
	assistantIndex := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" && len(messages[i].ToolCalls) == len(ids) {
			assistantIndex = i
			break
		}
	}
	if assistantIndex < 0 || assistantIndex+len(ids) >= len(messages) {
		t.Fatalf("parallel assistant/tool group missing: %#v", messages)
	}
	for offset, id := range ids {
		message := messages[assistantIndex+1+offset]
		if message.Role != "tool" || message.ToolCallID != id {
			t.Fatalf("message after parallel call %d = %#v, want tool result %q", offset, message, id)
		}
	}
	postlude := messages[assistantIndex+1+len(ids):]
	noticeSeen := notice == ""
	var contractSeen bool
	for _, message := range postlude {
		noticeSeen = noticeSeen || message.Role == "system" && strings.Contains(message.Content, notice)
		contractSeen = contractSeen || message.Role == "system" &&
			strings.HasPrefix(message.Content, exactAnswerContractPrefix) && strings.Contains(message.Content, literal)
	}
	if !noticeSeen || !contractSeen {
		t.Fatalf("postlude notice=%v contract=%v messages=%#v", noticeSeen, contractSeen, postlude)
	}
}

func TestAppendToolTurnPostludeKeepsNoticeAfterCompleteToolGroup(t *testing.T) {
	const (
		notice  = "preserve this tool result"
		literal = "SN-postlude-required"
	)
	calls := []llm.ToolCall{
		{ID: "call-a", Function: llm.ToolFunction{Name: "read_a"}},
		{ID: "call-b", Function: llm.ToolFunction{Name: "read_b"}},
	}
	messages := []llm.Message{
		{Role: "assistant", ToolCalls: calls},
		toolMessage("call-a", "read_a", literal),
		toolMessage("call-b", "read_b", "supporting evidence"),
	}
	contract := &exactAnswerContract{}
	contract.Add(tool.AnswerContract{RequiredLiterals: []string{literal}})

	got := appendToolTurnPostlude(messages, []string{notice}, contract)
	assertToolResultsPrecedePostlude(t, got, []string{"call-a", "call-b"}, notice, literal)
}

func TestRunPreservesFiftyNineRequiredLiteralsAcrossModelSessionAndReplay(t *testing.T) {
	serials := make([]string, 59)
	rows := make([]map[string]string, 59)
	for i := range serials {
		serials[i] = fmt.Sprintf("SN-%02d-完整序列号-%08d", i+1, 10000000+i)
		rows[i] = map[string]string{"sn": serials[i], "model": "device"}
	}
	payloadBytes, err := json.Marshal(map[string]any{
		"items":       rows,
		"total":       len(rows),
		"has_more":    false,
		"next_cursor": "cursor-after-59",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	payload := string(payloadBytes)
	answer := strings.Join(serials, "\n")

	var calls int32
	var modelToolContent string
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
		if atomic.AddInt32(&calls, 1) == 1 {
			writeTestSSE(t, w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-59","type":"function","function":{"name":"lookup_many","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		for _, message := range request.Messages {
			if message.Role == "tool" && message.ToolCallID == "call-59" {
				modelToolContent = message.Content
			}
		}
		encoded, _ := json.Marshal(streamChunkJS{Choices: []streamChoiceJS{{Delta: streamDeltaJS{Content: answer}, FinishReason: "stop"}}})
		writeTestSSE(t, w, string(encoded))
	}))
	defer server.Close()

	registry := testRegistry(t, Tool{
		ID:          "lookup_many",
		Description: "return a bounded device page",
		Kind:        ToolKindRead,
		InputSchema: objectSchema(map[string]any{}, nil),
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{
				Content:        payload,
				AnswerContract: tool.AnswerContract{RequiredLiterals: serials},
			}, nil
		}),
	})
	agent := NewAgent(
		llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{}),
		NewToolExecutor(registry),
		Config{MaxSteps: 2, AnswerMaxTokens: 100, MaxContinueRounds: 0, Timeout: 5 * time.Second, AnswerReserve: time.Second},
		&captureObserver{},
		nil,
	)

	result, err := agent.RunWithPlan(
		t.Context(), "run-59", "列出全部 59 个完整 SN", nil, nil,
		domain.EvidencePlan{Sources: domain.Internal}, false,
	)
	if err != nil {
		t.Fatalf("RunWithPlan: %v", err)
	}
	if result.Err != nil || result.Answer != answer {
		t.Fatalf("result = %+v", result)
	}
	assertContainsEveryLiteral(t, "model tool content", modelToolContent, serials)
	assertContainsEveryLiteral(t, "final answer", result.Answer, serials)
	if !strings.Contains(modelToolContent, `"next_cursor":"cursor-after-59"`) {
		t.Fatalf("model tool content lost JSON tail: %s", modelToolContent)
	}

	var sessionToolContent string
	for _, message := range result.SessionMessages {
		if message.Role == "tool" && message.ToolCallID == "call-59" {
			sessionToolContent = message.Content
			break
		}
	}
	if sessionToolContent != payload {
		t.Fatalf("session tool content changed: got %d bytes, want %d", len(sessionToolContent), len(payload))
	}

	recent := []llm.Message{{Role: "user", Content: "列出全部 59 个完整 SN"}}
	recent = append(recent, result.SessionMessages...)
	recent = append(recent, llm.Message{Role: "assistant", Content: result.Answer})
	nextMessages := agent.buildMessages(
		"上一轮第 59 个 SN 是什么？",
		domain.QueryPlan{Kind: domain.QueryFocusedFact},
		ConversationContext{Recent: recent},
		nil,
		domain.EvidencePlan{Sources: domain.Internal},
	)
	var replayedToolContent string
	for _, message := range nextMessages {
		if message.Role == "tool" && message.ToolCallID == "call-59" {
			replayedToolContent = message.Content
			break
		}
	}
	if replayedToolContent != payload {
		t.Fatalf("next-round replay changed tool content: got %d bytes, want %d", len(replayedToolContent), len(payload))
	}
	assertContainsEveryLiteral(t, "next-round replay", replayedToolContent, serials)
}

func assertContainsEveryLiteral(t *testing.T, label, content string, literals []string) {
	t.Helper()
	for i, literal := range literals {
		if !strings.Contains(content, literal) {
			t.Fatalf("%s missing literal %d: %q", label, i, literal)
		}
	}
}
