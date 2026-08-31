package llm

import (
	"context"
	"errors"
	"time"
)

const (
	defaultMaxAttempts = 3
	defaultBackoff     = 500 * time.Millisecond
	maxBackoff         = 8 * time.Second
)

// CallOptions tunes one non-streaming LLM call. JSON-specific fields apply
// only to ChatJSON; the zero value is usable.
type CallOptions struct {
	MaxTokens             int
	MaxAttempts           int
	Backoff               time.Duration
	RepairAttempts        int
	DisallowUnknownFields bool
	Validate              func(parsed any) error
}

func (o CallOptions) maxAttempts() int {
	if o.MaxAttempts <= 0 {
		return defaultMaxAttempts
	}
	return o.MaxAttempts
}

func (o CallOptions) backoff() time.Duration {
	if o.Backoff <= 0 {
		return defaultBackoff
	}
	return o.Backoff
}

// chatCaller is the seam over the transport. Production wires lc.chatMessages;
// tests inject a fake that returns scripted answers or *CallError values.
type chatCaller func(ctx context.Context, msgs []Message, maxTokens int) (string, error)

// sleepFor honors a server-advised Retry-After over exponential backoff.
func sleepFor(ctx context.Context, ce *CallError, backoff time.Duration) bool {
	return sleepCtx(ctx, retryDelay(ce, backoff))
}

func retryDelay(ce *CallError, backoff time.Duration) time.Duration {
	if ce != nil && ce.RetryAfter > 0 {
		return ce.RetryAfter
	}
	return backoff
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func doubleBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoff {
		return maxBackoff
	}
	return d
}

func callErrorDetails(err error) (kind string, status int, retryable bool, ce *CallError) {
	if !errors.As(err, &ce) {
		return "other", 0, false, nil
	}
	switch ce.Kind {
	case ErrKindNetwork:
		kind = "network"
	case ErrKindStatus:
		kind = "status"
	case ErrKindEmpty:
		kind = "empty"
	case ErrKindEnvelope:
		kind = "envelope"
	default:
		kind = "unknown"
	}
	return kind, ce.Status, ce.Retryable(), ce
}
