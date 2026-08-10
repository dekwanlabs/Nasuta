package run

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

func drainHub(ch chan SSEEvent, timeout time.Duration) []SSEEvent {
	var got []SSEEvent
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
			if ev.Type == EventRunFinished {
				return got
			}
		case <-deadline:
			return got
		}
	}
}

func TestHub_BroadcastsEvaluationTrace(t *testing.T) {
	hub := NewRunHub(nil)
	ch := hub.Subscribe("trace-run")
	hub.EmitTrace("trace-run", domain.EvaluationTrace{Sequence: 1, Node: "evidence_plan", Status: "completed"})
	select {
	case event := <-ch:
		trace, ok := event.Data.(domain.EvaluationTrace)
		if event.Type != EventTrace || !ok || trace.Node != "evidence_plan" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("trace event was not broadcast")
	}
}

func TestHub_BroadcastDeliversToSubscriber(t *testing.T) {
	t.Parallel()
	h := NewRunHub(nil)
	defer h.Complete("r1", RunOutcome{Status: RunStatusDone})

	ch := h.Subscribe("r1")
	h.OnToken(context.Background(), "r1", "hello")
	h.OnStep(context.Background(), "r1", StepRecord{
		StepNo: 1, Kind: StepKindToolCall,
		ToolCallID: "call-probe-1", Tool: "probe",
	})

	got := drainHub(ch, time.Second)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 events, got %d (%+v)", len(got), got)
	}
	text, ok := got[0].Data.(TextEvent)
	if got[0].Type != EventAnswerDelta || !ok || text.Text != "hello" {
		t.Errorf("first event = %+v, want answer delta", got[0])
	}
	tool, ok := got[1].Data.(ToolStartedEvent)
	if got[1].Type != EventToolStarted || !ok ||
		tool.ToolCallID != "call-probe-1" || tool.Name != "probe" {
		t.Errorf("second event = %+v, want tool=probe", got[1])
	}
}

func TestHub_BroadcastsStructuredStatus(t *testing.T) {
	hub := NewRunHub(nil)
	ch := hub.Subscribe("status-run")
	hub.EmitStatus("status-run", "正在检索", "retrieval.discover", 820)
	select {
	case event := <-ch:
		status, ok := event.Data.(TextEvent)
		if event.Type != EventStatus || !ok {
			t.Fatalf("event = %#v", event)
		}
		if status.Text != "正在检索" || status.Code != "retrieval.discover" || status.ElapsedMS != 820 {
			t.Fatalf("status = %#v", status)
		}
	case <-time.After(time.Second):
		t.Fatal("structured status event was not broadcast")
	}
}

