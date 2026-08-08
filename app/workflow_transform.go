package app

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/feature/pipeline"
	"github.com/dekwanlabs/nasuta/internal/feature/reviewworkflow"
)

type workflowTransformDispatcher struct {
	pipeline *pipeline.Executor
	review   *reviewworkflow.Executor
}

func newWorkflowTransformDispatcher(
	pipeline *pipeline.Executor,
	review *reviewworkflow.Executor,
) workflow.NodeExecutor {
	if pipeline == nil && review == nil {
		return nil
	}
	return &workflowTransformDispatcher{pipeline: pipeline, review: review}
}

func (dispatcher *workflowTransformDispatcher) Execute(
	ctx context.Context,
	request workflow.NodeRequest,
) (workflow.NodeResult, error) {
	switch request.Node.TransformID {
	case pipeline.TransformRequirementAnalysis,
		pipeline.TransformTechnicalProposal,
		pipeline.TransformSystemDesign,
		pipeline.TransformImplementationPlan,
		pipeline.TransformCoding,
		pipeline.TransformValidation:
		if dispatcher.pipeline == nil {
			return workflow.NodeResult{}, fmt.Errorf(
				"feature pipeline transform %q is unavailable",
				request.Node.TransformID,
			)
		}
		return dispatcher.pipeline.Execute(ctx, request)
	case reviewworkflow.TransformAssignment,
		reviewworkflow.TransformAdjudication,
		reviewworkflow.TransformGate:
		if dispatcher.review == nil {
			return workflow.NodeResult{}, fmt.Errorf(
				"feature review transform %q is unavailable",
				request.Node.TransformID,
			)
		}
		return dispatcher.review.Execute(ctx, request)
	default:
		return workflow.NodeResult{}, fmt.Errorf(
			"workflow transform %q is unsupported",
			request.Node.TransformID,
		)
	}
}

var _ workflow.NodeExecutor = (*workflowTransformDispatcher)(nil)
