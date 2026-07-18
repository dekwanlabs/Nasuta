package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestChatJSONFirstTryOK(t *testing.T) {
	f := &fakeCaller{answers: []string{`{"a":1}`}}
	var out map[string]any
	if err := chatJSONWith(context.Background(), f.call, "s", "u", &out, fastOpts()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["a"] != float64(1) || f.calls != 1 {
		t.Fatalf("out=%v calls=%d", out, f.calls)
	}
}

// Malformed JSON is repaired within one call - no reprompt.
func TestChatJSONRepairNoReprompt(t *testing.T) {
	f := &fakeCaller{answers: []string{`{"a":1,}`} /* trailing comma */}
	var out map[string]any
	if err := chatJSONWith(context.Background(), f.call, "s", "u", &out, fastOpts()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["a"] != float64(1) || f.calls != 1 {
		t.Fatalf("out=%v calls=%d", out, f.calls)
	}
}

func TestChatJSONRepromptOnUnrepairable(t *testing.T) {
	f := &fakeCaller{answers: []string{`not json at all`, `{"a":2}`}}
	var out map[string]any
	if err := chatJSONWith(context.Background(), f.call, "s", "u", &out, fastOpts()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["a"] != float64(2) || f.calls != 2 {
		t.Fatalf("out=%v calls=%d", out, f.calls)
	}
	// Second call must carry the bad output as an assistant turn plus the repair request.
	second := f.seen[1]
	if len(second) != 4 || second[2].Role != "assistant" || second[2].Content != "not json at all" {
		t.Fatalf("repair conversation wrong: %+v", second)
	}
	if second[3].Role != "user" || !strings.Contains(second[3].Content, "valid JSON") {
		t.Fatalf("repair prompt wrong: %+v", second[3])
	}
}

func TestChatJSONValidateFailureReprompts(t *testing.T) {
	f := &fakeCaller{answers: []string{`{"a":1}`, `{"a":2}`}}
	opts := fastOpts()
	opts.Validate = func(p any) error {
		m := p.(*map[string]any)
		if (*m)["a"] == float64(1) {
			return errors.New("a must not be 1")
		}
		return nil
	}
	var out map[string]any
	if err := chatJSONWith(context.Background(), f.call, "s", "u", &out, opts); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["a"] != float64(2) || f.calls != 2 {
		t.Fatalf("out=%v calls=%d", out, f.calls)
	}
}

func TestChatJSONExhaustedReturnsInvalidJSON(t *testing.T) {
	f := &fakeCaller{answers: []string{`garbage1`, `garbage2`}}
	opts := fastOpts()
	opts.MaxAttempts = 2
	var out map[string]any
	err := chatJSONWith(context.Background(), f.call, "s", "u", &out, opts)
	if !errors.Is(err, ErrInvalidJSON) {
		t.Fatalf("want ErrInvalidJSON, got %v", err)
	}
	if f.calls != 2 {
		t.Fatalf("calls=%d want 2", f.calls)
	}
}

func TestChatJSONNonRetryableErrorNoRetry(t *testing.T) {
	f := &fakeCaller{errs: []error{&CallError{Kind: ErrKindStatus, Status: 400}}}
	var out map[string]any
	err := chatJSONWith(context.Background(), f.call, "s", "u", &out, fastOpts())
	var ce *CallError
	if !errors.As(err, &ce) || ce.Status != 400 {
		t.Fatalf("want 400 CallError, got %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("non-retryable should not retry: calls=%d", f.calls)
	}
}

func TestChatJSONTransportRetryThenOK(t *testing.T) {
	f := &fakeCaller{
		errs:    []error{&CallError{Kind: ErrKindNetwork}},
		answers: []string{"", `{"a":1}`},
	}
	var out map[string]any
	if err := chatJSONWith(context.Background(), f.call, "s", "u", &out, fastOpts()); err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["a"] != float64(1) || f.calls != 2 {
		t.Fatalf("out=%v calls=%d", out, f.calls)
	}
}

// Dead parent context must not retry even on a retryable error.
func TestChatJSONDeadContextNoRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeCaller{errs: []error{&CallError{Kind: ErrKindNetwork, Err: context.Canceled}}}
	var out map[string]any
	_ = chatJSONWith(ctx, f.call, "s", "u", &out, fastOpts())
	if f.calls != 1 {
		t.Fatalf("canceled context should not retry: calls=%d", f.calls)
	}
}

func TestParseRepairValidateFreshParseNoStale(t *testing.T) {
	out := map[string]any{"stale": "leftover"}
	ok, _ := parseRepairValidate(`{"a":1}`, &out, nil)
	if !ok || out["stale"] != nil || out["a"] != float64(1) {
		t.Fatalf("stale data survived: out=%v ok=%v", out, ok)
	}
}

func TestParseRepairValidateRejectsNilPointer(t *testing.T) {
	var out *map[string]any
	if ok, _ := parseRepairValidate(`{"a":1}`, out, nil); ok {
		t.Fatal("typed nil pointer must be rejected")
	}
}
