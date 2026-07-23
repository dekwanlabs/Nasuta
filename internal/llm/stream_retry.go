package llm

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
)

// postStream retries setup failures only; retrying after token delivery would
// duplicate deltas already emitted to the caller.
func postStream(ctx context.Context, do func() (*resty.Response, error), onRetry func(time.Duration, error)) (*resty.Response, error) {
	backoff := defaultBackoff
	for attempt := 1; attempt <= defaultMaxAttempts; attempt++ {
		started := time.Now()
		resp, err := do()
		duration := time.Since(started)
		if err == nil && resp.StatusCode() == http.StatusOK {
			return resp, nil
		}
		var ce *CallError
		if err != nil {
			ce = &CallError{Kind: ErrKindNetwork, Err: err}
		} else {
			body := readErrorBody(resp.RawBody())
			resp.RawBody().Close()
			ce = &CallError{
				Kind:       ErrKindStatus,
				Status:     resp.StatusCode(),
				Body:       body,
				RetryAfter: parseRetryAfter(resp.Header(), maxBackoff),
			}
		}
		if !ce.Retryable() {
			return nil, ce
		}
		if attempt == defaultMaxAttempts {
			return nil, fmt.Errorf("%w: %w", ErrMaxAttempts, ce)
		}
		if !sleepFor(ctx, ce, backoff) {
			return nil, ctx.Err()
		}
		if onRetry != nil {
			onRetry(duration, ce)
		}
		backoff = doubleBackoff(backoff)
	}
	return nil, ErrMaxAttempts
}
