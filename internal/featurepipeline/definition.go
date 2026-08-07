package featurepipeline

import (
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
)

const (
	pipelineNodeTimeout = 30 * time.Minute
	pipelineTimeout     = 3 * time.Hour
)

// DefaultDefinition fixes the business stages and human checkpoints.
func DefaultDefinition(version int64) (agentworkflow.WorkflowDefinition, error) {
	if version <= 0 {
		return agentworkflow.WorkflowDefinition{}, fmt.Errorf("feature pipeline version must be positive")
	}
	permission := agentapi.PermissionPolicy{Scopes: []string{platformscope.FeatureDelivery}}
	nodes := []agentworkflow.NodeDefinition{
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
	edges := make([]agentworkflow.EdgeDefinition, 0, len(nodes)-1)
	for index := 0; index < len(nodes)-1; index++ {
		edges = append(edges, agentworkflow.EdgeDefinition{
			From: nodes[index].ID, To: nodes[index+1].ID, Required: true,
		})
	}
	return agentworkflow.WorkflowDefinition{
		ID:           WorkflowID,
		Version:      version,
		Purpose:      "Generate, review, implement, and validate one Feature Delivery artifact chain.",
		InputSchema:  requestSchema,
		OutputSchema: resultSchema,
		Permissions:  permission,
		Budget: agentworkflow.WorkflowBudget{
			MaxNodes:        len(nodes),
			MaxParallelism:  1,
			Timeout:         pipelineTimeout,
			MaxHandoffBytes: 2 << 20,
		},
		FailurePolicy: agentworkflow.WorkflowFailurePolicy{Mode: agentworkflow.FailFast},
		Nodes:         nodes,
		Edges:         edges,
	}, nil
}

func generationNode(
	id, transform string,
	input, output agentapi.SchemaRef,
) agentworkflow.NodeDefinition {
	return agentworkflow.NodeDefinition{
		ID: id, Kind: agentworkflow.NodeTransform, TransformID: transform,
		InputSchema: input, OutputSchema: output, RetrySafe: true,
		Retry:   agentworkflow.RetryPolicy{MaxAttempts: 2, Backoff: time.Second},
		Timeout: pipelineNodeTimeout,
	}
}

func approvalNode(id string) agentworkflow.NodeDefinition {
	return agentworkflow.NodeDefinition{
		ID: id, Kind: agentworkflow.NodeHumanApproval,
		InputSchema: stateSchema, OutputSchema: stateSchema,
		Timeout: pipelineNodeTimeout,
	}
}

func transformNode(
	id, transform string,
	input, output agentapi.SchemaRef,
) agentworkflow.NodeDefinition {
	return agentworkflow.NodeDefinition{
		ID: id, Kind: agentworkflow.NodeTransform, TransformID: transform,
		InputSchema: input, OutputSchema: output, Timeout: pipelineNodeTimeout,
	}
}

func stageKind(transform string) (featuredelivery.ArtifactKind, error) {
	switch transform {
	case TransformRequirementAnalysis:
		return featuredelivery.KindRequirementAnalysis, nil
	case TransformTechnicalProposal:
		return featuredelivery.KindTechnicalProposal, nil
	case TransformSystemDesign:
		return featuredelivery.KindSystemDesign, nil
	case TransformImplementationPlan:
		return featuredelivery.KindImplementationPlan, nil
	default:
		return "", fmt.Errorf("transform %q is not an artifact generation stage", transform)
	}
}
