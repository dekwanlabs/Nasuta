package llm

import (
	"context"
	"fmt"
)

// ChatText calls the model for a free-form answer with transport retry only.
func (lc *LLMClient) ChatText(ctx context.Context, system, user string, opts CallOptions) (string, error) {
	return chatTextWith(ctx, lc.chatMessages, system, user, opts)
}

func chatTextWith(ctx context.Context, call chatCaller, system, user string, opts CallOptions) (string, error) {
	maxAttempts := opts.maxAttempts()
	backoff := opts.backoff()
	msgs := []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err := call(ctx, msgs, opts.MaxTokens)
		if err == nil {
			return raw, nil
		}
		ce, retryable := retryableCallError(err)
		if !retryable {
			return "", err
		}
		if attempt == maxAttempts {
			return "", fmt.Errorf("%w: %w", ErrMaxAttempts, err)
		}
		if !sleepFor(ctx, ce, backoff) {
			return "", ctx.Err()
		}
		backoff = doubleBackoff(backoff)
	}
	return "", ErrMaxAttempts
}
