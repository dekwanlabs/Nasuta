package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxErrorBodyBytes = 64 << 10

// ErrorKind classifies one LLM call failure so retry policy can decide.
type ErrorKind int

const (
	// ErrKindNetwork is a transport-level failure (connection, read timeout, EOF).
	ErrKindNetwork ErrorKind = iota
	// ErrKindStatus is a non-2xx HTTP response.
	ErrKindStatus
	// ErrKindEmpty means the response parsed but carried no choices.
	ErrKindEmpty
	// ErrKindEnvelope means the response body could not be decoded as a chat envelope.
	ErrKindEnvelope
)

// CallError classifies one LLM call failure. Callers that only need "did it
// work" treat it as error; retry-aware callers inspect Retryable().
type CallError struct {
	Kind       ErrorKind
	Status     int           // HTTP status when Kind == ErrKindStatus
	Body       string        // response body excerpt for logging
	Err        error         // underlying error when wrapping a transport/parse failure
	RetryAfter time.Duration // server-advised backoff (Retry-After header), 0 if none
}

func (e *CallError) Error() string {
	switch e.Kind {
	case ErrKindStatus:
		return fmt.Sprintf("LLM API %d: %s", e.Status, strings.TrimSpace(e.Body))
	case ErrKindNetwork:
		if e.Err != nil {
			return fmt.Sprintf("LLM network: %v", e.Err)
		}
		return "LLM network error"
	case ErrKindEmpty:
		return "empty LLM response"
	case ErrKindEnvelope:
		if e.Err != nil {
			return fmt.Sprintf("LLM response parse: %v", e.Err)
		}
		return "LLM response parse error"
	}
	return "LLM error"
}

func (e *CallError) Unwrap() error { return e.Err }

// Retryable reports whether a transport retry with backoff is sensible. A dead
// parent context never is - retrying a cancelled/deadline-exceeded call just
// fails again. Non-2xx is retryable only for 429 and 5xx.
func (e *CallError) Retryable() bool {
	if e.Err != nil && (errors.Is(e.Err, context.Canceled) || errors.Is(e.Err, context.DeadlineExceeded)) {
		return false
	}
	switch e.Kind {
	case ErrKindNetwork:
		return true
	case ErrKindStatus:
		return e.Status == 429 || e.Status >= 500
	default:
		return false
	}
}

// Sentinel errors for structured-call outcomes after retry/repair are exhausted.
var (
	// ErrInvalidJSON means the model output could not be parsed or validated
	// even after programmatic repair and reprompt.
	ErrInvalidJSON = errors.New("llm: invalid JSON after repair and reprompt")
	// ErrMaxAttempts means the call exhausted its transport retry budget.
	ErrMaxAttempts = errors.New("llm: call exhausted retry attempts")
)

func boundedErrorBody(body []byte) string {
	if len(body) <= maxErrorBodyBytes {
		return string(body)
	}
	return string(body[:maxErrorBodyBytes]) + "…"
}

func readErrorBody(reader io.Reader) string {
	if reader == nil {
		return ""
	}
	body, _ := io.ReadAll(io.LimitReader(reader, maxErrorBodyBytes+1))
	return boundedErrorBody(body)
}

// parseRetryAfter returns the delay indicated by an HTTP Retry-After header, or
// zero if absent/unparseable. Accepts integer seconds or an HTTP-date, capped at
// the retry ceiling so a hostile header cannot stall a call indefinitely.
func parseRetryAfter(header http.Header, cap time.Duration) time.Duration {
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		d := time.Duration(secs) * time.Second
		if d > cap {
			return cap
		}
		return d
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			if d > cap {
				return cap
			}
			return d
		}
	}
	return 0
}
