package runtrace

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// Capture owns a synchronous trace run and returns its stable export snapshot.
func Capture[Output any](
	ctx context.Context,
	correlation Correlation,
	execute func(context.Context) (Output, error),
) (Output, []domain.EvaluationTrace, error) {
	inherited := FromContext(ctx)
	scope := inherited
	if scope == nil {
		scope = Begin(ctx)
	}
	if scope == nil {
		output, err := execute(ctx)
		return output, nil, err
	}
	if inherited == nil {
		defer scope.Close()
	}
	runCtx := WithScope(ctx, scope)
	runCtx = WithCorrelation(runCtx, correlation)
	output, err := execute(runCtx)
	return output, scope.Snapshot(), err
}
