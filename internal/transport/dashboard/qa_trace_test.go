package dashboard

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/tracecontract"
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
	hub := run.NewHub(nil)
	channel := hub.Subscribe("run")
	scope := runtrace.Begin(runtrace.WithEvaluation(t.Context(), func(event domain.EvaluationTrace) {
		hub.EmitTrace("run", event)
	}))
	ctx := runtrace.WithScope(t.Context(), scope)
	domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "evidence_plan"})
	domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "agent_model_turn"})
	for sequence := 1; sequence <= 2; sequence++ {
		live := <-channel
		trace, ok := live.Data.(domain.EvaluationTrace)
		if live.Type != run.EventTrace || !ok || trace.Sequence != sequence {
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

// The QA stream loop forwards every hub event and stops only on the terminal
// one, so a trace event queued ahead of run.finished must still reach the client.
func TestStreamLoopDrainsTraceBeforeTerminal(t *testing.T) {
	terminal := &run.Terminal{Status: run.StatusDone, Answer: "answer"}
	events := []run.SSEEvent{
		{Type: run.EventTrace, Data: domain.EvaluationTrace{Node: "retrieval_assemble"}},
		{Type: run.EventRunFinished, Data: terminal},
	}
	var names []string
	var got *run.Terminal
	for _, ev := range events {
		if got != nil {
			t.Fatalf("kept forwarding after terminal event %q", ev.Type)
		}
		if !emitHubEvent(ev, func(name string, _ any) bool {
			names = append(names, name)
			return true
		}) {
			t.Fatalf("emit %q failed", ev.Type)
		}
		got = run.TerminalFromEvent(ev)
	}
	if got != terminal || len(names) != 2 || names[0] != "trace" || names[1] != "run.finished" {
		t.Fatalf("terminal=%+v events=%v", got, names)
	}
}

func TestEmitHubEventForwardsTaggedEvent(t *testing.T) {
	var eventName string
	var data any
	timing := llm.CallLifecycle{CallSeq: 2, Phase: llm.PhaseAgentStep, Status: llm.CallLifecycleFinished, DurationMs: 1200}
	emitHubEvent(run.SSEEvent{Type: run.EventLLMCall, Data: timing}, func(name string, value any) bool {
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
	payload := run.ToolFinishedEvent{Step: 4, Tool: "observe_logs", Summary: "matched logs", Failed: true, DurationMs: 1543}
	emitHubEvent(run.SSEEvent{Type: run.EventToolFinished, Data: payload}, func(name string, value any) bool {
		eventName, data = name, value
		return true
	})
	if eventName != "tool.finished" || data != payload {
		t.Fatalf("event=%q data=%#v", eventName, data)
	}
}

func TestEmitHubEventForwardsExecutionEvents(t *testing.T) {
	payload := run.ExecutionEvent{
		RunID: "qa-parent", ParentRunID: "qa-parent", ChildRunID: "child-1",
		WorkflowRunID: "workflow-1", NodeID: "investigate.code",
		DelegationID: "del-1", Capability: "knowledge.code.inspect",
		ObjectiveSummary: "inspect the code path", ReportID: "report-1",
		ToolCalls: 3, ReportBytes: 512, Completeness: "complete",
		CitationCoverage: 1, StructuredClaimCoverage: 0.5,
		ConflictCount: 1, RequiresVerification: true,
		VerificationReasons: []string{"critical_structured_conflict"},
		QueryKind:           "code_review", ReferenceCount: 4,
		Strategy: "multi_agent", Status: "completed", Reason: "evidence joined",
		Complexity: 0.95, Confidence: 0.91,
	}
	tests := []struct {
		eventType run.EventType
	}{
		{eventType: run.EventExecutionRouted},
		{eventType: run.EventExecutionDegraded},
		{eventType: run.EventDelegationCreated},
		{eventType: run.EventDelegationStarted},
		{eventType: run.EventDelegationDone},
		{eventType: run.EventDelegationFailed},
		{eventType: run.EventDelegationCancelled},
		{eventType: run.EventDelegationRejected},
		{eventType: run.EventDelegationValidated},
		{eventType: run.EventDelegationVerificationStarted},
		{eventType: run.EventDelegationVerificationDone},
		{eventType: run.EventDelegationVerificationFailed},
		{eventType: run.EventDelegationVerificationRejected},
		{eventType: run.EventDelegationAdoptionEvaluated},
	}
	for _, test := range tests {
		t.Run(string(test.eventType), func(t *testing.T) {
			var eventName string
			var data any
			emitHubEvent(run.SSEEvent{Type: test.eventType, Data: payload}, func(name string, value any) bool {
				eventName, data = name, value
				return true
			})
			if eventName != string(test.eventType) || !reflect.DeepEqual(data, payload) {
				t.Fatalf("event=%q data=%#v", eventName, data)
			}
		})
	}
}

func TestRunFinishedIsTheOnlyTerminalEvent(t *testing.T) {
	for _, status := range []run.Status{run.StatusDone, run.StatusFailed, run.StatusAborted} {
		var names []string
		emitHubEvent(run.SSEEvent{Type: run.EventRunFinished, Data: &run.Terminal{Status: status}}, func(name string, _ any) bool {
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
