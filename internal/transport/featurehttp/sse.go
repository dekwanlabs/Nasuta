package featurehttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

const eventReplayPage = 500

func (handler *Handler) RunEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	runID := r.PathValue("run_id")
	run, err := handler.service.GetImplementation(r.Context(), runID, user.ID, user.IsAdmin)
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
	lastSeq, terminal, err := handler.replayEvents(r.Context(), writer, runID, afterSeq)
	if err != nil {
		writer.emitError(err)
		return
	}
	if terminal || featuredelivery.IsTerminalRun(run.Status) {
		return
	}

	live, unsubscribe, err := handler.service.SubscribeRun(runID)
	if err != nil {
		writer.emitError(err)
		return
	}
	defer unsubscribe()

	lastSeq, terminal, err = handler.replayEvents(r.Context(), writer, runID, lastSeq)
	if err != nil {
		writer.emitError(err)
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
			if event.Seq <= lastSeq {
				continue
			}
			if err := writer.emit(event); err != nil {
				return
			}
			lastSeq = event.Seq
			if terminalEvent(event.Kind) {
				return
			}
		case <-ticker.C:
			var replayTerminal bool
			lastSeq, replayTerminal, err = handler.replayEvents(r.Context(), writer, runID, lastSeq)
			if err != nil {
				writer.emitError(err)
				return
			}
			if replayTerminal {
				return
			}
			writer.keepalive()
		}
	}
}

func (handler *Handler) replayEvents(ctx context.Context, writer *eventWriter, runID string, afterSeq int64) (int64, bool, error) {
	lastSeq := afterSeq
	for {
		events, err := handler.service.ListRunEvents(ctx, runID, lastSeq, eventReplayPage, 0, true)
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
			if terminalEvent(event.Kind) {
				return lastSeq, true, nil
			}
		}
		if len(events) < eventReplayPage {
			return lastSeq, false, nil
		}
	}
}

func eventCursor(r *http.Request) (int64, error) {
	value := strings.TrimSpace(r.URL.Query().Get("after_seq"))
	if value == "" {
		value = strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	}
	if value == "" {
		return 0, nil
	}
	seq, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seq < 0 {
		return 0, fmt.Errorf("after_seq must be a non-negative integer")
	}
	return seq, nil
}

type eventWriter struct {
	writer  http.ResponseWriter
	flusher http.Flusher
}

func newEventWriter(w http.ResponseWriter) (*eventWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &eventWriter{writer: w, flusher: flusher}, nil
}

func (writer *eventWriter) emit(event featuredelivery.RunEvent) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer.writer, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Kind, raw); err != nil {
		return err
	}
	writer.flusher.Flush()
	return nil
}

func (writer *eventWriter) emitError(err error) {
	raw, _ := json.Marshal(map[string]string{"error": err.Error()})
	_, _ = fmt.Fprintf(writer.writer, "event: error\ndata: %s\n\n", raw)
	writer.flusher.Flush()
}

func (writer *eventWriter) keepalive() {
	_, _ = fmt.Fprint(writer.writer, ": keepalive\n\n")
	writer.flusher.Flush()
}

func terminalEvent(kind featuredelivery.EventKind) bool {
	switch kind {
	case featuredelivery.EventRunFailed, featuredelivery.EventRunCancelled,
		featuredelivery.EventRunInterrupted, featuredelivery.EventRunSucceeded:
		return true
	default:
		return false
	}
}
