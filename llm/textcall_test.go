package llm

import (
	"context"
	"errors"
	"testing"
)

func TestChatTextRetriesTransportFailure(t *testing.T) {
	fake := &fakeCaller{
		errs:    []error{&CallError{Kind: ErrKindNetwork}},
		answers: []string{"", "answer"},
	}
	answer, err := chatTextWith(context.Background(), fake.call, "system", "user", fastOpts())
	if err != nil {
		t.Fatalf("chatTextWith: %v", err)
	}
	if answer != "answer" || fake.calls != 2 {
		t.Fatalf("answer = %q calls = %d, want retry then answer", answer, fake.calls)
	}
}

func TestChatTextReturnsMaxAttemptsWhenRetriesExhausted(t *testing.T) {
	fake := &fakeCaller{errs: []error{
		&CallError{Kind: ErrKindNetwork},
		&CallError{Kind: ErrKindNetwork},
		&CallError{Kind: ErrKindNetwork},
	}}
	_, err := chatTextWith(context.Background(), fake.call, "system", "user", fastOpts())
	if !errors.Is(err, ErrMaxAttempts) {
		t.Fatalf("want ErrMaxAttempts, got %v", err)
	}
}