func TestHub_UnsubscribeVsBroadcast_NoPanic(t *testing.T) {
	t.Parallel()
	h := NewRunHub(nil)
	defer h.Complete("r1", RunOutcome{Status: RunStatusDone})

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			h.OnToken(context.Background(), "r1", "t")
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			ch := h.Subscribe("r1")
			select {
			case <-ch:
			default:
			}
			h.Unsubscribe("r1", ch)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestHub_PauseThenFinishUnblocksWaitResume(t *testing.T) {
	t.Parallel()
	h := NewRunHub(nil)

	const runID = "r1"
	h.Send(runID, ControlSignal{Kind: CtrlPause})
	sig := h.Poll(runID)
	if sig.Kind != CtrlPause {
		t.Fatalf("Poll = %v, want CtrlPause", sig.Kind)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	released := make(chan error, 1)
	go func() {
		released <- h.WaitResume(waitCtx, runID)
	}()

	time.Sleep(50 * time.Millisecond)
	h.Complete(runID, RunOutcome{Status: RunStatusAborted})

	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("WaitResume returned error after Finish: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitResume did not return within 1s of Finish")
	}
}

func TestHub_FinishCleansUpSignalsAndPause(t *testing.T) {
	t.Parallel()
	h := NewRunHub(nil)

	const runID = "r1"
	h.Send(runID, ControlSignal{Kind: CtrlPause})
	_ = h.Poll(runID)
	h.Complete(runID, RunOutcome{Status: RunStatusDone})

	h.mu.Lock()
	_, hasSig := h.signals[runID]
	_, hasPaused := h.paused[runID]
	h.mu.Unlock()
	if hasSig {
		t.Error("signals[runID] still present after Finish")
	}
	if hasPaused {
		t.Error("paused[runID] still present after Finish")
	}
	if got := h.Poll(runID); got.Kind != CtrlNone {
		t.Errorf("Poll after Finish = %v, want CtrlNone", got.Kind)
	}
}

func TestHub_CompletePublishesOneTerminalEvent(t *testing.T) {
	hub := NewRunHub(nil)
	const runID = "terminal-once"
	ch := hub.Subscribe(runID)
	outcome := RunOutcome{
		Status: RunStatusDone, StepCount: 2, TokenUsed: 12, Answer: "answer",
		SessionMessages: []llm.Message{{Role: "tool", ToolCallID: "call-1", Name: "observe", Content: "result"}},
	}
	hub.Complete(runID, outcome)
	hub.Complete(runID, outcome)

	select {
	case event := <-ch:
		terminal := TerminalFromEvent(event)
		if terminal == nil || terminal.Status != RunStatusDone || terminal.Answer != "answer" {
			t.Fatalf("event = %+v, want done terminal", event)
		}
		if len(terminal.SessionMessages) != 1 || terminal.SessionMessages[0].ToolCallID != "call-1" {
			t.Fatalf("terminal lost session tool messages: %+v", terminal)
		}
	case <-time.After(time.Second):
		t.Fatal("missing terminal event")
	}
	select {
	case event := <-ch:
		t.Fatalf("duplicate terminal event: %+v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHub_CompletePreservesTerminalWhenSubscriberBufferIsFull(t *testing.T) {
	hub := NewRunHub(nil)
	const runID = "full-buffer"
	ch := hub.Subscribe(runID)
	defer hub.Unsubscribe(runID, ch)
	for i := 0; i < subscriberDiagnosticLimit*2; i++ {
		hub.EmitTrace(runID, domain.EvaluationTrace{Sequence: i + 1})
	}
	hub.Complete(runID, RunOutcome{Status: RunStatusDone, Answer: "answer"})

	var terminal *RunTerminal
	deadline := time.After(time.Second)
	for terminal == nil {
		select {
		case event := <-ch:
			terminal = TerminalFromEvent(event)
		case <-deadline:
			t.Fatal("terminal event was not delivered after diagnostic congestion")
		}
	}
	if terminal.Answer != "answer" {
		t.Fatalf("terminal = %+v", terminal)
	}
}

func TestRunSubscriberMergesAdjacentTextAndStatusEvents(t *testing.T) {
	sub := &runSubscriber{
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
	}
	defer sub.close()

	if !sub.enqueue(SSEEvent{Type: EventAnswerDelta, Data: TextEvent{Text: "hello "}}) ||
		!sub.enqueue(SSEEvent{Type: EventAnswerDelta, Data: TextEvent{Text: "world"}}) {
		t.Fatal("answer deltas were not accepted")
	}
	if !sub.enqueue(SSEEvent{Type: EventStatus, Data: TextEvent{Text: "searching"}}) ||
		!sub.enqueue(SSEEvent{Type: EventStatus, Data: TextEvent{Text: "answering"}}) {
		t.Fatal("status events were not accepted")
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.queue) != 2 {
		t.Fatalf("queue length = %d, want 2", len(sub.queue))
	}
	answer, ok := sub.queue[0].Data.(TextEvent)
	if !ok || answer.Text != "hello world" {
		t.Fatalf("merged answer = %#v", sub.queue[0].Data)
	}
	status, ok := sub.queue[1].Data.(TextEvent)
	if !ok || status.Text != "answering" {
		t.Fatalf("merged status = %#v", sub.queue[1].Data)
	}
}

func TestRunSubscriberQueueIsBoundedAndPreservesTerminal(t *testing.T) {
	sub := &runSubscriber{
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
	}
	defer sub.close()

	for index := 0; index < subscriberDiagnosticLimit; index++ {
		if !sub.enqueue(SSEEvent{Type: EventToolStarted, Data: ToolStartedEvent{Step: index + 1}}) {
			t.Fatalf("tool event %d was rejected before the queue reached its limit", index)
		}
	}
	if !sub.enqueue(SSEEvent{Type: EventRunFinished, Data: &RunTerminal{RunID: "bounded"}}) {
		t.Fatal("terminal event was rejected by a full queue")
	}

	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.queue) != subscriberDiagnosticLimit {
		t.Fatalf("queue length = %d, want %d", len(sub.queue), subscriberDiagnosticLimit)
	}
	if sub.queue[len(sub.queue)-1].Type != EventRunFinished {
		t.Fatalf("last event = %s, want %s", sub.queue[len(sub.queue)-1].Type, EventRunFinished)
	}
}

func TestRunSubscriberCloseReleasesQueueAndRejectsLateEvents(t *testing.T) {
	sub := &runSubscriber{
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
	}
	if !sub.enqueue(SSEEvent{Type: EventToolStarted}) {
		t.Fatal("initial event was rejected")
	}

	sub.close()
	if sub.enqueue(SSEEvent{Type: EventRunFinished}) {
		t.Fatal("closed subscriber accepted a late event")
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.queue) != 0 {
		t.Fatalf("closed subscriber retained %d queued events", len(sub.queue))
	}
}

func TestHub_AbortReleasesPausedRunBeforeNextStep(t *testing.T) {
	hub := NewRunHub(nil)
	const runID = "abort-paused"
	hub.Send(runID, ControlSignal{Kind: CtrlPause})
	if signal := hub.Poll(runID); signal.Kind != CtrlPause {
		t.Fatalf("first signal = %v, want pause", signal.Kind)
	}

	waited := make(chan error, 1)
	go func() { waited <- hub.WaitResume(context.Background(), runID) }()
	hub.Send(runID, ControlSignal{Kind: CtrlAbort})
	select {
	case err := <-waited:
		if err != nil {
			t.Fatalf("WaitResume: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("abort did not release paused run")
	}
	if signal := hub.Poll(runID); signal.Kind != CtrlAbort {
		t.Fatalf("next signal = %v, want abort", signal.Kind)
	}
}

func TestHub_ResumeCancelsQueuedPause(t *testing.T) {
	hub := NewRunHub(nil)
	const runID = "resume-before-pause"
	hub.Send(runID, ControlSignal{Kind: CtrlPause})
	if err := hub.Resume(runID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if signal := hub.Poll(runID); signal.Kind != CtrlNone {
		t.Fatalf("signal = %v, want queued pause canceled", signal.Kind)
	}
}

func TestHub_PersistsPauseAndResume(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	hub := NewRunHub(&RunStore{db: db})
	const runID = "persist-control"
	mock.ExpectExec("UPDATE agent_runs SET status=\\? WHERE id=\\? AND status=\\?").
		WithArgs(RunStatusPaused, runID, RunStatusRunning).
		WillReturnResult(sqlmock.NewResult(0, 1))
	hub.Send(runID, ControlSignal{Kind: CtrlPause})
	if signal := hub.Poll(runID); signal.Kind != CtrlPause {
		t.Fatalf("signal = %v, want pause", signal.Kind)
	}

	mock.ExpectExec("UPDATE agent_runs SET status=\\? WHERE id=\\? AND status=\\?").
		WithArgs(RunStatusRunning, runID, RunStatusPaused).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := hub.Resume(runID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHubBroadcastsOnlyTemporaryPreviewForToolResults(t *testing.T) {
	hub := NewRunHub(nil)
	const runID = "tool-preview"
	channel := hub.Subscribe(runID)
	content := strings.Repeat("authoritative-", 200)
	promptContent := strings.Repeat("model-input-", 200)

	if err := hub.OnStep(t.Context(), runID, StepRecord{
		StepNo:        2,
		Kind:          StepKindToolResult,
		ToolCallID:    "call-preview-1",
		Content:       content,
		PromptContent: promptContent,
		SizeBytes:     int64(len(content)),
	}); err != nil {
		t.Fatalf("OnStep: %v", err)
	}

	select {
	case event := <-channel:
		payload, ok := event.Data.(ToolFinishedEvent)
		if event.Type != EventToolFinished || !ok {
			t.Fatalf("event = %+v", event)
		}
		if payload.Summary == "" || !strings.HasPrefix(content, strings.TrimSuffix(payload.Summary, "...")) {
			t.Fatalf("summary = %q", payload.Summary)
		}
		if payload.SizeBytes != int64(len(content)) {
			t.Fatalf("size bytes = %d", payload.SizeBytes)
		}
		if payload.ToolCallID != "call-preview-1" {
			t.Fatalf("tool call id = %q", payload.ToolCallID)
		}
	case <-time.After(time.Second):
		t.Fatal("missing tool result event")
	}
}
