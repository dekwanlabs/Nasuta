package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCallErrorRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  *CallError
		want bool
	}{
		{"network", &CallError{Kind: ErrKindNetwork}, true},
		{"429", &CallError{Kind: ErrKindStatus, Status: 429}, true},
		{"500", &CallError{Kind: ErrKindStatus, Status: 500}, true},
		{"503", &CallError{Kind: ErrKindStatus, Status: 503}, true},
		{"400", &CallError{Kind: ErrKindStatus, Status: 400}, false},
		{"401", &CallError{Kind: ErrKindStatus, Status: 401}, false},
		{"404", &CallError{Kind: ErrKindStatus, Status: 404}, false},
		{"empty", &CallError{Kind: ErrKindEmpty}, false},
		{"envelope", &CallError{Kind: ErrKindEnvelope}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.err.Retryable(); got != c.want {
				t.Fatalf("Retryable=%t want %t", got, c.want)
			}
		})
	}
}

func TestErrorBodyIsBounded(t *testing.T) {
	body := strings.Repeat("x", maxErrorBodyBytes+100)
	got := readErrorBody(strings.NewReader(body))
	if len(got) > maxErrorBodyBytes+len("…") {
		t.Fatalf("bounded body length = %d", len(got))
	}
}

// A network error caused by a dead parent context must not retry - retrying a
// cancelled/deadline-exceeded call just fails again.
func TestCallErrorDeadContextNotRetryable(t *testing.T) {
	net := &CallError{Kind: ErrKindNetwork, Err: context.DeadlineExceeded}
	if net.Retryable() {
		t.Fatal("deadline-exceeded network error should not retry")
	}
	status := &CallError{Kind: ErrKindStatus, Status: 503, Err: context.Canceled}
	if status.Retryable() {
		t.Fatal("canceled 503 should not retry")
	}
}

func TestCallErrorAsAndUnwrap(t *testing.T) {
	inner := errors.New("boom")
	ce := &CallError{Kind: ErrKindEnvelope, Err: inner}
	if !errors.Is(ce, inner) {
		t.Fatal("errors.Is should reach wrapped inner error")
	}
	var target *CallError
	if !errors.As(ce, &target) {
		t.Fatal("errors.As should match *CallError")
	}
	if target.Kind != ErrKindEnvelope {
		t.Fatalf("kind=%v want ErrKindEnvelope", target.Kind)
	}
}
