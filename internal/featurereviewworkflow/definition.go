package featurereviewworkflow

import (
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
)

// Definition derives the fixed panel represented directly by a published Policy.
func Definition(policy featuredelivery.ReviewPolicy) (agentworkflow.WorkflowDefinition, error) {
	prepared, err := featuredelivery.PrepareReviewPolicy(policy)
	if err != nil {
		return agentworkflow.WorkflowDefinition{}, err
	}
	return definitionForReviewers(prepared, prepared.Reviewers, "")
}

// DefinitionForRound derives the Workflow only from the immutable Round panel.
func DefinitionForRound(
	policy featuredelivery.ReviewPolicy,
	round featuredelivery.ReviewRound,
) (agentworkflow.WorkflowDefinition, error) {
	prepared, err := featuredelivery.PrepareReviewPolicy(policy)
	if err != nil {
		return agentworkflow.WorkflowDefinition{}, err
	}
	if err := featuredelivery.ValidateReviewRoundSnapshot(prepared, round); err != nil {
		return agentworkflow.WorkflowDefinition{}, err
	}
	return definitionForReviewers(prepared, round.Reviewers, round.PanelHash)
}

func definitionForReviewers(
	prepared featuredelivery.ReviewPolicy,
	reviewers []featuredelivery.ReviewerSpec,
	panelHash string,
) (agentworkflow.WorkflowDefinition, error) {
	id, err := workflowID(prepared, panelHash)
	if err != nil {
		return agentworkflow.WorkflowDefinition{}, err
	}
	permission := agentapi.PermissionPolicy{Scopes: []string{platformscope.FeatureDelivery}}
	nodes := make([]agentworkflow.NodeDefinition, 0, len(reviewers)+3)
	edges := make([]agentworkflow.EdgeDefinition, 0, len(reviewers)+2)
	seenNodes := make(map[string]string, len(reviewers))
	budgetedNodes := len(reviewers)
	if prepared.Adjudicator != nil {
		budgetedNodes++
	}
	for index, reviewer := range reviewers {
		nodeID := reviewerNodeID(reviewer.ID)
		if existing, duplicate := seenNodes[nodeID]; duplicate {
			return agentworkflow.WorkflowDefinition{}, fmt.Errorf(
				"reviewers %q and %q share workflow node %q: %w",
				existing, reviewer.ID, nodeID, featuredelivery.ErrConflict,
			)
		}
		seenNodes[nodeID] = reviewer.ID
		nodes = append(nodes, agentworkflow.NodeDefinition{
			ID: nodeID, Kind: agentworkflow.NodeTransform,
			TransformID: TransformAssignment,
			InputSchema: requestSchema, OutputSchema: reportSchema,
			Permissions: permission, RetrySafe: true, Optional: !reviewer.Required,
			Budget:  reviewNodeBudget(prepared, index, budgetedNodes),
			Retry:   reviewRetryPolicy(prepared.MaxRetries),
			Timeout: prepared.Timeout,
		})
		edges = append(edges, agentworkflow.EdgeDefinition{
			From: nodeID, To: NodeReportsJoin, Required: reviewer.Required,
		})
	}
	adjudicationBudget := agentworkflow.NodeBudget{}
	if prepared.Adjudicator != nil {
		adjudicationBudget = reviewNodeBudget(
			prepared,
			len(reviewers),
			budgetedNodes,
		)
	}
	nodes = append(nodes,
		agentworkflow.NodeDefinition{
			ID: NodeReportsJoin, Kind: agentworkflow.NodeJoin,
			InputSchema: reportSchema, OutputSchema: reportListSchema,
			Permissions: permission,
			Retry:       reviewRetryPolicy(prepared.MaxRetries),
			Timeout:     prepared.Timeout,
		},
		reviewTransformNode(
			NodeAdjudicate,
			TransformAdjudication,
			reportListSchema,
			reportListSchema,
			permission,
			adjudicationBudget,
			prepared,
		),
		reviewTransformNode(
			NodeGate,
			TransformGate,
			reportListSchema,
			gateSchema,
			permission,
			agentworkflow.NodeBudget{},
			prepared,
		),
	)
	edges = append(edges,
		agentworkflow.EdgeDefinition{From: NodeReportsJoin, To: NodeAdjudicate, Required: true},
		agentworkflow.EdgeDefinition{From: NodeAdjudicate, To: NodeGate, Required: true},
	)
	return agentworkflow.WorkflowDefinition{
		ID: id, Version: prepared.Version,
		Purpose:     "Run an immutable parallel Feature Delivery review panel, adjudication, and deterministic gate.",
		InputSchema: requestSchema, OutputSchema: gateSchema,
		Permissions: permission,
		Budget: agentworkflow.WorkflowBudget{
			MaxNodes:        len(nodes),
			MaxParallelism:  min(prepared.MaxParallelism, len(reviewers)),
			Timeout:         prepared.Timeout,
			MaxHandoffBytes: 8 << 20,
			MaxInputTokens:  prepared.MaxInputTokens,
			MaxOutputTokens: prepared.MaxOutputTokens,
			MaxTotalTokens:  prepared.MaxTotalTokens,
			MaxToolCalls:    prepared.MaxToolCalls,
			MaxCostMicros:   prepared.MaxCostMicros,
			MaxRetries:      prepared.MaxRetries,
		},
		FailurePolicy: agentworkflow.WorkflowFailurePolicy{Mode: agentworkflow.CollectAvailable},
		Nodes:         nodes,
		Edges:         edges,
	}, nil
}

func reviewTransformNode(
	id, transform string,
	input, output agentapi.SchemaRef,
	permission agentapi.PermissionPolicy,
	budget agentworkflow.NodeBudget,
	policy featuredelivery.ReviewPolicy,
) agentworkflow.NodeDefinition {
	return agentworkflow.NodeDefinition{
		ID: id, Kind: agentworkflow.NodeTransform, TransformID: transform,
		InputSchema: input, OutputSchema: output,
		Permissions: permission, Budget: budget, RetrySafe: true,
		Retry:   reviewRetryPolicy(policy.MaxRetries),
		Timeout: policy.Timeout,
	}
}

func reviewNodeBudget(
	policy featuredelivery.ReviewPolicy,
	index, count int,
) agentworkflow.NodeBudget {
	return agentworkflow.NodeBudget{
		MaxInputTokens:  splitBudget(policy.MaxInputTokens, index, count),
		MaxOutputTokens: splitBudget(policy.MaxOutputTokens, index, count),
		MaxTotalTokens:  splitBudget(policy.MaxTotalTokens, index, count),
		MaxToolCalls:    splitBudget(policy.MaxToolCalls, index, count),
		MaxCostMicros:   splitBudget(policy.MaxCostMicros, index, count),
	}
}

func splitBudget(total int64, index, count int) int64 {
	share := total / int64(count)
	if int64(index) < total%int64(count) {
		share++
	}
	return share
}

func reviewRetryPolicy(maxRetries int64) agentworkflow.RetryPolicy {
	maxAttempts := 10
	if maxRetries < 9 {
		maxAttempts = int(maxRetries) + 1
	}
	return agentworkflow.RetryPolicy{MaxAttempts: maxAttempts}
}
