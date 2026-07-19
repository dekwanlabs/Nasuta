package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// JSONHandler adapts a typed operation to the shared text result contract.
func JSONHandler[T any](fn func(context.Context, Arguments) (T, error)) Handler {
	return HandlerFunc(func(ctx context.Context, args Arguments) (Result, error) {
		value, err := fn(ctx, args)
		if err != nil {
			return Result{}, err
		}
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return Result{}, fmt.Errorf("encode tool result: %w", err)
		}
		return Result{Content: string(data)}, nil
	})
}
