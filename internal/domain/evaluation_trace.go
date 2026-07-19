package domain

import "context"

// EvaluationTrace is a request-scoped, read-only execution event.
type EvaluationTrace struct {
	Sequence   int            `json:"sequence,omitempty"`
	Node       string         `json:"node"`
	Status     string         `json:"status"`
	ElapsedMS  int64          `json:"elapsed_ms,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
	Output     map[string]any `json:"output,omitempty"`
}

// TraceRecorder accepts events only when an ingress explicitly enables tracing.
type TraceRecorder interface {
	RecordTrace(EvaluationTrace)
}

type traceRecorderKey struct{}

// WithTraceRecorder attaches one request-local trace sink.
func WithTraceRecorder(ctx context.Context, recorder TraceRecorder) context.Context {
	if recorder == nil {
		return ctx
	}
	return context.WithValue(ctx, traceRecorderKey{}, recorder)
}

// TraceEnabled lets hot paths avoid constructing trace payloads for normal requests.
func TraceEnabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	recorder, _ := ctx.Value(traceRecorderKey{}).(TraceRecorder)
	return recorder != nil
}

// RecordTrace is a no-op for normal product requests.
func RecordTrace(ctx context.Context, event EvaluationTrace) {
	if ctx == nil {
		return
	}
	recorder, _ := ctx.Value(traceRecorderKey{}).(TraceRecorder)
	if recorder == nil {
		return
	}
	if event.Status == "" {
		event.Status = "completed"
	}
	recorder.RecordTrace(event)
}
