package workflowhttp

import (
	"context"
	"net/http"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/transport/sse"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

const (
	eventReplayPage     = 100
	eventReplayInterval = 2 * time.Second
)

func (handler *Handler) StreamEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	afterSeq, err := eventCursor(r, true)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	runID := r.PathValue("run_id")
	run, reader, err := handler.service.OpenRunEvents(
		r.Context(), runID, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writer, err := newEventWriter(w)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	lastSeq, terminal, err := handler.replayEvents(
		r.Context(), writer, reader, afterSeq,
	)
	if err != nil {
		writer.EmitError(err)
		return
	}
	if terminal || terminalRunStatus(run.Status) {
		return
	}

	live, unsubscribe, err := handler.service.SubscribeEvents(runID)
	if err != nil {
		writer.EmitError(err)
		return
	}
	defer unsubscribe()

	lastSeq, terminal, err = handler.replayEvents(
		r.Context(), writer, reader, lastSeq,
	)
	if err != nil {
		writer.EmitError(err)
		return
	}
	if terminal {
		return
	}

	ticker := time.NewTicker(eventReplayInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-live:
			if !open {
				writer.EmitError(workflow.ErrUnavailable)
				return
			}
			lastSeq, terminal, err = handler.emitLiveEvent(
				r.Context(), writer, reader, lastSeq, event,
			)
			if err != nil {
				return
			}
			if terminal {
				return
			}
		case <-ticker.C:
			lastSeq, terminal, err = handler.replayEvents(
				r.Context(), writer, reader, lastSeq,
			)
			if err != nil {
				writer.EmitError(err)
				return
			}
			if terminal {
				return
			}
			writer.Keepalive()
		}
	}
}

func (handler *Handler) emitLiveEvent(
	ctx context.Context,
	writer *eventWriter,
	reader workflow.EventReader,
	lastSeq int64,
	event workflow.Event,
) (int64, bool, error) {
	return sse.EmitLive(ctx, reader, lastSeq, event, workflowEventReplayOptions(writer))
}

func (handler *Handler) replayEvents(
	ctx context.Context,
	writer *eventWriter,
	reader workflow.EventReader,
	afterSeq int64,
) (int64, bool, error) {
	return sse.Replay(ctx, reader, afterSeq, workflowEventReplayOptions(writer))
}

func workflowEventReplayOptions(writer *eventWriter) sse.ReplayOptions[workflow.Event] {
	return sse.ReplayOptions[workflow.Event]{
		PageSize: eventReplayPage,
		Sequence: func(event workflow.Event) int64 { return event.Seq },
		Terminal: func(event workflow.Event) bool { return terminalEvent(event.Kind) },
		Emit: func(event workflow.Event) error {
			return writer.Emit(event.Seq, event.Kind, event)
		},
	}
}

type eventWriter = sse.Writer

func newEventWriter(w http.ResponseWriter) (*eventWriter, error) {
	return sse.New(w)
}

func terminalEvent(kind string) bool {
	switch kind {
	case "workflow_succeeded", "workflow_failed", "workflow_cancelled",
		"workflow_timed_out":
		return true
	default:
		return false
	}
}

func terminalRunStatus(status workflow.RunStatus) bool {
	switch status {
	case workflow.RunSucceeded, workflow.RunFailed,
		workflow.RunCancelled, workflow.RunTimedOut:
		return true
	default:
		return false
	}
}
