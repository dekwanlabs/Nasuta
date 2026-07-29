package featuredelivery

import (
	"context"
	"errors"
	"testing"
	"time"
)

type failureStore struct {
	Store
	status        RunStatus
	event         EventKind
	summary       string
	transitionErr error
}

func (store *failureStore) TransitionImplementation(_ context.Context, _ string, _ string, _ RunStatus, to RunStatus, update RunUpdate) error {
	store.status = to
	store.summary = update.ErrorSummary
	return store.transitionErr
}

func (store *failureStore) AppendRunEvent(_ context.Context, event RunEvent) (*RunEvent, error) {
	store.event = event.Kind
	event.Seq = 1
	return &event, nil
}

func TestImplementationFailureClassification(t *testing.T) {
	tests := []struct {
		name        string
		cause       error
		runErr      error
		wantStatus  RunStatus
		wantEvent   EventKind
		wantSummary string
	}{
		{
			name: "administrator cancellation", cause: errImplementationCancelled,
			runErr: context.Canceled, wantStatus: RunCancelled,
			wantEvent: EventRunCancelled, wantSummary: "implementation cancelled",
		},
		{
			name: "timeout", cause: errImplementationTimedOut,
			runErr: context.DeadlineExceeded, wantStatus: RunFailed,
			wantEvent: EventRunFailed, wantSummary: "implementation timed out",
		},
		{
			name: "shutdown", cause: context.Canceled,
			runErr: context.Canceled, wantStatus: RunInterrupted,
			wantEvent: EventRunInterrupted, wantSummary: "implementation interrupted",
		},
		{
			name: "lease loss", cause: errImplementationLeaseLost,
			runErr: context.Canceled, wantStatus: RunInterrupted,
			wantEvent: EventRunInterrupted, wantSummary: "worker lease lost",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runCtx, cancel := context.WithCancelCause(context.Background())
			cancel(test.cause)
			store := &failureStore{}
			manager := &ImplementationManager{
				store: store, hub: NewEventHub(),
				config: ImplementationConfig{WorktreeTTL: time.Hour},
				now:    func() time.Time { return time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC) },
			}
			manager.finishFailureWithResult(
				runCtx,
				ImplementationRun{ID: "run-1"},
				"worker-1",
				RunRunning,
				CodingResult{},
				test.runErr,
			)
			if store.status != test.wantStatus || store.event != test.wantEvent || store.summary != test.wantSummary {
				t.Fatalf("got status=%s event=%s summary=%q", store.status, store.event, store.summary)
			}
		})
	}
}

func TestImplementationFailureDoesNotEmitAfterLostTransition(t *testing.T) {
	runCtx, cancel := context.WithCancelCause(context.Background())
	cancel(errImplementationLeaseLost)
	store := &failureStore{transitionErr: ErrConflict}
	manager := &ImplementationManager{
		store: store, hub: NewEventHub(),
		config: ImplementationConfig{WorktreeTTL: time.Hour},
		now:    func() time.Time { return time.Now().UTC() },
	}
	manager.finishFailure(runCtx, ImplementationRun{ID: "run-1"}, "worker-1", RunRunning, context.Canceled)
	if store.event != "" {
		t.Fatalf("event emitted after failed transition: %s", store.event)
	}
	if !errors.Is(store.transitionErr, ErrConflict) {
		t.Fatal("test setup lost transition conflict")
	}
}
