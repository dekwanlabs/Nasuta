package dashboard

import (
	"net/http/httptest"
	"testing"
	"time"

	agentcap "github.com/dekwanlabs/astris/internal/agent"
	types "github.com/dekwanlabs/astris/internal/domain"
)

func TestQATraceRecorderSequencesEvents(t *testing.T) {
	hub := agentcap.NewRunHub(nil)
	channel := hub.Subscribe("run")
	recorder := &qaTraceRecorder{started: time.Now(), runID: "run", hub: hub}
	recorder.RecordTrace(types.EvaluationTrace{Node: "evidence_plan"})
	recorder.RecordTrace(types.EvaluationTrace{Node: "query_rewrite"})
	buffered := recorder.Activate()
	if len(buffered) != 2 || buffered[0].Sequence != 1 || buffered[1].Sequence != 2 {
		t.Fatalf("buffered = %#v", buffered)
	}
	recorder.RecordTrace(types.EvaluationTrace{Node: "agent_model_turn"})
	live := <-channel
	if live.Trace == nil || live.Trace.Sequence != 3 {
		t.Fatalf("live event = %#v", live)
	}
}

func TestStreamAgentEventsDrainsTraceBeforeTerminal(t *testing.T) {
	hubEvents := make(chan agentcap.SSEEvent, 2)
	trace := types.EvaluationTrace{Node: "retrieval_assemble", Status: "completed"}
	hubEvents <- agentcap.SSEEvent{Trace: &trace}
	hubEvents <- agentcap.SSEEvent{Terminal: &agentcap.RunTerminal{Status: agentcap.RunStatusDone}}
	var names []string
	handler := &Handler{}
	_, terminal := handler.streamAgentEvents(&agentcap.AskResult{RunID: "run"}, hubEvents, func(name, _ string) {
		names = append(names, name)
	}, httptest.NewRequest("GET", "/", nil))
	if terminal == nil || terminal.Status != agentcap.RunStatusDone || len(names) != 3 || names[0] != "trace" || names[1] != "run_end" || names[2] != "done" {
		t.Fatalf("terminal=%+v events=%v", terminal, names)
	}
}

func TestEmitHubEventWritesTraceSSE(t *testing.T) {
	var eventName, data string
	event := types.EvaluationTrace{Sequence: 1, Node: "vector_search", Status: "completed"}
	emitHubEvent("", agentcap.SSEEvent{Trace: &event}, "run", func(name, value string) {
		eventName, data = name, value
	})
	if eventName != "trace" || data == "" {
		t.Fatalf("event=%q data=%q", eventName, data)
	}
}

func TestEmitHubEventNonSuccessDoesNotEmitDone(t *testing.T) {
	for _, terminal := range []agentcap.RunTerminal{
		{Status: agentcap.RunStatusFailed, Error: "provider failed"},
		{Status: agentcap.RunStatusAborted},
	} {
		var names []string
		emitHubEvent("partial", agentcap.SSEEvent{Terminal: &terminal}, "run", func(name, _ string) {
			names = append(names, name)
		})
		if len(names) != 2 || names[0] != "run_end" || names[1] != "error" {
			t.Fatalf("status=%s events=%v, want run_end,error", terminal.Status, names)
		}
	}
}
