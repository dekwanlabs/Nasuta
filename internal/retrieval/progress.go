package retrieval

import (
	"context"
	"time"
)

// ProgressEvent is a low-cardinality retrieval phase update.
type ProgressEvent struct {
	Code      string
	Text      string
	ElapsedMS int64
}

type progressFunc func(ProgressEvent)

type progressContextKey struct{}

// WithProgress attaches an optional retrieval progress sink to one request.
func WithProgress(ctx context.Context, emit func(ProgressEvent)) context.Context {
	if emit == nil {
		return ctx
	}
	return context.WithValue(ctx, progressContextKey{}, progressFunc(emit))
}

func reportProgress(ctx context.Context, code, text string, started time.Time) {
	emit, _ := ctx.Value(progressContextKey{}).(progressFunc)
	if emit == nil {
		return
	}
	elapsed := int64(0)
	if !started.IsZero() {
		elapsed = time.Since(started).Milliseconds()
	}
	emit(ProgressEvent{Code: code, Text: text, ElapsedMS: elapsed})
}
