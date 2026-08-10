package llm

import (
	"context"
	"fmt"
	"time"
)

// ChatText calls the model for a free-form answer with transport retry only.
func (lc *LLMClient) ChatText(ctx context.Context, system, user string, opts CallOptions) (string, error) {
	return chatTextWith(ctx, lc.chatMessages, system, user, opts)
}

func chatTextWith(ctx context.Context, call chatCaller, system, user string, opts CallOptions) (string, error) {
	maxAttempts := opts.maxAttempts()
	backoff := opts.backoff()
	logicalCallSeq := beginLogicalCall(ctx)
	msgs := []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		started := time.Now()
		raw, err := call(ctx, msgs, opts.MaxTokens)
		if err == nil {
			observeAttempt(ctx, Attempt{
				LogicalCallSeq: logicalCallSeq, Kind: "transport", Attempt: attempt,
				MaxAttempts: maxAttempts, Duration: time.Since(started), Outcome: "succeeded",
			})
			return raw, nil
		}
		kind, status, retryable, ce := callErrorDetails(err)
		retryScheduled := retryable && attempt < maxAttempts
		delay := time.Duration(0)
		if retryScheduled {
			delay = retryDelay(ce, backoff)
		}
		observeAttempt(ctx, Attempt{
			LogicalCallSeq: logicalCallSeq, Kind: "transport", Attempt: attempt,
			MaxAttempts: maxAttempts, Duration: time.Since(started), ErrorKind: kind,
			StatusCode: status, Retryable: retryable, RetryScheduled: retryScheduled,
			Backoff: delay, Outcome: "failed",
		})
		if !retryScheduled {
			if retryable && attempt == maxAttempts {
				return "", fmt.Errorf("%w: %w", ErrMaxAttempts, err)
			}
			return "", err
		}
		if !sleepCtx(ctx, delay) {
			return "", ctx.Err()
		}
		backoff = doubleBackoff(backoff)
	}
	return "", ErrMaxAttempts
}
