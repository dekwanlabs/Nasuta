package dashboard

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

type stubQAApplication struct {
	start func(context.Context, QAStartRequest) (QAStartedRun, error)
}

func (s *stubQAApplication) CompactionStatus(string) agentrun.SessionStatusEvent {
	return agentrun.SessionStatusEvent{}
}

func (s *stubQAApplication) ContextUsage(string) (agentrun.ContextUsageEvent, bool) {
	return agentrun.ContextUsageEvent{}, false
}

func (s *stubQAApplication) Start(ctx context.Context, req QAStartRequest) (QAStartedRun, error) {
	if s.start == nil {
		return QAStartedRun{}, nil
	}
	return s.start(ctx, req)
}

func TestServeAgentSSEContractEventOrderAndSingleTerminal(t *testing.T) {
	events := make(chan agentrun.SSEEvent, 4)
	events <- agentrun.SSEEvent{Type: agentrun.EventStatus, Data: map[string]any{"text": "planning"}}
	events <- agentrun.SSEEvent{Type: agentrun.EventAnswerDelta, Data: map[string]any{"delta": "answer"}}
	events <- agentrun.SSEEvent{Type: agentrun.EventRunFinished, Data: agentrun.Terminal{Status: agentrun.StatusDone, Answer: "answer"}}
	close(events)

	app := &stubQAApplication{start: func(context.Context, QAStartRequest) (QAStartedRun, error) {
		return QAStartedRun{
			RunID:   "run-1",
			Context: &retrieval.RetrievedContext{References: []retrieval.Reference{{Label: "ref"}}},
			Events:  events,
			Close:   func() {},
		}, nil
	}}
	handler := &Handler{qaPortsFn: func() QAApplicationPorts {
		return QAApplicationPorts{Application: app}
	}}

	var emitted []string
	req := httptest.NewRequest("POST", "/api/qa/ask", nil)
	handler.serveAgentSSE(
		context.Background(), "question",
		execution.ConversationContext{}, "", false, nil, false,
		func(event string, payload any) error {
			emitted = append(emitted, event)
			return nil
		},
		req,
	)

	want := []string{"run.started", "context", "status", "answer.delta", "run.finished"}
	if len(emitted) != len(want) {
		t.Fatalf("emitted = %v, want %v", emitted, want)
	}
	for i := range want {
		if emitted[i] != want[i] {
			t.Fatalf("event[%d] = %q, want %q (all=%v)", i, emitted[i], want[i], emitted)
		}
	}
}

func TestServeAgentSSEApplicationUnavailableEmitsFailedTerminal(t *testing.T) {
	handler := &Handler{qaPortsFn: func() QAApplicationPorts { return QAApplicationPorts{} }}
	var emitted []string
	handler.serveAgentSSE(
		context.Background(), "question",
		execution.ConversationContext{}, "", false, nil, false,
		func(event string, payload any) error {
			emitted = append(emitted, event)
			return nil
		},
		httptest.NewRequest("POST", "/api/qa/ask", nil),
	)
	if len(emitted) != 1 || emitted[0] != "run.finished" {
		t.Fatalf("emitted = %v, want single run.finished", emitted)
	}
}

func TestServeAgentSSEStartErrorEmitsFailedTerminal(t *testing.T) {
	app := &stubQAApplication{start: func(context.Context, QAStartRequest) (QAStartedRun, error) {
		return QAStartedRun{}, context.DeadlineExceeded
	}}
	handler := &Handler{qaPortsFn: func() QAApplicationPorts { return QAApplicationPorts{Application: app} }}
	var emitted []string
	handler.serveAgentSSE(
		context.Background(), "question",
		execution.ConversationContext{}, "", false, nil, false,
		func(event string, payload any) error {
			emitted = append(emitted, event)
			return nil
		},
		httptest.NewRequest("POST", "/api/qa/ask", nil),
	)
	if len(emitted) != 1 || emitted[0] != "run.finished" {
		t.Fatalf("emitted = %v, want single run.finished", emitted)
	}
}

func TestServeAgentSSEClientDisconnectStopsConsuming(t *testing.T) {
	events := make(chan agentrun.SSEEvent)
	closed := make(chan struct{})
	app := &stubQAApplication{start: func(context.Context, QAStartRequest) (QAStartedRun, error) {
		return QAStartedRun{
			RunID:  "run-1",
			Events: events,
			Close: func() {
				close(closed)
			},
		}, nil
	}}
	handler := &Handler{qaPortsFn: func() QAApplicationPorts { return QAApplicationPorts{Application: app} }}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest("POST", "/api/qa/ask", nil).WithContext(ctx)
		handler.serveAgentSSE(
			ctx, "question", execution.ConversationContext{}, "", false, nil, false,
			func(string, any) error { return nil },
			req,
		)
	}()
	cancel()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("subscription was not closed on client disconnect")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveAgentSSE did not return after client disconnect")
	}
}
