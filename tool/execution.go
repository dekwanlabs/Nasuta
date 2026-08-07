package tool

import (
	"context"
	"time"

	"github.com/dekwanlabs/nasuta/log"
)

// ExecutionObserver receives one physical tool execution outcome.
type ExecutionObserver interface {
	OnToolExecution(context.Context, Execution)
}

// Execution describes one physical invocation without prescribing an export protocol.
type Execution struct {
	ID        ToolID
	Arguments Arguments
	Result    Result
	Err       error
	Panic     any
	Duration  time.Duration
}

type executionObserverKey struct{}

// WithExecutionObserver attaches one run-scoped tool execution observer.
func WithExecutionObserver(ctx context.Context, observer ExecutionObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, executionObserverKey{}, observer)
}

func beginExecution(ctx context.Context, id ToolID, args Arguments) func(Result, error, any) {
	observer, _ := ctx.Value(executionObserverKey{}).(ExecutionObserver)
	if observer == nil {
		return func(Result, error, any) {}
	}
	argumentSnapshot := make(Arguments, len(args))
	for name, value := range args {
		argumentSnapshot[name] = value
	}
	started := time.Now()
	return func(result Result, err error, recovered any) {
		func() {
			defer func() {
				if observerPanic := recover(); observerPanic != nil {
					log.ErrorfCtx(ctx, "[tool] execution observer tool=%q: %v", id, observerPanic)
				}
			}()
			observer.OnToolExecution(ctx, Execution{
				ID: id, Arguments: argumentSnapshot, Result: result, Err: err, Panic: recovered,
				Duration: time.Since(started),
			})
		}()
	}
}
