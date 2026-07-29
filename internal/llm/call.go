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
	d := backoff
	if ce.RetryAfter > 0 {
		d = ce.RetryAfter
	}
	return sleepCtx(ctx, d)
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

func retryableCallError(err error) (*CallError, bool) {
	var ce *CallError
	if !errors.As(err, &ce) || !ce.Retryable() {
		return nil, false
	}
	return ce, true
}
