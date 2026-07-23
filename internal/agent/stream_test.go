package agent

import (
	"context"
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
			if ev.Terminal != nil {
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
		if event.Trace == nil || event.Trace.Node != "evidence_plan" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("trace event was not broadcast")
	}
}

func TestStreamPipeRecordsModelEventTiming(t *testing.T) {
	started := time.Now().Add(-20 * time.Millisecond)
	pipe := newStreamPipe(nil, "run", 1, started, nil)
	pipe.OnReasoning("thinking")
	pipe.OnToken("preamble")
	pipe.OnToolCallDelta()
	pipe.OnToolCall(llm.ToolCall{})

	got := pipe.Timings()
	if got.FirstEvent <= 0 || got.FirstReasoning <= 0 || got.FirstContent <= 0 || got.FirstToolDelta <= 0 || got.FirstToolCall <= 0 {
		t.Fatalf("timing = %+v", got)
	}
	if got.FirstEvent != got.FirstReasoning {
		t.Fatalf("first event = %s, first reasoning = %s", got.FirstEvent, got.FirstReasoning)
	}
}

func TestHub_BroadcastDeliversToSubscriber(t *testing.T) {
	t.Parallel()
	h := NewRunHub(nil)
	defer h.Complete("r1", RunOutcome{Status: RunStatusDone})

	ch := h.Subscribe("r1")
	h.OnToken(context.Background(), "r1", "hello")
	h.OnStep(context.Background(), "r1", StepRecord{StepNo: 1, Tool: "probe"})

	got := drainHub(ch, time.Second)
	if len(got) < 2 {
		t.Fatalf("expected at least 2 events, got %d (%+v)", len(got), got)
	}
	if got[0].Token != "hello" {
		t.Errorf("first event token = %q, want %q", got[0].Token, "hello")
	}
	if got[1].Step == nil || got[1].Step.Tool != "probe" {
		t.Errorf("second event step = %+v, want tool=probe", got[1].Step)
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
	outcome := RunOutcome{Status: RunStatusDone, StepCount: 2, TokenUsed: 12, Answer: "answer"}
	hub.Complete(runID, outcome)
	hub.Complete(runID, outcome)

	select {
	case event := <-ch:
		if event.Terminal == nil || event.Terminal.Status != RunStatusDone {
			t.Fatalf("event = %+v, want done terminal", event)
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
