package tool

import (
	"context"
	"testing"
	"time"
)

func TestExecutorAppliesDefaultTimeout(t *testing.T) {
	registry := NewRegistry()
	candidate := testTool("slow", "ok")
	candidate.Handler = HandlerFunc(func(ctx context.Context, _ Arguments) (Result, error) {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return Result{Content: "ok"}, nil
		}
	})
	if err := registry.Register(candidate); err != nil {
		t.Fatal(err)
	}
	_, err := NewExecutor(20 * time.Millisecond).Execute(
		context.Background(),
		registry.Snapshot(ReadPolicy()),
		"slow",
		Arguments{},
	)
	if err == nil {
		t.Fatal("expected default timeout")
	}
}

func TestExecutorInheritsCallerDeadline(t *testing.T) {
	registry := NewRegistry()
	candidate := testTool("nested", "ok")
	candidate.Timeout = InheritCallerDeadline
	started := make(chan struct{})
	candidate.Handler = HandlerFunc(func(ctx context.Context, _ Arguments) (Result, error) {
		close(started)
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(80 * time.Millisecond):
			return Result{Content: "ok"}, nil
		}
	})
	if err := registry.Register(candidate); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result, err := NewExecutor(20 * time.Millisecond).Execute(
		ctx,
		registry.Snapshot(ReadPolicy()),
		"nested",
		Arguments{},
	)
	if err != nil || result.Content != "ok" {
		t.Fatalf("result = %#v, err = %v", result, err)
	}
	select {
	case <-started:
	default:
		t.Fatal("handler did not start")
	}
}

func TestExecutorToolTimeoutOverridesDefault(t *testing.T) {
	registry := NewRegistry()
	candidate := testTool("override", "ok")
	candidate.Timeout = 20 * time.Millisecond
	candidate.Handler = HandlerFunc(func(ctx context.Context, _ Arguments) (Result, error) {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return Result{Content: "ok"}, nil
		}
	})
	if err := registry.Register(candidate); err != nil {
		t.Fatal(err)
	}
	_, err := NewExecutor(time.Second).Execute(
		context.Background(),
		registry.Snapshot(ReadPolicy()),
		"override",
		Arguments{},
	)
	if err == nil {
		t.Fatal("expected tool timeout")
	}
}
