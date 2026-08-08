package workflowhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
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
		writer.emitError(err)
		return
	}
	if terminal || terminalRunStatus(run.Status) {
		return
	}

	live, unsubscribe, err := handler.service.SubscribeEvents(runID)
	if err != nil {
		writer.emitError(err)
		return
	}
	defer unsubscribe()

	lastSeq, terminal, err = handler.replayEvents(
		r.Context(), writer, reader, lastSeq,
	)
	if err != nil {
		writer.emitError(err)
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
				writer.emitError(workflow.ErrUnavailable)
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

func (handler *Handler) emitLiveEvent(
	ctx context.Context,
	writer *eventWriter,
	reader eventReader,
	lastSeq int64,
	event workflow.Event,
) (int64, bool, error) {
	if event.Seq <= lastSeq {
		return lastSeq, false, nil
	}
	if event.Seq > lastSeq+1 {
		var terminal bool
		var err error
		lastSeq, terminal, err = handler.replayEvents(
			ctx, writer, reader, lastSeq,
		)
		if err != nil || terminal || event.Seq <= lastSeq {
			return lastSeq, terminal, err
		}
	}
	if err := writer.emit(event); err != nil {
		return lastSeq, false, err
	}
	return event.Seq, terminalEvent(event.Kind), nil
}

func (handler *Handler) replayEvents(
	ctx context.Context,
	writer *eventWriter,
	reader eventReader,
	afterSeq int64,
) (int64, bool, error) {
	lastSeq := afterSeq
	for {
		events, err := reader.List(ctx, lastSeq, eventReplayPage)
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

func (writer *eventWriter) emit(event workflow.Event) error {
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

func (writer *eventWriter) emitError(err error) {
	raw, _ := json.Marshal(map[string]string{"error": err.Error()})
	_, _ = fmt.Fprintf(writer.writer, "event: error\ndata: %s\n\n", raw)
	writer.flusher.Flush()
}

func (writer *eventWriter) keepalive() {
	_, _ = fmt.Fprint(writer.writer, ": keepalive\n\n")
	writer.flusher.Flush()
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
