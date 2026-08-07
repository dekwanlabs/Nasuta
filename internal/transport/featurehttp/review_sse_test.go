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

type reviewEventStreamStore struct {
	featuredelivery.Store

	mu           sync.Mutex
	feature      featuredelivery.FeatureRequest
	artifact     featuredelivery.Artifact
	round        featuredelivery.ReviewRound
	events       []featuredelivery.ReviewEvent
	listCalls    []int64
	roundReads   int
	secondReplay chan struct{}
	secondOnce   sync.Once
}

func (store *reviewEventStreamStore) GetReviewRound(
	_ context.Context,
	id string,
) (*featuredelivery.ReviewRound, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.roundReads++
	if id != store.round.ID {
		return nil, featuredelivery.ErrNotFound
	}
	round := store.round
	return &round, nil
}

func (store *reviewEventStreamStore) GetArtifact(
	_ context.Context,
	id string,
) (*featuredelivery.Artifact, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if id != store.artifact.ID {
		return nil, featuredelivery.ErrNotFound
	}
	artifact := store.artifact
	return &artifact, nil
}

func (store *reviewEventStreamStore) GetFeature(
	_ context.Context,
	id string,
) (*featuredelivery.FeatureRequest, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if id != store.feature.ID {
		return nil, featuredelivery.ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *reviewEventStreamStore) ListReviewEvents(
	_ context.Context,
	roundID string,
	afterSeq int64,
	limit int,
) ([]featuredelivery.ReviewEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if roundID != store.round.ID {
		return nil, featuredelivery.ErrNotFound
	}
	store.listCalls = append(store.listCalls, afterSeq)
	if len(store.listCalls) == 2 && store.secondReplay != nil {
		store.secondOnce.Do(func() { close(store.secondReplay) })
	}
	start := sort.Search(len(store.events), func(index int) bool {
		return store.events[index].Seq > afterSeq
	})
	end := min(start+limit, len(store.events))
	return append([]featuredelivery.ReviewEvent(nil), store.events[start:end]...), nil
}

func (store *reviewEventStreamStore) RequestReviewRoundCancel(
	_ context.Context,
	roundID string,
	at time.Time,
) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if roundID != store.round.ID {
		return false, featuredelivery.ErrNotFound
	}
	switch store.round.Status {
	case featuredelivery.RoundCancelled:
		return false, nil
	case featuredelivery.RoundCreated, featuredelivery.RoundRunning,
		featuredelivery.RoundEvaluating:
		store.round.Status = featuredelivery.RoundCancelled
		store.round.CompletedAt = &at
		return true, nil
	default:
		return false, featuredelivery.ErrConflict
	}
}

func (store *reviewEventStreamStore) AppendReviewEvent(
	_ context.Context,
	event featuredelivery.ReviewEvent,
) (*featuredelivery.ReviewEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if event.RoundID != store.round.ID {
		return nil, featuredelivery.ErrNotFound
	}
	event.Seq = int64(len(store.events) + 1)
	store.events = append(store.events, event)
	persisted := event
	return &persisted, nil
}

func (store *reviewEventStreamStore) cursors() []int64 {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]int64(nil), store.listCalls...)
}

func (store *reviewEventStreamStore) authorizationReads() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.roundReads
}

func TestReviewReplayEventsPaginatesWithoutGaps(t *testing.T) {
	store := newReviewEventStreamStore(featuredelivery.RoundRunning)
	store.events = makeReviewEvents(1001, 0)
	handler, reader := openReviewEventReader(t, store)
	response := httptest.NewRecorder()
	writer, err := newReviewEventWriter(response)
	if err != nil {
		t.Fatal(err)
	}

	lastSeq, terminal, err := handler.replayReviewEvents(
		context.Background(), writer, reader, 0,
	)
	if err != nil || terminal || lastSeq != 1001 {
		t.Fatalf("last=%d terminal=%t err=%v", lastSeq, terminal, err)
	}
	want := []int64{0, 500, 1000}
	cursors := store.cursors()
	if len(cursors) != len(want) {
		t.Fatalf("replay cursors=%v", cursors)
	}
	for index := range want {
		if cursors[index] != want[index] {
			t.Fatalf("replay cursors=%v", cursors)
		}
	}
}

