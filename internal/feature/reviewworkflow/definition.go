package reviewworkflow

import (
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
)

// Definition derives the fixed panel represented directly by a published Policy.
func Definition(policy delivery.ReviewPolicy) (workflow.WorkflowDefinition, error) {
	prepared, err := delivery.PrepareReviewPolicy(policy)
	if err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	return definitionForReviewers(prepared, prepared.Reviewers, "")
}

// DefinitionForRound derives the Workflow only from the immutable Round panel.
func DefinitionForRound(
	policy delivery.ReviewPolicy,
	round delivery.ReviewRound,
) (workflow.WorkflowDefinition, error) {
	prepared, err := delivery.PrepareReviewPolicy(policy)
	if err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	if err := delivery.ValidateReviewRoundSnapshot(prepared, round); err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	return definitionForReviewers(prepared, round.Reviewers, round.PanelHash)
}

func definitionForReviewers(
	prepared delivery.ReviewPolicy,
	reviewers []delivery.ReviewerSpec,
	panelHash string,
) (workflow.WorkflowDefinition, error) {
	id, err := workflowID(prepared, panelHash)
	if err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	permission := agentapi.PermissionPolicy{Scopes: []string{platformscope.FeatureDelivery}}
	nodes := make([]workflow.NodeDefinition, 0, len(reviewers)+3)
	edges := make([]workflow.EdgeDefinition, 0, len(reviewers)+2)
	seenNodes := make(map[string]string, len(reviewers))
	budgetedNodes := len(reviewers)
	if prepared.Adjudicator != nil {
		budgetedNodes++
	}
	for index, reviewer := range reviewers {
		nodeID := reviewerNodeID(reviewer.ID)
		if existing, duplicate := seenNodes[nodeID]; duplicate {
			return workflow.WorkflowDefinition{}, fmt.Errorf(
				"reviewers %q and %q share workflow node %q: %w",
				existing, reviewer.ID, nodeID, delivery.ErrConflict,
			)
		}
		seenNodes[nodeID] = reviewer.ID
		nodes = append(nodes, workflow.NodeDefinition{
			ID: nodeID, Kind: workflow.NodeTransform,
			TransformID: TransformAssignment,
			InputSchema: requestSchema, OutputSchema: reportSchema,
			Permissions: permission, RetrySafe: true, Optional: !reviewer.Required,
			Budget:  reviewNodeBudget(prepared, index, budgetedNodes),
			Retry:   reviewRetryPolicy(prepared.MaxRetries),
			Timeout: prepared.Timeout,
		})
		edges = append(edges, workflow.EdgeDefinition{
			From: nodeID, To: NodeReportsJoin, Required: reviewer.Required,
		})
	}
	adjudicationBudget := workflow.NodeBudget{}
	if prepared.Adjudicator != nil {
		adjudicationBudget = reviewNodeBudget(
			prepared,
			len(reviewers),
			budgetedNodes,
		)
	}
	nodes = append(nodes,
		workflow.NodeDefinition{
			ID: NodeReportsJoin, Kind: workflow.NodeJoin,
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
			workflow.NodeBudget{},
			prepared,
		),
	)
	edges = append(edges,
		workflow.EdgeDefinition{From: NodeReportsJoin, To: NodeAdjudicate, Required: true},
		workflow.EdgeDefinition{From: NodeAdjudicate, To: NodeGate, Required: true},
	)
	return workflow.WorkflowDefinition{
		ID: id, Version: prepared.Version,
		Purpose:     "Run an immutable parallel Feature Delivery review panel, adjudication, and deterministic gate.",
		InputSchema: requestSchema, OutputSchema: gateSchema,
		Permissions: permission,
		Budget: workflow.WorkflowBudget{
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
		FailurePolicy: workflow.WorkflowFailurePolicy{Mode: workflow.CollectAvailable},
		Nodes:         nodes,
		Edges:         edges,
	}, nil
}

func reviewTransformNode(
	id, transform string,
	input, output agentapi.SchemaRef,
	permission agentapi.PermissionPolicy,
	budget workflow.NodeBudget,
	policy delivery.ReviewPolicy,
) workflow.NodeDefinition {
	return workflow.NodeDefinition{
		ID: id, Kind: workflow.NodeTransform, TransformID: transform,
		InputSchema: input, OutputSchema: output,
		Permissions: permission, Budget: budget, RetrySafe: true,
		Retry:   reviewRetryPolicy(policy.MaxRetries),
		Timeout: policy.Timeout,
	}
}

func reviewNodeBudget(
	policy delivery.ReviewPolicy,
	index, count int,
) workflow.NodeBudget {
	return workflow.NodeBudget{
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

func reviewRetryPolicy(maxRetries int64) workflow.RetryPolicy {
	maxAttempts := 10
	if maxRetries < 9 {
		maxAttempts = int(maxRetries) + 1
	}
	return workflow.RetryPolicy{MaxAttempts: maxAttempts}
}
