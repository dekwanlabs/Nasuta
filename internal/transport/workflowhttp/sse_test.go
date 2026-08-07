package workflowhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/auth"
)

type sliceEventReader struct {
	mu     sync.Mutex
	events []agentworkflow.Event
	calls  []int64
}

func (reader *sliceEventReader) List(
	_ context.Context,
	afterSeq int64,
	limit int,
) ([]agentworkflow.Event, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.calls = append(reader.calls, afterSeq)
	start := sort.Search(len(reader.events), func(index int) bool {
		return reader.events[index].Seq > afterSeq
	})
	end := min(start+limit, len(reader.events))
	return append([]agentworkflow.Event(nil), reader.events[start:end]...), nil
}

func (reader *sliceEventReader) cursors() []int64 {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return append([]int64(nil), reader.calls...)
}

func TestWorkflowEventCursorUsesLastEventID(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/workflow-runs/workflow_1/events/stream",
		nil,
	)
	request.Header.Set("Last-Event-ID", "42")
	seq, err := eventCursor(request, true)
	if err != nil || seq != 42 {
		t.Fatalf("cursor=%d err=%v", seq, err)
	}

	request = httptest.NewRequest(
		http.MethodGet,
		"/events?after_seq=7",
		nil,
	)
	request.Header.Set("Last-Event-ID", "42")
	seq, err = eventCursor(request, true)
	if err != nil || seq != 7 {
		t.Fatalf("query cursor=%d err=%v", seq, err)
	}
}

func TestWorkflowReplayEventsPaginatesAndStopsAtTerminal(t *testing.T) {
	events := makeWorkflowEvents(250, 205)
	reader := &sliceEventReader{events: events}
	handler := &Handler{}
	response := httptest.NewRecorder()
	writer, err := newEventWriter(response)
	if err != nil {
		t.Fatal(err)
	}
	lastSeq, terminal, err := handler.replayEvents(
		context.Background(), writer, reader, 0,
	)
	if err != nil || !terminal || lastSeq != 205 {
		t.Fatalf("last=%d terminal=%t err=%v", lastSeq, terminal, err)
	}
	if strings.Contains(response.Body.String(), "id: 206\n") {
		t.Fatal("replay emitted events after terminal state")
	}
	want := []int64{0, 100, 200}
	cursors := reader.cursors()
	if len(cursors) != len(want) {
		t.Fatalf("replay cursors=%v", cursors)
	}
	for index := range want {
		if cursors[index] != want[index] {
			t.Fatalf("replay cursors=%v", cursors)
		}
	}
}

func TestWorkflowLiveEventFillsPersistentSequenceGap(t *testing.T) {
	reader := &sliceEventReader{events: []agentworkflow.Event{
		{
			WorkflowRunID: "workflow_1", Seq: 2,
			Kind: "node_succeeded", Summary: "node succeeded",
		},
	}}
	response := httptest.NewRecorder()
	writer, err := newEventWriter(response)
	if err != nil {
		t.Fatal(err)
	}
	lastSeq, terminal, err := (&Handler{}).emitLiveEvent(
		context.Background(),
		writer,
		reader,
		1,
		agentworkflow.Event{
			WorkflowRunID: "workflow_1", Seq: 3,
			Kind: "handoff_created", Summary: "handoff created",
		},
	)
	if err != nil || terminal || lastSeq != 3 {
		t.Fatalf("last=%d terminal=%t err=%v", lastSeq, terminal, err)
	}
	body := response.Body.String()
	if strings.Count(body, "id: 2\n") != 1 ||
		strings.Count(body, "id: 3\n") != 1 {
		t.Fatalf("gap replay body=%q", body)
	}
}

func TestWorkflowStreamReplaysThenSwitchesLiveWithOneAuthorization(t *testing.T) {
	reader := &sliceEventReader{events: []agentworkflow.Event{
		{
			WorkflowRunID: "workflow_1", Seq: 1,
			Kind: "workflow_started", Summary: "started",
		},
	}}
	live := make(chan agentworkflow.Event, 1)
	live <- agentworkflow.Event{
		WorkflowRunID: "workflow_1", Seq: 2,
		Kind: "workflow_succeeded", Summary: "succeeded",
	}
	workflows := &recordingService{
		run: agentworkflow.WorkflowRunRecord{
			ID: "workflow_1", ActorUserID: 7, Status: agentworkflow.RunRunning,
		},
		reader: reader,
		live:   live,
	}
	mux := workflowMux(&Handler{service: workflows})
	response := serveWorkflowRequest(
		mux,
		http.MethodGet,
		"/api/workflow-runs/workflow_1/events/stream",
		"",
		&auth.User{ID: 7},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "id: 1\n") ||
		!strings.Contains(body, "id: 2\n") {
		t.Fatalf("stream body=%q", body)
	}
	if workflows.openCalls != 1 || workflows.subscribeCalls != 1 {
		t.Fatalf(
			"open calls=%d subscribe calls=%d",
			workflows.openCalls,
			workflows.subscribeCalls,
		)
	}
}

func TestWorkflowStreamResumesAfterLastEventIDAndExitsAtTerminalRun(t *testing.T) {
	reader := &sliceEventReader{events: []agentworkflow.Event{
		{
			WorkflowRunID: "workflow_1", Seq: 1,
			Kind: "workflow_started", Summary: "started",
		},
		{
			WorkflowRunID: "workflow_1", Seq: 2,
			Kind: "workflow_succeeded", Summary: "succeeded",
		},
	}}
	workflows := &recordingService{
		run: agentworkflow.WorkflowRunRecord{
			ID: "workflow_1", ActorUserID: 7, Status: agentworkflow.RunSucceeded,
		},
		reader: reader,
	}
	handler := &Handler{service: workflows}
	mux := workflowMux(handler)
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/workflow-runs/workflow_1/events/stream",
		nil,
	)
	request.Header.Set("Last-Event-ID", "1")
	request = request.WithContext(auth.WithUser(
		request.Context(),
		&auth.User{ID: 7},
	))
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	body := response.Body.String()
	if strings.Contains(body, "id: 1\n") ||
		!strings.Contains(body, "id: 2\n") {
		t.Fatalf("resumed stream body=%q", body)
	}
	if workflows.openCalls != 1 || workflows.subscribeCalls != 0 {
		t.Fatalf(
			"open calls=%d subscribe calls=%d",
			workflows.openCalls,
			workflows.subscribeCalls,
		)
	}
}

func makeWorkflowEvents(count int, terminalSeq int64) []agentworkflow.Event {
	createdAt := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	events := make([]agentworkflow.Event, count)
	for index := range events {
		seq := int64(index + 1)
		kind := "node_progress"
		if seq == terminalSeq {
			kind = "workflow_succeeded"
		}
		events[index] = agentworkflow.Event{
			WorkflowRunID: "workflow_1",
			Seq:           seq,
			Kind:          kind,
			Summary:       "event",
			CreatedAt:     createdAt,
		}
	}
	return events
}