func TestReviewReplayEventsStopsAtTerminalEvent(t *testing.T) {
	store := newReviewEventStreamStore(featuredelivery.RoundCompleted)
	store.events = makeReviewEvents(800, 501)
	handler, reader := openReviewEventReader(t, store)
	response := httptest.NewRecorder()
	writer, err := newReviewEventWriter(response)
	if err != nil {
		t.Fatal(err)
	}

	lastSeq, terminal, err := handler.replayReviewEvents(
		context.Background(), writer, reader, 0,
	)
	if err != nil || !terminal || lastSeq != 501 {
		t.Fatalf("last=%d terminal=%t err=%v", lastSeq, terminal, err)
	}
	if strings.Contains(response.Body.String(), "id: 502\n") {
		t.Fatal("replay emitted events after the terminal event")
	}
	want := []int64{0, 500}
	cursors := store.cursors()
	if len(cursors) != len(want) {
		t.Fatalf("replay cursors=%v", cursors)
	}
	for index := range want {
		if cursors[index] != want[index] {
			t.Fatalf("replay cursors=%v", cursors)
		}
	}
}

func TestReviewLiveEventFillsPersistentSequenceGapWithoutDuplicate(t *testing.T) {
	store := newReviewEventStreamStore(featuredelivery.RoundRunning)
	store.events = makeReviewEvents(2, 0)
	handler, reader := openReviewEventReader(t, store)
	response := httptest.NewRecorder()
	writer, err := newReviewEventWriter(response)
	if err != nil {
		t.Fatal(err)
	}

	lastSeq, terminal, err := handler.emitLiveReviewEvent(
		context.Background(), writer, reader, 0, store.events[1],
	)
	if err != nil || terminal || lastSeq != 2 {
		t.Fatalf("last=%d terminal=%t err=%v", lastSeq, terminal, err)
	}
	body := response.Body.String()
	if strings.Count(body, "id: 1\n") != 1 ||
		strings.Count(body, "id: 2\n") != 1 {
		t.Fatalf("gap replay body=%q", body)
	}
}

func TestReviewEventStreamResumesAfterLastEventIDAndStopsAtTerminal(t *testing.T) {
	store := newReviewEventStreamStore(featuredelivery.RoundCompleted)
	store.events = []featuredelivery.ReviewEvent{
		reviewEvent(1, featuredelivery.ReviewEventRoundStarted),
		reviewEvent(2, featuredelivery.ReviewEventRoundCompleted),
		reviewEvent(3, featuredelivery.ReviewEventAssignmentSucceeded),
	}
	handler := New(featuredelivery.NewService(store, nil, time.Second))
	mux := http.NewServeMux()
	handler.RegisterRoutes(func(pattern string, route http.HandlerFunc) {
		mux.HandleFunc(pattern, route)
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-rounds/round-1/events/stream",
		nil,
	)
	request.Header.Set("Last-Event-ID", "1")
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 7},
	))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "text/event-stream" {
		t.Fatalf("content type=%q", contentType)
	}
	body := response.Body.String()
	if strings.Contains(body, "id: 1\n") ||
		!strings.Contains(body, "id: 2\n") ||
		strings.Contains(body, "id: 3\n") {
		t.Fatalf("resumed stream body=%q", body)
	}
	if reads := store.authorizationReads(); reads != 1 {
		t.Fatalf("round authorization reads=%d, want 1", reads)
	}
}

