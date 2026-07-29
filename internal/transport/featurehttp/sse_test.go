package featurehttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

type eventReplayStore struct {
	featuredelivery.Store
	mu     sync.Mutex
	events []featuredelivery.RunEvent
	calls  []int64
	reads  int
	status featuredelivery.RunStatus
}

func (store *eventReplayStore) GetImplementation(context.Context, string) (*featuredelivery.ImplementationRun, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.reads++
	status := store.status
	if status == "" {
		status = featuredelivery.RunRunning
	}
	return &featuredelivery.ImplementationRun{ID: "run-1", RequestID: "feat-1", Status: status}, nil
}

func (store *eventReplayStore) GetFeature(context.Context, string) (*featuredelivery.FeatureRequest, error) {
	return &featuredelivery.FeatureRequest{ID: "feat-1", CreatedBy: 7}, nil
}

func (store *eventReplayStore) ListRunEvents(_ context.Context, _ string, afterSeq int64, limit int) ([]featuredelivery.RunEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.calls = append(store.calls, afterSeq)
	start := sort.Search(len(store.events), func(index int) bool { return store.events[index].Seq > afterSeq })
	end := min(start+limit, len(store.events))
	return append([]featuredelivery.RunEvent(nil), store.events[start:end]...), nil
}

func (store *eventReplayStore) cursors() []int64 {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]int64(nil), store.calls...)
}

func (store *eventReplayStore) implementationReads() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.reads
}

func TestEventCursorUsesLastEventID(t *testing.T) {
	request := httptest.NewRequest("GET", "/events", nil)
	request.Header.Set("Last-Event-ID", "42")
	seq, err := eventCursor(request)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 42 {
		t.Fatalf("event cursor = %d", seq)
	}
}

func TestEventCursorPrefersQueryAndRejectsInvalidValues(t *testing.T) {
	request := httptest.NewRequest("GET", "/events?after_seq=7", nil)
	request.Header.Set("Last-Event-ID", "42")
	seq, err := eventCursor(request)
	if err != nil || seq != 7 {
		t.Fatalf("query cursor=%d err=%v", seq, err)
	}
	for _, value := range []string{"-1", "not-a-number"} {
		request = httptest.NewRequest("GET", "/events?after_seq="+value, nil)
		if _, err := eventCursor(request); err == nil {
			t.Fatalf("cursor %q must be rejected", value)
		}
	}
}

func TestReplayEventsPaginatesWithoutGaps(t *testing.T) {
	store := &eventReplayStore{events: makeEvents(1001, 0)}
	handler := New(featuredelivery.NewService(store, nil, 0))
	recorder := httptest.NewRecorder()
	writer, err := newEventWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	reader := openTestRunEventReader(t, handler)
	lastSeq, terminal, err := handler.replayEvents(context.Background(), writer, reader, 0)
	if err != nil || terminal || lastSeq != 1001 {
		t.Fatalf("last=%d terminal=%t err=%v", lastSeq, terminal, err)
	}
	wantCursors := []int64{0, 500, 1000}
	if cursors := store.cursors(); len(cursors) != len(wantCursors) {
		t.Fatalf("replay cursors=%v", cursors)
	} else {
		for index := range cursors {
			if cursors[index] != wantCursors[index] {
				t.Fatalf("replay cursors=%v", cursors)
			}
		}
	}
	body := recorder.Body.String()
	for _, marker := range []string{"id: 1\n", "id: 500\n", "id: 501\n", "id: 1001\n"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("stream is missing %q", marker)
		}
	}
}

