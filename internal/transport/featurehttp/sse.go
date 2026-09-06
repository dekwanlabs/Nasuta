package featurehttp

import (
	"context"
	"net/http"
	"time"

	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	"github.com/dekwanlabs/nasuta/internal/transport/sse"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

const eventReplayPage = 500

func (handler *Handler) RunEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	runID := r.PathValue("run_id")
	run, reader, err := handler.service.OpenRunEvents(r.Context(), runID, user.ID, user.IsAdmin)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	afterSeq, err := eventCursor(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	writer, err := newEventWriter(w)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	lastSeq, terminal, err := handler.replayEvents(r.Context(), writer, reader, afterSeq)
	if err != nil {
		writer.EmitError(err)
		return
	}
	if terminal || delivery.IsTerminalRun(run.Status) {
		return
	}

	live, unsubscribe, err := handler.service.SubscribeRun(runID)
	if err != nil {
		writer.EmitError(err)
		return
	}
	defer unsubscribe()

	lastSeq, terminal, err = handler.replayEvents(r.Context(), writer, reader, lastSeq)
	if err != nil {
		writer.EmitError(err)
		return
	}
	if terminal {
		return
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-live:
			var liveTerminal bool
			lastSeq, liveTerminal, err = handler.emitLiveEvent(r.Context(), writer, reader, lastSeq, event)
			if err != nil {
				return
			}
			if liveTerminal {
				return
			}
		case <-ticker.C:
			var replayTerminal bool
			lastSeq, replayTerminal, err = handler.replayEvents(r.Context(), writer, reader, lastSeq)
			if err != nil {
				writer.EmitError(err)
				return
			}
			if replayTerminal {
				return
			}
			writer.Keepalive()
		}
	}
}

func (handler *Handler) emitLiveEvent(ctx context.Context, writer *eventWriter, reader *delivery.RunEventReader, lastSeq int64, event delivery.RunEvent) (int64, bool, error) {
	return sse.EmitLive(ctx, reader, lastSeq, event, runEventReplayOptions(writer))
}

func (handler *Handler) replayEvents(ctx context.Context, writer *eventWriter, reader *delivery.RunEventReader, afterSeq int64) (int64, bool, error) {
	return sse.Replay(ctx, reader, afterSeq, runEventReplayOptions(writer))
}

func runEventReplayOptions(writer *eventWriter) sse.ReplayOptions[delivery.RunEvent] {
	return sse.ReplayOptions[delivery.RunEvent]{
		PageSize: eventReplayPage,
		Sequence: func(event delivery.RunEvent) int64 { return event.Seq },
		Terminal: func(event delivery.RunEvent) bool { return terminalEvent(event.Kind) },
		Emit: func(event delivery.RunEvent) error {
			return writer.Emit(event.Seq, string(event.Kind), event)
		},
	}
}

func eventCursor(r *http.Request) (int64, error) {
	return sse.Cursor(r, true)
}

type eventWriter = sse.Writer

func newEventWriter(w http.ResponseWriter) (*eventWriter, error) {
	return sse.New(w)
}

func terminalEvent(kind delivery.EventKind) bool {
	switch kind {
	case delivery.EventRunFailed, delivery.EventRunCancelled,
		delivery.EventRunInterrupted, delivery.EventRunSucceeded:
		return true
	default:
		return false
	}
}
