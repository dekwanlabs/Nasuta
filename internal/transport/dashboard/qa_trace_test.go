package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tracecontract"
)

type failingSSEWriter struct {
	header   http.Header
	writeErr error
	flushErr error
}

func (writer *failingSSEWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (writer *failingSSEWriter) Write([]byte) (int, error) {
	if writer.writeErr != nil {
		return 0, writer.writeErr
	}
	return 1, nil
}

func (*failingSSEWriter) WriteHeader(int) {}
func (*failingSSEWriter) Flush()          {}
func (writer *failingSSEWriter) FlushError() error {
	return writer.flushErr
}

func TestQATraceExporterStreamsScopeEvents(t *testing.T) {
	hub := agent.NewRunHub(nil)
	channel := hub.Subscribe("run")
	scope := executiontrace.Begin(executiontrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		hub.EmitTrace("run", event)
	}))
	ctx := executiontrace.WithScope(t.Context(), scope)
	domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "evidence_plan"})
	domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "agent_model_turn"})
	for sequence := 1; sequence <= 2; sequence++ {
		live := <-channel
		trace, ok := live.Data.(domain.EvaluationTrace)
		if live.Type != agent.EventTrace || !ok || trace.Sequence != sequence {
			t.Fatalf("live event = %#v", live)
		}
	}
}

func TestQATraceUsesV1WireContract(t *testing.T) {
	event := tracecontract.EventV1{
		TraceID: "trace-1", RunID: "agent-1", ParentRunID: "workflow-1",
		WorkflowRunID: "workflow-1", AgentRunID: "agent-1", WorkflowNodeID: "review.code",
		Node: "evidence_plan", Status: "completed",
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded tracecontract.EventV1
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, event) {
		t.Fatalf("trace contract = %#v", decoded)
	}
}

func TestStreamAgentEventsDrainsTraceBeforeTerminal(t *testing.T) {
	hubEvents := make(chan agent.SSEEvent, 2)
	hubEvents <- agent.SSEEvent{Type: agent.EventTrace, Data: domain.EvaluationTrace{Node: "retrieval_assemble"}}
	terminal := &agent.RunTerminal{Status: agent.RunStatusDone, Answer: "answer"}
	hubEvents <- agent.SSEEvent{Type: agent.EventRunFinished, Data: terminal}
	var names []string
	handler := &Handler{}
	got := handler.streamAgentEvents(hubEvents, func(name string, _ any) bool {
		names = append(names, name)
		return true
	}, httptest.NewRequest("GET", "/", nil))
	if got != terminal || len(names) != 2 || names[0] != "trace" || names[1] != "run.finished" {
		t.Fatalf("terminal=%+v events=%v", got, names)
	}
}

func TestEmitHubEventForwardsTaggedEvent(t *testing.T) {
	var eventName string
	var data any
	timing := llm.CallLifecycle{CallSeq: 2, Phase: llm.PhaseAgentStep, Status: llm.CallLifecycleFinished, DurationMs: 1200}
	emitHubEvent(agent.SSEEvent{Type: agent.EventLLMCall, Data: timing}, func(name string, value any) bool {
		eventName, data = name, value
		return true
	})
	if eventName != "llm.call" || data != timing {
		t.Fatalf("event=%q data=%#v", eventName, data)
	}
}

func TestEmitHubEventUsesToolSummary(t *testing.T) {
	var eventName string
	var data any
	payload := agent.ToolFinishedEvent{Step: 4, Tool: "observe_logs", Summary: "matched logs", Failed: true, DurationMs: 1543}
	emitHubEvent(agent.SSEEvent{Type: agent.EventToolFinished, Data: payload}, func(name string, value any) bool {
		eventName, data = name, value
		return true
	})
	if eventName != "tool.finished" || data != payload {
		t.Fatalf("event=%q data=%#v", eventName, data)
	}
}

func TestEmitHubEventForwardsExecutionEvents(t *testing.T) {
	payload := agent.ExecutionEvent{
		RunID: "qa-parent", WorkflowRunID: "workflow-1", NodeID: "investigate.code",
		Strategy: "multi_agent", Status: "completed", Reason: "evidence joined",
		Complexity: 0.95, Confidence: 0.91,
	}
	tests := []struct {
		eventType agent.EventType
	}{
		{eventType: agent.EventExecutionRouted},
		{eventType: agent.EventExecutionDegraded},
		{eventType: agent.EventWorkflowStarted},
		{eventType: agent.EventAgentStarted},
		{eventType: agent.EventAgentCompleted},
		{eventType: agent.EventEvidenceJoined},
	}
	for _, test := range tests {
		t.Run(string(test.eventType), func(t *testing.T) {
			var eventName string
			var data any
			emitHubEvent(agent.SSEEvent{Type: test.eventType, Data: payload}, func(name string, value any) bool {
				eventName, data = name, value
				return true
			})
			if eventName != string(test.eventType) || data != payload {
				t.Fatalf("event=%q data=%#v", eventName, data)
			}
		})
	}
}

func TestRunFinishedIsTheOnlyTerminalEvent(t *testing.T) {
	for _, status := range []agent.RunStatus{agent.RunStatusDone, agent.RunStatusFailed, agent.RunStatusAborted} {
		var names []string
		emitHubEvent(agent.SSEEvent{Type: agent.EventRunFinished, Data: &agent.RunTerminal{Status: status}}, func(name string, _ any) bool {
			names = append(names, name)
			return true
		})
		if len(names) != 1 || names[0] != "run.finished" {
			t.Fatalf("status=%s events=%v", status, names)
		}
	}
}

func TestSSEWriterReturnsEncodingError(t *testing.T) {
	stream, err := newSSEWriter(&failingSSEWriter{})
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	err = stream.emit("trace", make(chan struct{}))
	if err == nil {
		t.Fatal("emit accepted an unsupported payload")
	}
}

func TestSSEWriterReturnsWriteAndFlushErrors(t *testing.T) {
	writeFailure := errors.New("broken pipe")
	stream, err := newSSEWriter(&failingSSEWriter{writeErr: writeFailure})
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	if err := stream.emit("status", map[string]string{"text": "working"}); !errors.Is(err, writeFailure) {
		t.Fatalf("write error = %v", err)
	}

	flushFailure := errors.New("flush failed")
	stream, err = newSSEWriter(&failingSSEWriter{flushErr: flushFailure})
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	if err := stream.emit("status", map[string]string{"text": "working"}); !errors.Is(err, flushFailure) {
		t.Fatalf("flush error = %v", err)
	}
}
