package executiontrace

import (
	"context"
	"errors"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

type invokeResult struct {
	value    string
	degraded bool
}

func TestInvokeSkipsProjectionWhenTracingIsDisabled(t *testing.T) {
	projected := false
	spec := Spec[string, string]{
		Node: "disabled",
		Input: func(string) map[string]any {
			projected = true
			return nil
		},
		Output: func(string, string, error) map[string]any {
			projected = true
			return nil
		},
		Status: func(string, error) string {
			projected = true
			return "completed"
		},
	}
	result, err := Invoke(t.Context(), spec, "input", func(context.Context, string) (string, error) {
		return "output", nil
	})
	if err != nil || result != "output" || projected {
		t.Fatalf("result = %q, err = %v, projected = %v", result, err, projected)
	}
}

func TestInvokeProjectsDefaultAndBusinessStatuses(t *testing.T) {
	tests := []struct {
		name       string
		result     invokeResult
		err        error
		wantStatus string
	}{
		{name: "completed", result: invokeResult{value: "ok"}, wantStatus: "completed"},
		{name: "failed", err: errors.New("backend unavailable"), wantStatus: "failed"},
		{name: "cancelled", err: context.Canceled, wantStatus: "cancelled"},
		{name: "deadline", err: context.DeadlineExceeded, wantStatus: "cancelled"},
		{name: "degraded", result: invokeResult{value: "fallback", degraded: true}, wantStatus: "degraded"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var events []domain.EvaluationTrace
			ctx := WithScope(t.Context(), NewScope(Evaluation, func(event domain.EvaluationTrace) {
				events = append(events, event)
			}))
			spec := Spec[string, invokeResult]{
				Operation: "test.invoke", Node: "invoke_test",
				Input: func(input string) map[string]any { return map[string]any{"value": input} },
				Output: func(_ string, output invokeResult, err error) map[string]any {
					fields := map[string]any{"value": output.value}
					if err != nil {
						fields["error"] = err.Error()
					}
					return fields
				},
				Status: func(output invokeResult, _ error) string {
					if output.degraded {
						return "degraded"
					}
					return ""
				},
			}
			result, err := Invoke(ctx, spec, "request", func(context.Context, string) (invokeResult, error) {
				return test.result, test.err
			})
			if !errors.Is(err, test.err) || result != test.result {
				t.Fatalf("result = %#v, err = %v", result, err)
			}
			if len(events) != 1 || events[0].Node != "invoke_test" || events[0].Status != test.wantStatus || events[0].Input["value"] != "request" {
				t.Fatalf("events = %#v", events)
			}
		})
	}
}

func TestInvokeRecordsAndRethrowsPanic(t *testing.T) {
	var events []domain.EvaluationTrace
	ctx := WithScope(t.Context(), NewScope(Evaluation, func(event domain.EvaluationTrace) {
		events = append(events, event)
	}))
	spec := Spec[string, string]{Node: "panic_test"}
	defer func() {
		if recovered := recover(); recovered != "boom" {
			t.Fatalf("recovered = %#v", recovered)
		}
		if len(events) != 1 || events[0].Status != "failed" || events[0].Output["error"] != "boom" {
			t.Fatalf("events = %#v", events)
		}
	}()
	_, _ = Invoke(ctx, spec, "request", func(context.Context, string) (string, error) {
		panic("boom")
	})
}

func TestInvokeProjectionPanicDoesNotChangeBusinessResult(t *testing.T) {
	ctx := WithScope(t.Context(), NewScope(Evaluation, nil))
	spec := Spec[string, string]{
		Operation: "test.projection_panic", Node: "projection_panic",
		Output: func(string, string, error) map[string]any { panic("projection failed") },
	}
	result, err := Invoke(ctx, spec, "request", func(context.Context, string) (string, error) {
		return "output", nil
	})
	if err != nil || result != "output" {
		t.Fatalf("result = %q, err = %v", result, err)
	}
}

func TestInvokeRecordPredicateSkipsEventAndProjection(t *testing.T) {
	projected := false
	scope := NewScope(Evaluation, nil)
	ctx := WithScope(t.Context(), scope)
	spec := Spec[string, string]{
		Node: "conditional",
		Input: func(string) map[string]any {
			projected = true
			return nil
		},
		Output: func(string, string, error) map[string]any {
			projected = true
			return nil
		},
		Record: func(string, error) bool { return false },
	}
	result, err := Invoke(ctx, spec, "request", func(context.Context, string) (string, error) {
		return "output", nil
	})
	if err != nil || result != "output" || projected || len(scope.Snapshot()) != 0 {
		t.Fatalf("result = %q, err = %v, projected = %v, events = %#v", result, err, projected, scope.Snapshot())
	}
}
