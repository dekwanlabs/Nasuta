package runtrace

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/log"
)

// Spec projects one typed execution boundary onto the stable trace protocol.
type Spec[Input, Output any] struct {
	Operation string
	Node      string
	Input     func(Input) map[string]any
	Output    func(Input, Output, error) map[string]any
	Status    func(Output, error) string
	Record    func(Output, error) bool
}

// Invoke records one typed execution boundary when tracing is enabled.
func Invoke[Input, Output any](
	ctx context.Context,
	spec Spec[Input, Output],
	input Input,
	execute func(context.Context, Input) (Output, error),
) (output Output, err error) {
	if !domain.TraceEnabled(ctx) {
		return execute(ctx, input)
	}
	started := time.Now()
	defer func() {
		recovered := recover()
		recordInvocation(ctx, spec, input, output, err, recovered, time.Since(started))
		if recovered != nil {
			panic(recovered)
		}
	}()
	return execute(ctx, input)
}

func recordInvocation[Input, Output any](
	ctx context.Context,
	spec Spec[Input, Output],
	input Input,
	output Output,
	err error,
	recovered any,
	duration time.Duration,
) {
	defer func() {
		if projectionPanic := recover(); projectionPanic != nil {
			log.ErrorfCtx(ctx, "[execution_trace] project operation=%q node=%q: %v", spec.Operation, spec.Node, projectionPanic)
		}
	}()
	if recovered != nil {
		domain.RecordTrace(ctx, domain.EvaluationTrace{
			Node: spec.Node, Status: "failed", DurationMS: duration.Milliseconds(),
			Input: projectInput(spec, input), Output: map[string]any{"error": fmt.Sprint(recovered)},
		})
		return
	}
	if spec.Record != nil && !spec.Record(output, err) {
		return
	}
	status := defaultStatus(err)
	if spec.Status != nil {
		if projected := spec.Status(output, err); projected != "" {
			status = projected
		}
	}
	domain.RecordTrace(ctx, domain.EvaluationTrace{
		Node: spec.Node, Status: status, DurationMS: duration.Milliseconds(),
		Input: projectInput(spec, input), Output: projectOutput(spec, input, output, err),
	})
}

func projectInput[Input, Output any](spec Spec[Input, Output], input Input) map[string]any {
	if spec.Input == nil {
		return nil
	}
	return spec.Input(input)
}

func projectOutput[Input, Output any](spec Spec[Input, Output], input Input, output Output, err error) map[string]any {
	if spec.Output == nil {
		return nil
	}
	return spec.Output(input, output, err)
}

func defaultStatus(err error) string {
	switch {
	case err == nil:
		return "completed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	default:
		return "failed"
	}
}
