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

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

type fakeDelegationAwaiter struct {
	dispatch agentapi.DelegationDispatchResult
	calls    int32
}

func (f *fakeDelegationAwaiter) AwaitSettlement(
	context.Context,
	string,
	time.Time,
) (agentapi.DelegationDispatchResult, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.dispatch, nil
}

func completedDispatch(delegationID string) agentapi.DelegationDispatchResult {
	return agentapi.DelegationDispatchResult{
		DelegationID: delegationID,
		Status:       agentapi.DelegationCompleted,
		Tasks: []agentapi.DelegationTaskStatus{
			{
				TaskID:  "child-1",
				Subject: "rgb 灯效",
				Status:  agentapi.DelegationCompleted,
				Report: &agentapi.DelegationReport{
					ReportID:   "report-1",
					Capability: "knowledge.service.trace",
					Status:     agentapi.DelegationCompleted,
					Summary:    "rgb 灯效流程已完成",
					Flow: &agentapi.FlowIR{
						Nodes: []agentapi.FlowNode{{ID: "n1", Label: "入口"}},
					},
				},
			},
		},
	}
}

// TestAwaitDelegationBackfillsReportsAndNotices asserts the mid-loop await (②):
// after delegate_investigation returns, the parent waits on the server side,
// backfills the report into the answer contract, appends a system notice, and
// never asks the model to poll delegation_status.
func TestAwaitDelegationBackfillsReportsAndNotices(t *testing.T) {
	dispatch := completedDispatch("del-1")
	awaiter := &fakeDelegationAwaiter{dispatch: dispatch}

	var modelCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		drainRequestBody(r)
		call := atomic.AddInt32(&modelCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			writeTestSSE(t, w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"delegate_investigation","arguments":"{\"tasks\":[{\"capability\":\"knowledge.service.trace\",\"objective\":\"rgb 灯效\"}]}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		writeTestSSE(t, w, `{"choices":[{"delta":{"content":"已根据报告完成 synthesis。\n[NASUTA_DELEGATION_ADOPTION] {\"delegations\":[{\"delegation_id\":\"del-1\",\"adopted_report_ids\":[\"report-1\"]}]}"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	registry := testRegistry(t, Tool{
		ID: "delegate_investigation", Description: "delegate", Kind: ToolKindRead,
		InputSchema: objectSchema(nil, nil),
		Handler: stringHandler(func(context.Context, tool.Arguments) (string, error) {
			raw, _ := json.Marshal(agentapi.DelegationDispatchResult{
				DelegationID: "del-1",
				Status:       agentapi.DelegationRunning,
				Tasks:        []agentapi.DelegationTaskStatus{{TaskID: "child-1", Status: agentapi.DelegationRunning}},
			})
			return string(raw), nil
		}),
	})
	client := llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{})
	agent := NewAgent(client, NewToolExecutor(registry), Config{
		MaxSteps: 4, AnswerMaxTokens: 100,
		Timeout: 5 * time.Second, AnswerReserve: time.Second,
		DelegationAwaiter: awaiter,
	}, nil, nil)

	result, err := agent.RunCompiled(t.Context(), "run_await", Input{
		Question: "分析 rgb 灯效流程",
		Messages: []llm.Message{{Role: "user", Content: "分析 rgb 灯效流程"}},
	}, agent.executor.Snapshot(ToolPolicyForRun(false)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Err != nil {
		t.Fatalf("result.Err = %v", result.Err)
	}
	if atomic.LoadInt32(&awaiter.calls) != 1 {
		t.Fatalf("awaiter calls = %d, want 1", atomic.LoadInt32(&awaiter.calls))
	}
	if got := atomic.LoadInt32(&modelCalls); got != 2 {
		t.Fatalf("model calls = %d, want 2 (no delegation_status polling)", got)
	}
	// The backfilled report must be adoptable through the answer contract.
	if len(result.DelegationAdoptions) != 1 {
		t.Fatalf("delegation adoptions = %#v, want 1 delegation", result.DelegationAdoptions)
	}
	// A settlement notice must be present in session messages.
	foundNotice := false
	for _, msg := range result.SessionMessages {
		if msg.Role == "system" && containsAll(msg.Content, "del-1", "rgb 灯效流程已完成") {
			foundNotice = true
		}
	}
	if !foundNotice {
		t.Fatalf("settled notice missing from session messages: %#v", result.SessionMessages)
	}
}

// TestFinishLoopAwaitsUnsettledDelegations asserts the backstop (③): a
// delegation dispatched but not awaited mid-loop is still awaited at finish
// time, so the reservation has closed before the run lease is released.
func TestFinishLoopAwaitsUnsettledDelegations(t *testing.T) {
	awaiter := &fakeDelegationAwaiter{dispatch: completedDispatch("del-2")}

	var modelCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&modelCalls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			// The model answers directly without ever awaiting; this is the
			// degenerate case the finish-loop backstop must cover.
			writeTestSSE(t, w, `{"choices":[{"delta":{"content":"直接回答，未等待子任务。"},"finish_reason":"stop"}]}`)
			return
		}
		writeTestSSE(t, w, `{"choices":[{"delta":{"content":"兜底。"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	registry := testRegistry(t, Tool{
		ID: "delegate_investigation", Description: "delegate", Kind: ToolKindRead,
		InputSchema: objectSchema(nil, nil),
		Handler: stringHandler(func(context.Context, tool.Arguments) (string, error) {
			return "", fmt.Errorf("not expected to run in this test")
		}),
	})
	client := llm.NewLLMClientWithHTTP(server.URL, "k", "test", 100, &http.Client{})
	agent := NewAgent(client, NewToolExecutor(registry), Config{
		MaxSteps: 4, AnswerMaxTokens: 100,
		Timeout: 5 * time.Second, AnswerReserve: time.Second,
		DelegationAwaiter: awaiter,
	}, nil, nil)

	// Directly exercise finishLoop against a prepared loop state whose
	// dispatchedDelegations already contains an unsettled delegation.
	state := agent.prepareLoop(
		t.Context(), t.Context(), t.Context(), "run_finish_await",
		Input{Question: "q", Messages: []llm.Message{{Role: "user", Content: "q"}}},
		agent.executor.Snapshot(ToolPolicyForRun(false)),
		time.Now(),
	)
	state.dispatchedDelegations = []string{"del-2"}

	agent.finishLoop(state)

	if atomic.LoadInt32(&awaiter.calls) != 1 {
		t.Fatalf("awaiter calls = %d, want 1", atomic.LoadInt32(&awaiter.calls))
	}
	if !state.settledDelegations["del-2"] {
		t.Fatalf("delegation del-2 not marked settled: %#v", state.settledDelegations)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}
