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

func (executor *Executor) Execute(ctx context.Context, snapshot Snapshot, id ToolID, args Arguments) (Result, error) {
	candidate, ok := snapshot.Get(id)
	if !ok {
		return Result{}, fmt.Errorf("tool %q is not available in this snapshot", id)
	}
	if err := validateArguments(candidate.InputSchema, args, "arguments"); err != nil {
		return Result{}, fmt.Errorf("tool %q arguments: %w", id, err)
	}
	timeout := executor.DefaultTimeout
	if candidate.Prefetch != nil && candidate.Prefetch.Timeout > 0 {
		timeout = candidate.Prefetch.Timeout
	}
	if timeout <= 0 {
		return candidate.Handler.Execute(ctx, args)
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := candidate.Handler.Execute(execCtx, args)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