func TestReplayEventsStopsAtTerminalEvent(t *testing.T) {
	store := &eventReplayStore{events: makeEvents(800, 501)}
	handler := New(featuredelivery.NewService(store, nil, 0))
	recorder := httptest.NewRecorder()
	writer, err := newEventWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	reader := openTestRunEventReader(t, handler)
	lastSeq, terminal, err := handler.replayEvents(context.Background(), writer, reader, 0)
	if err != nil || !terminal || lastSeq != 501 {
		t.Fatalf("last=%d terminal=%t err=%v", lastSeq, terminal, err)
	}
	if strings.Contains(recorder.Body.String(), "id: 502\n") {
		t.Fatal("events after terminal state were emitted")
	}
	cursors := store.cursors()
	if len(cursors) != 2 || cursors[0] != 0 || cursors[1] != 500 {
		t.Fatalf("replay cursors=%v", cursors)
	}
}

func TestEmitLiveEventReplaysSequenceGapWithoutDuplicate(t *testing.T) {
	store := &eventReplayStore{events: makeEvents(2, 0)}
	handler := New(featuredelivery.NewService(store, nil, 0))
	recorder := httptest.NewRecorder()
	writer, err := newEventWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	reader := openTestRunEventReader(t, handler)
	lastSeq, terminal, err := handler.emitLiveEvent(
		context.Background(), writer, reader, 0, store.events[1],
	)
	if err != nil || terminal || lastSeq != 2 {
		t.Fatalf("last=%d terminal=%t err=%v", lastSeq, terminal, err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "id: 1\n") || strings.Count(body, "id: 2\n") != 1 {
		t.Fatalf("gap replay output=%q", body)
	}
	if cursors := store.cursors(); len(cursors) != 1 || cursors[0] != 0 {
		t.Fatalf("gap replay cursors=%v", cursors)
	}
}

func TestEmitLiveEventSkipsReplayForContiguousSequence(t *testing.T) {
	store := &eventReplayStore{}
	handler := New(featuredelivery.NewService(store, nil, 0))
	recorder := httptest.NewRecorder()
	writer, err := newEventWriter(recorder)
	if err != nil {
		t.Fatal(err)
	}
	event := featuredelivery.RunEvent{RunID: "run-1", Seq: 2, Kind: featuredelivery.EventProviderMessage}
	reader := openTestRunEventReader(t, handler)
	lastSeq, terminal, err := handler.emitLiveEvent(context.Background(), writer, reader, 1, event)
	if err != nil || terminal || lastSeq != 2 {
		t.Fatalf("last=%d terminal=%t err=%v", lastSeq, terminal, err)
	}
	if cursors := store.cursors(); len(cursors) != 0 {
		t.Fatalf("unexpected replay cursors=%v", cursors)
	}
}

func TestRunEventsAuthorizesOnceAcrossReplayPages(t *testing.T) {
	store := &eventReplayStore{events: makeEvents(1001, 0), status: featuredelivery.RunSucceeded}
	handler := New(featuredelivery.NewService(store, nil, 0))
	mux := http.NewServeMux()
	handler.RegisterRoutes(func(pattern string, route http.HandlerFunc) { mux.HandleFunc(pattern, route) })
	request := httptest.NewRequest(http.MethodGet, "/api/feature-implementations/run-1/events", nil)
	request = request.WithContext(auth.WithUser(request.Context(), &auth.User{ID: 7}))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "id: 1001\n") {
		t.Fatalf("status=%d stream tail missing", response.Code)
	}
	if reads := store.implementationReads(); reads != 1 {
		t.Fatalf("implementation authorization reads=%d, want 1", reads)
	}
}

func makeEvents(count int, terminalSeq int64) []featuredelivery.RunEvent {
	createdAt := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	events := make([]featuredelivery.RunEvent, count)
	for index := range events {
		seq := int64(index + 1)
		kind := featuredelivery.EventProviderMessage
		if seq == terminalSeq {
			kind = featuredelivery.EventRunSucceeded
		}
		events[index] = featuredelivery.RunEvent{
			RunID: "run-1", Seq: seq, Kind: kind, Summary: "event", CreatedAt: createdAt,
		}
	}
	return events
}

func openTestRunEventReader(t *testing.T, handler *Handler) *featuredelivery.RunEventReader {
	t.Helper()
	_, reader, err := handler.service.OpenRunEvents(context.Background(), "run-1", 7, false)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}
