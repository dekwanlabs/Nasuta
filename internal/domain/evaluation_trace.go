package domain

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/tracecontract"
)

// EvaluationTrace is a request-scoped, read-only execution event.
type EvaluationTrace = tracecontract.EventV1

// TraceRecorder accepts events only when an ingress explicitly enables tracing.
type TraceRecorder interface {
	RecordTrace(EvaluationTrace)
}

type traceRecorderState interface {
	Enabled() bool
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
	if recorder == nil {
		return false
	}
	if state, ok := recorder.(traceRecorderState); ok {
		return state.Enabled()
	}
	return true
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
