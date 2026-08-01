package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
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

func TestQATraceRecorderSequencesEvents(t *testing.T) {
	hub := agent.NewRunHub(nil)
	channel := hub.Subscribe("run")
	recorder := &qaTraceRecorder{started: time.Now(), runID: "run", hub: hub}
	recorder.RecordTrace(domain.EvaluationTrace{Node: "evidence_plan"})
	recorder.RecordTrace(domain.EvaluationTrace{Node: "query_rewrite"})
	buffered := recorder.Activate()
	if len(buffered) != 2 || buffered[0].Sequence != 1 || buffered[1].Sequence != 2 {
		t.Fatalf("buffered = %#v", buffered)
	}
	recorder.RecordTrace(domain.EvaluationTrace{Node: "agent_model_turn"})
	live := <-channel
	trace, ok := live.Data.(domain.EvaluationTrace)
	if live.Type != agent.EventTrace || !ok || trace.Sequence != 3 {
		t.Fatalf("live event = %#v", live)
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
