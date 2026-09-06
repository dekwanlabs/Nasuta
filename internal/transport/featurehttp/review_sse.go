package featurehttp

import (
	"context"
	"net/http"
	"time"

	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	"github.com/dekwanlabs/nasuta/internal/transport/sse"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

const (
	reviewEventReplayPage     = 500
	reviewEventReplayInterval = 2 * time.Second
)

func (handler *Handler) StreamReviewEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	afterSeq, err := eventCursor(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, delivery.ErrUnavailable)
		return
	}
	round, reader, err := handler.service.OpenReviewEvents(
		r.Context(), r.PathValue("round_id"), user.ID, user.IsAdmin,
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
	lastSeq, terminal, err := handler.replayReviewEvents(
		r.Context(), writer, reader, afterSeq,
	)
	if err != nil {
		writer.EmitError(err)
		return
	}
	if terminal || terminalReviewRoundStatus(round.Status) {
		return
	}

	live, unsubscribe, err := reader.Subscribe()
	if err != nil {
		writer.EmitError(err)
		return
	}
	defer unsubscribe()

	lastSeq, terminal, err = handler.replayReviewEvents(
		r.Context(), writer, reader, lastSeq,
	)
	if err != nil {
		writer.EmitError(err)
		return
	}
	if terminal {
		return
	}

	ticker := time.NewTicker(reviewEventReplayInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event, open := <-live:
			if !open {
				writer.EmitError(delivery.ErrUnavailable)
				return
			}
			lastSeq, terminal, err = handler.emitLiveReviewEvent(
				r.Context(), writer, reader, lastSeq, event,
			)
			if err != nil {
				return
			}
			if terminal {
				return
			}
		case <-ticker.C:
			lastSeq, terminal, err = handler.replayReviewEvents(
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

func (handler *Handler) emitLiveReviewEvent(
	ctx context.Context,
	writer *eventWriter,
	reader *delivery.ReviewEventReader,
	lastSeq int64,
	event delivery.ReviewEvent,
) (int64, bool, error) {
	return sse.EmitLive(ctx, reader, lastSeq, event, reviewEventReplayOptions(writer))
}

func (handler *Handler) replayReviewEvents(
	ctx context.Context,
	writer *eventWriter,
	reader *delivery.ReviewEventReader,
	afterSeq int64,
) (int64, bool, error) {
	return sse.Replay(ctx, reader, afterSeq, reviewEventReplayOptions(writer))
}

func reviewEventReplayOptions(writer *eventWriter) sse.ReplayOptions[delivery.ReviewEvent] {
	return sse.ReplayOptions[delivery.ReviewEvent]{
		PageSize: reviewEventReplayPage,
		Sequence: func(event delivery.ReviewEvent) int64 { return event.Seq },
		Terminal: func(event delivery.ReviewEvent) bool { return terminalReviewEvent(event.Kind) },
		Emit: func(event delivery.ReviewEvent) error {
			return writer.Emit(event.Seq, string(event.Kind), event)
		},
	}
}

func terminalReviewEvent(kind delivery.ReviewEventKind) bool {
	switch kind {
	case delivery.ReviewEventRoundCompleted,
		delivery.ReviewEventRoundFailed,
		delivery.ReviewEventRoundCancelled:
		return true
	default:
		return false
	}
}

func terminalReviewRoundStatus(status delivery.ReviewRoundStatus) bool {
	switch status {
	case delivery.RoundCompleted,
		delivery.RoundFailed,
		delivery.RoundCancelled:
		return true
	default:
		return false
	}
}
