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
	if live.Trace == nil || live.Trace.Sequence != 3 {
		t.Fatalf("live event = %#v", live)
	}
}

func TestStreamAgentEventsDrainsTraceBeforeTerminal(t *testing.T) {
	hubEvents := make(chan agent.SSEEvent, 2)
	trace := domain.EvaluationTrace{Node: "retrieval_assemble", Status: "completed"}
	hubEvents <- agent.SSEEvent{Trace: &trace}
	hubEvents <- agent.SSEEvent{Terminal: &agent.RunTerminal{Status: agent.RunStatusDone}}
	var names []string
	handler := &Handler{}
	_, terminal := handler.streamAgentEvents(&agent.AskResult{RunID: "run"}, hubEvents, "", func(name, _ string) {
		names = append(names, name)
	}, httptest.NewRequest("GET", "/", nil))
	if terminal == nil || terminal.Status != agent.RunStatusDone || len(names) != 3 || names[0] != "trace" || names[1] != "run_end" || names[2] != "done" {
		t.Fatalf("terminal=%+v events=%v", terminal, names)
	}
}

func TestEmitHubEventWritesLLMTimingSSE(t *testing.T) {
	var eventName, data string
	timing := llm.CallLifecycle{CallSeq: 2, Phase: llm.PhaseAgentStep, Status: llm.CallLifecycleFinished, DurationMs: 1200}
	emitHubEvent("", agent.SSEEvent{LLMCall: &timing}, "run", func(name, value string) {
		eventName, data = name, value
	})
	if eventName != "llm_timing" || !strings.Contains(data, `"call_seq":2`) {
		t.Fatalf("event=%q data=%q", eventName, data)
	}
}

func TestEmitHubEventWritesToolDurationSSE(t *testing.T) {
	var eventName, data string
	step := agent.StepRecord{StepNo: 4, Kind: agent.StepKindToolResult, Tool: "observe_logs", DurationMs: 1543}
	emitHubEvent("", agent.SSEEvent{Step: &step}, "run", func(name, value string) {
		eventName, data = name, value
	})
	if eventName != "tool_result" || !strings.Contains(data, `"duration_ms":1543`) {
		t.Fatalf("event=%q data=%q", eventName, data)
	}
}

func TestEmitHubEventWritesTraceSSE(t *testing.T) {
	var eventName, data string
	event := domain.EvaluationTrace{Sequence: 1, Node: "vector_search", Status: "completed"}
	emitHubEvent("", agent.SSEEvent{Trace: &event}, "run", func(name, value string) {
		eventName, data = name, value
	})
	if eventName != "trace" || data == "" {
		t.Fatalf("event=%q data=%q", eventName, data)
	}
}

func TestEmitHubEventNonSuccessDoesNotEmitDone(t *testing.T) {
	for _, terminal := range []agent.RunTerminal{
		{Status: agent.RunStatusFailed, Error: "provider failed"},
		{Status: agent.RunStatusAborted},
	} {
		var names []string
		emitHubEvent("partial", agent.SSEEvent{Terminal: &terminal}, "run", func(name, _ string) {
			names = append(names, name)
		})
		if len(names) != 2 || names[0] != "run_end" || names[1] != "error" {
			t.Fatalf("status=%s events=%v, want run_end,error", terminal.Status, names)
		}
	}
}
