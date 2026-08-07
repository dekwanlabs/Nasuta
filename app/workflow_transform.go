package app

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featurepipeline"
	"github.com/dekwanlabs/nasuta/internal/featurereviewworkflow"
)

type workflowTransformDispatcher struct {
	pipeline *featurepipeline.Executor
	review   *featurereviewworkflow.Executor
}

func newWorkflowTransformDispatcher(
	pipeline *featurepipeline.Executor,
	review *featurereviewworkflow.Executor,
) agentworkflow.NodeExecutor {
	if pipeline == nil && review == nil {
		return nil
	}
	return &workflowTransformDispatcher{pipeline: pipeline, review: review}
}

func (dispatcher *workflowTransformDispatcher) Execute(
	ctx context.Context,
	request agentworkflow.NodeRequest,
) (agentworkflow.NodeResult, error) {
	switch request.Node.TransformID {
	case featurepipeline.TransformRequirementAnalysis,
		featurepipeline.TransformTechnicalProposal,
		featurepipeline.TransformSystemDesign,
		featurepipeline.TransformImplementationPlan,
		featurepipeline.TransformCoding,
		featurepipeline.TransformValidation:
		if dispatcher.pipeline == nil {
			return agentworkflow.NodeResult{}, fmt.Errorf(
				"feature pipeline transform %q is unavailable",
				request.Node.TransformID,
			)
		}
		return dispatcher.pipeline.Execute(ctx, request)
	case featurereviewworkflow.TransformAssignment,
		featurereviewworkflow.TransformAdjudication,
		featurereviewworkflow.TransformGate:
		if dispatcher.review == nil {
			return agentworkflow.NodeResult{}, fmt.Errorf(
				"feature review transform %q is unavailable",
				request.Node.TransformID,
			)
		}
		return dispatcher.review.Execute(ctx, request)
	default:
		return agentworkflow.NodeResult{}, fmt.Errorf(
			"workflow transform %q is unsupported",
			request.Node.TransformID,
		)
	}
}

var _ agentworkflow.NodeExecutor = (*workflowTransformDispatcher)(nil)
