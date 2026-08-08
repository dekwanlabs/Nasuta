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

	got := agent.prepareToolDelivery("run-1", nil, nil, call, execution)
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
	agent := &Agent{cfg: AgentConfig{
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

	got := agent.prepareToolDelivery(
		"run-large",
		[]llm.Message{{Role: "system", Content: "system"}, {Role: "user", Content: "list all"}},
		nil,
		call,
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
	agent := &Agent{cfg: AgentConfig{
		ContextWindow:       1000,
		AnswerMaxTokens:     200,
		ConclusionMaxTokens: 200,
	}}
	available, required, err := agent.toolDeliveryBudget(
		[]llm.Message{{Role: "system", Content: strings.Repeat("context ", 100)}},
		nil,
		llm.ToolCall{ID: "call", Function: llm.ToolFunction{Name: "lookup"}},
		ToolExecution{PromptContent: "result"},
	)
	if err != nil {
		t.Fatalf("toolDeliveryBudget: %v", err)
	}
	if available != 0 || required <= 0 {
		t.Fatalf("available=%d required=%d", available, required)
	}
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
		AgentConfig{MaxSteps: 2, AnswerMaxTokens: 100, MaxContinueRounds: 0, Timeout: 5 * time.Second, AnswerReserve: time.Second},
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
	nextMessages := agent.buildAgentMessages(
		"上一轮第 59 个 SN 是什么？",
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
