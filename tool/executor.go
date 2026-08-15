package tool

import (
	"context"
	"fmt"
	"time"
)

// Executor applies validation and timeouts before invoking a pinned handler.
type Executor struct {
	DefaultTimeout time.Duration
}

func NewExecutor(timeout time.Duration) *Executor {
	return &Executor{DefaultTimeout: timeout}
}

// Execute resolves from the caller's snapshot so one Run cannot observe registry changes mid-flight.
func (executor *Executor) Execute(ctx context.Context, snapshot Snapshot, id ToolID, args Arguments) (Result, error) {
	finish := beginExecution(ctx, id, args)
	var result Result
	var err error
	defer func() {
		if recovered := recover(); recovered != nil {
			finish(result, err, recovered)
			panic(recovered)
		}
		finish(result, err, nil)
	}()
	candidate, ok := snapshot.Get(id)
	if !ok {
		err = fmt.Errorf("tool %q is not available in this snapshot", id)
		return Result{}, err
	}
	if validationErr := validateArguments(candidate.InputSchema, args, "arguments"); validationErr != nil {
		err = fmt.Errorf("tool %q arguments: %w", id, validationErr)
		return Result{}, err
	}
	timeout := executor.DefaultTimeout
	if candidate.Prefetch != nil && candidate.Prefetch.Timeout > 0 {
		timeout = candidate.Prefetch.Timeout
	}
	if timeout <= 0 {
		result, err = candidate.Handler.Execute(ctx, args)
		return result, err
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err = candidate.Handler.Execute(execCtx, args)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
