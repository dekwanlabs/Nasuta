package llm

import (
	"context"
	"errors"
	"time"
)

type fakeCaller struct {
	answers []string
	errs    []error
	calls   int
	seen    [][]Message
}

func (fake *fakeCaller) call(_ context.Context, msgs []Message, _ int) (string, error) {
	fake.seen = append(fake.seen, append([]Message(nil), msgs...))
	index := fake.calls
	fake.calls++
	if index < len(fake.errs) && fake.errs[index] != nil {
		return "", fake.errs[index]
	}
	if index < len(fake.answers) {
		return fake.answers[index], nil
	}
	return "", errors.New("fake caller exhausted")
}

func fastOpts() CallOptions {
	return CallOptions{Backoff: time.Millisecond, MaxAttempts: 3}
}
