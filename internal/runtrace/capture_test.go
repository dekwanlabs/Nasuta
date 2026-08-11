package runtrace

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestCaptureOwnsRequestedScopeAndReturnsSnapshot(t *testing.T) {
	var captured *Scope
	output, events, err := Capture(
		WithEvaluation(t.Context(), nil),
		Correlation{RunID: "run-1"},
		func(ctx context.Context) (string, error) {
			captured = FromContext(ctx)
			domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "captured"})
			return "ok", nil
		},
	)
	if err != nil || output != "ok" {
		t.Fatalf("output = %q, err = %v", output, err)
	}
	if captured == nil || captured.Enabled() {
		t.Fatal("owned scope was not attached and closed")
	}
	if len(events) != 1 || events[0].Node != "captured" || events[0].RunID != "run-1" {
		t.Fatalf("events = %#v", events)
	}
}

func TestCaptureSkipsScopeForOrdinaryExecution(t *testing.T) {
	output, events, err := Capture(
		t.Context(),
		Correlation{},
		func(ctx context.Context) (string, error) {
			if FromContext(ctx) != nil || domain.TraceEnabled(ctx) {
				t.Fatal("ordinary execution received a trace scope")
			}
			return "ok", nil
		},
	)
	if err != nil || output != "ok" || events != nil {
		t.Fatalf("output = %q, events = %#v, err = %v", output, events, err)
	}
}

func TestCaptureDoesNotCloseInheritedScope(t *testing.T) {
	scope := NewScope(Evaluation, nil)
	ctx := WithScope(t.Context(), scope)
	_, events, err := Capture(ctx, Correlation{AgentRunID: "agent-1"}, func(ctx context.Context) (struct{}, error) {
		domain.RecordTrace(ctx, domain.EvaluationTrace{Node: "child"})
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Enabled() {
		t.Fatal("capture closed an inherited scope")
	}
	if len(events) != 1 || events[0].AgentRunID != "agent-1" {
		t.Fatalf("events = %#v", events)
	}
}
