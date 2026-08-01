package dashboard

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

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
	got := handler.streamAgentEvents(hubEvents, func(name, _ string) { names = append(names, name) }, httptest.NewRequest("GET", "/", nil))
	if got != terminal || len(names) != 2 || names[0] != "trace" || names[1] != "run.finished" {
		t.Fatalf("terminal=%+v events=%v", got, names)
	}
}

func TestEmitHubEventForwardsTaggedEvent(t *testing.T) {
	var eventName, data string
	timing := llm.CallLifecycle{CallSeq: 2, Phase: llm.PhaseAgentStep, Status: llm.CallLifecycleFinished, DurationMs: 1200}
	emitHubEvent(agent.SSEEvent{Type: agent.EventLLMCall, Data: timing}, func(name, value string) {
		eventName, data = name, value
	})
	if eventName != "llm.call" || !strings.Contains(data, `"call_seq":2`) {
		t.Fatalf("event=%q data=%q", eventName, data)
	}
}

func TestEmitHubEventUsesToolSummary(t *testing.T) {
	var eventName, data string
	payload := agent.ToolFinishedEvent{Step: 4, Tool: "observe_logs", Summary: "matched logs", Failed: true, DurationMs: 1543}
	emitHubEvent(agent.SSEEvent{Type: agent.EventToolFinished, Data: payload}, func(name, value string) {
		eventName, data = name, value
	})
	if eventName != "tool.finished" || !strings.Contains(data, `"summary":"matched logs"`) || !strings.Contains(data, `"failed":true`) {
		t.Fatalf("event=%q data=%q", eventName, data)
	}
}

func TestRunFinishedIsTheOnlyTerminalEvent(t *testing.T) {
	for _, status := range []agent.RunStatus{agent.RunStatusDone, agent.RunStatusFailed, agent.RunStatusAborted} {
		var names []string
		emitHubEvent(agent.SSEEvent{Type: agent.EventRunFinished, Data: &agent.RunTerminal{Status: status}}, func(name, _ string) {
			names = append(names, name)
		})
		if len(names) != 1 || names[0] != "run.finished" {
			t.Fatalf("status=%s events=%v", status, names)
		}
	}
}
