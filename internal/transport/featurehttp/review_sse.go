package featurehttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
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
		writeDomainError(w, featuredelivery.ErrUnavailable)
		return
	}
	round, reader, err := handler.service.OpenReviewEvents(
		r.Context(), r.PathValue("round_id"), user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writer, err := newReviewEventWriter(w)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	lastSeq, terminal, err := handler.replayReviewEvents(
		r.Context(), writer, reader, afterSeq,
	)
	if err != nil {
		writer.emitError(err)
		return
	}
	if terminal || terminalReviewRoundStatus(round.Status) {
		return
	}

	live, unsubscribe, err := reader.Subscribe()
	if err != nil {
		writer.emitError(err)
		return
	}
	defer unsubscribe()

	lastSeq, terminal, err = handler.replayReviewEvents(
		r.Context(), writer, reader, lastSeq,
	)
	if err != nil {
		writer.emitError(err)
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
				writer.emitError(featuredelivery.ErrUnavailable)
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
				writer.emitError(err)
				return
			}
			if terminal {
				return
			}
			writer.keepalive()
		}
	}
}

func (handler *Handler) emitLiveReviewEvent(
	ctx context.Context,
	writer *reviewEventWriter,
	reader *featuredelivery.ReviewEventReader,
	lastSeq int64,
	event featuredelivery.ReviewEvent,
) (int64, bool, error) {
	if event.Seq <= lastSeq {
		return lastSeq, false, nil
	}
	if event.Seq > lastSeq+1 {
		var terminal bool
		var err error
		lastSeq, terminal, err = handler.replayReviewEvents(
			ctx, writer, reader, lastSeq,
		)
		if err != nil || terminal || event.Seq <= lastSeq {
			return lastSeq, terminal, err
		}
	}
	if err := writer.emit(event); err != nil {
		return lastSeq, false, err
	}
	return event.Seq, terminalReviewEvent(event.Kind), nil
}

func (handler *Handler) replayReviewEvents(
	ctx context.Context,
	writer *reviewEventWriter,
	reader *featuredelivery.ReviewEventReader,
	afterSeq int64,
) (int64, bool, error) {
	lastSeq := afterSeq
	for {
		events, err := reader.List(ctx, lastSeq, reviewEventReplayPage)
		if err != nil {
			return lastSeq, false, err
		}
		for _, event := range events {
			if event.Seq <= lastSeq {
				continue
			}
			if err := writer.emit(event); err != nil {
				return lastSeq, false, err
			}
			lastSeq = event.Seq
			if terminalReviewEvent(event.Kind) {
				return lastSeq, true, nil
			}
		}
		if len(events) < reviewEventReplayPage {
			return lastSeq, false, nil
		}
	}
}

type reviewEventWriter struct {
	writer  http.ResponseWriter
	flusher http.Flusher
}

func newReviewEventWriter(w http.ResponseWriter) (*reviewEventWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &reviewEventWriter{writer: w, flusher: flusher}, nil
}

func (writer *reviewEventWriter) emit(event featuredelivery.ReviewEvent) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		writer.writer,
		"id: %d\nevent: %s\ndata: %s\n\n",
		event.Seq,
		event.Kind,
		raw,
	); err != nil {
		return err
	}
	writer.flusher.Flush()
	return nil
}

func (writer *reviewEventWriter) emitError(err error) {
	raw, _ := json.Marshal(map[string]string{"error": err.Error()})
	_, _ = fmt.Fprintf(writer.writer, "event: error\ndata: %s\n\n", raw)
	writer.flusher.Flush()
}

func (writer *reviewEventWriter) keepalive() {
	_, _ = fmt.Fprint(writer.writer, ": keepalive\n\n")
	writer.flusher.Flush()
}

func terminalReviewEvent(kind featuredelivery.ReviewEventKind) bool {
	switch kind {
	case featuredelivery.ReviewEventRoundCompleted,
		featuredelivery.ReviewEventRoundFailed,
		featuredelivery.ReviewEventRoundCancelled:
		return true
	default:
		return false
	}
}

func terminalReviewRoundStatus(status featuredelivery.ReviewRoundStatus) bool {
	switch status {
	case featuredelivery.RoundCompleted,
		featuredelivery.RoundFailed,
		featuredelivery.RoundCancelled:
		return true
	default:
		return false
	}
}