func TestReviewEventStreamReplaysThenSwitchesLiveWithOneAuthorization(t *testing.T) {
	store := newReviewEventStreamStore(featuredelivery.RoundRunning)
	store.events = []featuredelivery.ReviewEvent{
		reviewEvent(1, featuredelivery.ReviewEventRoundStarted),
	}
	store.secondReplay = make(chan struct{})
	service := featuredelivery.NewService(store, nil, time.Second)
	handler := New(service)
	mux := http.NewServeMux()
	handler.RegisterRoutes(func(pattern string, route http.HandlerFunc) {
		mux.HandleFunc(pattern, route)
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-rounds/round-1/events/stream",
		nil,
	)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 7},
	))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		mux.ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-store.secondReplay:
	case <-time.After(time.Second):
		t.Fatal("stream did not reach the subscribed replay window")
	}
	if err := service.CancelReviewRound(context.Background(), "round-1", true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not exit after round cancellation")
	}

	body := response.Body.String()
	if !strings.Contains(body, "id: 1\n") ||
		!strings.Contains(body, "id: 2\n") ||
		!strings.Contains(body, "event: round_cancelled\n") {
		t.Fatalf("stream body=%q", body)
	}
	if reads := store.authorizationReads(); reads != 1 {
		t.Fatalf("round authorization reads=%d, want 1", reads)
	}
}

func TestReviewEventStreamHidesUnauthorizedRound(t *testing.T) {
	store := newReviewEventStreamStore(featuredelivery.RoundCompleted)
	store.events = []featuredelivery.ReviewEvent{
		reviewEvent(1, featuredelivery.ReviewEventRoundCompleted),
	}
	handler := New(featuredelivery.NewService(store, nil, time.Second))
	mux := http.NewServeMux()
	handler.RegisterRoutes(func(pattern string, route http.HandlerFunc) {
		mux.HandleFunc(pattern, route)
	})
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/feature-delivery/review-rounds/round-1/events/stream",
		nil,
	)
	request = request.WithContext(auth.WithUser(
		request.Context(), &auth.User{ID: 8},
	))
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("unauthorized response started an event stream")
	}
}

func newReviewEventStreamStore(
	status featuredelivery.ReviewRoundStatus,
) *reviewEventStreamStore {
	return &reviewEventStreamStore{
		feature: featuredelivery.FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: featuredelivery.Artifact{
			ID: "artifact-1", RequestID: "feat-1", ContentHash: "artifact-hash",
		},
		round: featuredelivery.ReviewRound{
			ID: "round-1", Status: status,
			Subject: featuredelivery.ReviewSubject{
				Kind: featuredelivery.SubjectSystemDesign, ID: "artifact-1",
				SourceContentHash: "artifact-hash",
			},
		},
	}
}

func openReviewEventReader(
	t *testing.T,
	store *reviewEventStreamStore,
) (*Handler, *featuredelivery.ReviewEventReader) {
	t.Helper()
	handler := New(featuredelivery.NewService(store, nil, time.Second))
	_, reader, err := handler.service.OpenReviewEvents(
		context.Background(), "round-1", 7, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	return handler, reader
}

func makeReviewEvents(
	count int,
	terminalSeq int64,
) []featuredelivery.ReviewEvent {
	events := make([]featuredelivery.ReviewEvent, count)
	for index := range events {
		seq := int64(index + 1)
		kind := featuredelivery.ReviewEventAssignmentStarted
		if seq == terminalSeq {
			kind = featuredelivery.ReviewEventRoundCompleted
		}
		events[index] = reviewEvent(seq, kind)
	}
	return events
}

func reviewEvent(
	seq int64,
	kind featuredelivery.ReviewEventKind,
) featuredelivery.ReviewEvent {
	return featuredelivery.ReviewEvent{
		RoundID: "round-1",
		Seq:     seq,
		Kind:    kind,
		Summary: "event",
		CreatedAt: time.Date(
			2026, 8, 6, 12, 0, 0, 0, time.UTC,
		),
	}
}
