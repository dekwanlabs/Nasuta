// Package sse contains the transport-level mechanics shared by event streams.
package sse

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Writer writes server-sent events and flushes each frame immediately.
type Writer struct {
	writer  http.ResponseWriter
	flusher http.Flusher
}

// New prepares an HTTP response for an SSE stream.
func New(w http.ResponseWriter) (*Writer, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("streaming not supported")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &Writer{writer: w, flusher: flusher}, nil
}

// Emit writes one event frame using the supplied sequence and event name.
func (writer *Writer) Emit(seq int64, kind string, event any) error {
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		writer.writer,
		"id: %d\nevent: %s\ndata: %s\n\n",
		seq,
		kind,
		raw,
	); err != nil {
		return err
	}
	writer.flusher.Flush()
	return nil
}

// EmitError writes a non-sequenced error event to the stream.
func (writer *Writer) EmitError(err error) {
	raw, _ := json.Marshal(map[string]string{"error": err.Error()})
	_, _ = fmt.Fprintf(writer.writer, "event: error\ndata: %s\n\n", raw)
	writer.flusher.Flush()
}

// Keepalive writes an SSE comment so intermediaries keep the connection open.
func (writer *Writer) Keepalive() {
	_, _ = fmt.Fprint(writer.writer, ": keepalive\n\n")
	writer.flusher.Flush()
}
