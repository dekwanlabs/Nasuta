package pipeline

import (
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	"github.com/dekwanlabs/nasuta/internal/scope"
)

const (
	pipelineNodeTimeout = 30 * time.Minute
	pipelineTimeout     = 3 * time.Hour
)

// DefaultDefinition fixes the business stages and human checkpoints.
func DefaultDefinition(version int64) (workflow.Definition, error) {
	if version <= 0 {
		return workflow.Definition{}, fmt.Errorf("feature pipeline version must be positive")
	}
	permission := agentapi.PermissionPolicy{Scopes: []string{scope.FeatureDelivery}}
	nodes := []workflow.NodeDefinition{
		generationNode(NodeGenerateRequirementAnalysis, TransformRequirementAnalysis, requestSchema, stateSchema),
		approvalNode(NodeApproveRequirementAnalysis),
		generationNode(NodeGenerateTechnicalProposal, TransformTechnicalProposal, stateSchema, stateSchema),
		approvalNode(NodeApproveTechnicalProposal),
		generationNode(NodeGenerateSystemDesign, TransformSystemDesign, stateSchema, stateSchema),
		approvalNode(NodeApproveSystemDesign),
		generationNode(NodeGenerateImplementationPlan, TransformImplementationPlan, stateSchema, stateSchema),
		approvalNode(NodeApproveImplementationPlan),
		transformNode(NodeCoding, TransformCoding, stateSchema, stateSchema),
		transformNode(NodeValidation, TransformValidation, stateSchema, resultSchema),
	}
	for index := range nodes {
		nodes[index].Permissions = permission
	}
	edges := make([]workflow.EdgeDefinition, 0, len(nodes)-1)
	for index := 0; index < len(nodes)-1; index++ {
		edges = append(edges, workflow.EdgeDefinition{
			From: nodes[index].ID, To: nodes[index+1].ID, Required: true,
		})
	}
	return workflow.Definition{
		ID:           WorkflowID,
		Version:      version,
		Purpose:      "Generate, review, implement, and validate one Feature Delivery artifact chain.",
		InputSchema:  requestSchema,
		OutputSchema: resultSchema,
		Permissions:  permission,
		Budget: workflow.Budget{
			MaxNodes:        len(nodes),
			MaxParallelism:  1,
			MaxRounds:       1,
			MaxDepth:        len(nodes),
			Timeout:         pipelineTimeout,
			MaxHandoffBytes: 2 << 20,
		},
		FailurePolicy: workflow.FailurePolicy{Mode: workflow.FailFast},
		Nodes:         nodes,
		Edges:         edges,
	}, nil
}

func generationNode(
	id, transform string,
	input, output agentapi.SchemaRef,
) workflow.NodeDefinition {
	return workflow.NodeDefinition{
		ID: id, Kind: workflow.NodeTransform, TransformID: transform,
		InputSchema: input, OutputSchema: output, RetrySafe: true,
		Retry:   workflow.RetryPolicy{MaxAttempts: 2, Backoff: time.Second},
		Timeout: pipelineNodeTimeout,
	}
}

func approvalNode(id string) workflow.NodeDefinition {
	return workflow.NodeDefinition{
		ID: id, Kind: workflow.NodeHumanApproval,
		InputSchema: stateSchema, OutputSchema: stateSchema,
		Timeout: pipelineNodeTimeout,
	}
}

func transformNode(
	id, transform string,
	input, output agentapi.SchemaRef,
) workflow.NodeDefinition {
	return workflow.NodeDefinition{
		ID: id, Kind: workflow.NodeTransform, TransformID: transform,
		InputSchema: input, OutputSchema: output, Timeout: pipelineNodeTimeout,
	}
}

func stageKind(transform string) (delivery.ArtifactKind, error) {
	switch transform {
	case TransformRequirementAnalysis:
		return delivery.KindRequirementAnalysis, nil
	case TransformTechnicalProposal:
		return delivery.KindTechnicalProposal, nil
	case TransformSystemDesign:
		return delivery.KindSystemDesign, nil
	case TransformImplementationPlan:
		return delivery.KindImplementationPlan, nil
	default:
		return "", fmt.Errorf("transform %q is not an artifact generation stage", transform)
	}
}
