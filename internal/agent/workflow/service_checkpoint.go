package workflow

import (
	"context"
	"fmt"
	"sort"

	"github.com/dekwanlabs/nasuta/internal/scope"
)

func workflowProgressFromState(
	definition WorkflowDefinition,
	state *WorkflowRunState,
) (WorkflowProgress, error) {
	if state == nil {
		return WorkflowProgress{}, fmt.Errorf("workflow checkpoint is required")
	}
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	progress := WorkflowProgress{
		StartedAt:      state.Run.StartedAt,
		Input:          state.Input,
		NodeOutputs:    state.NodeOutputs,
		Gates:          state.Gates,
		FailedOptional: make(map[string]struct{}),
		WaitingHuman:   make(map[string]struct{}),
		NodeAttempts:   make(map[string]NodeAttemptProgress),
		Usage:          state.Run.Usage,
	}
	nodeIDs := make([]string, 0, len(state.Nodes))
	for nodeID := range state.Nodes {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Strings(nodeIDs)
	for _, nodeID := range nodeIDs {
		run := state.Nodes[nodeID]
		node, ok := nodes[nodeID]
		if !ok {
			return WorkflowProgress{}, fmt.Errorf(
				"workflow run %q checkpoint contains unknown node %q",
				state.Run.ID, nodeID,
			)
		}
		if run.Kind != node.Kind {
			return WorkflowProgress{}, fmt.Errorf(
				"workflow run %q node %q kind changed from %q to %q",
				state.Run.ID, nodeID, run.Kind, node.Kind,
			)
		}
		switch run.Status {
		case RunSucceeded:
		case RunWaitingHuman:
			if node.Kind != NodeHumanApproval {
				return WorkflowProgress{}, fmt.Errorf(
					"workflow run %q non-human node %q is waiting for approval",
					state.Run.ID, nodeID,
				)
			}
			progress.WaitingHuman[nodeID] = struct{}{}
		case RunFailed, RunCancelled, RunTimedOut:
			if run.Status == RunFailed &&
				retryableCheckpointFailure(run.ErrorCode) &&
				recoveryRetryAllowed(definition, state.Run, node, run.Attempt) {
				if run.EndedAt == nil || run.FirstStartedAt.IsZero() {
					return WorkflowProgress{}, fmt.Errorf(
						"workflow run %q node %q retry timing is incomplete",
						state.Run.ID, nodeID,
					)
				}
				progress.NodeAttempts[nodeID] = NodeAttemptProgress{
					NextAttempt:    run.Attempt + 1,
					FirstStartedAt: run.FirstStartedAt,
					NotBefore:      run.EndedAt.Add(node.Retry.Backoff),
				}
				continue
			}
			if node.Optional && definition.FailurePolicy.Mode == CollectAvailable {
				progress.FailedOptional[nodeID] = struct{}{}
				continue
			}
			return WorkflowProgress{}, &checkpointTerminalError{
				workflowRunID: state.Run.ID,
				nodeID:        nodeID,
				status:        run.Status,
			}
		default:
			return WorkflowProgress{}, fmt.Errorf(
				"workflow run %q node %q checkpoint status %q cannot be resumed",
				state.Run.ID, nodeID, run.Status,
			)
		}
	}
	return progress, nil
}

func interruptedAttemptUsage(node NodeDefinition, attempt int) WorkflowUsage {
	totalTokens := node.Budget.MaxTotalTokens
	if totalTokens == 0 {
		totalTokens = node.Budget.MaxInputTokens + node.Budget.MaxOutputTokens
	}
	usage := WorkflowUsage{
		InputTokens:  node.Budget.MaxInputTokens,
		OutputTokens: node.Budget.MaxOutputTokens,
		TotalTokens:  totalTokens,
		ToolCalls:    node.Budget.MaxToolCalls,
		CostMicros:   node.Budget.MaxCostMicros,
	}
	if attempt > 1 {
		usage.Retries = 1
	}
	return usage
}

type checkpointTerminalError struct {
	workflowRunID string
	nodeID        string
	status        RunStatus
}

func (err *checkpointTerminalError) Error() string {
	return fmt.Sprintf(
		"workflow run %q required node %q is %q",
		err.workflowRunID, err.nodeID, err.status,
	)
}

func (err *checkpointTerminalError) Unwrap() error {
	switch err.status {
	case RunCancelled:
		return context.Canceled
	case RunTimedOut:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func retryableCheckpointFailure(errorCode string) bool {
	return errorCode == nodeRetryableErrorCode ||
		errorCode == nodeRestartedRetryableErrorCode
}

func recoveryRetryAllowed(
	definition WorkflowDefinition,
	run WorkflowRunRecord,
	node NodeDefinition,
	attempt int,
) bool {
	if attempt <= 0 || attempt >= node.Retry.MaxAttempts ||
		(node.Kind != NodeAgent &&
			node.Kind != NodeJoin &&
			!(node.Kind == NodeTransform && node.RetrySafe)) {
		return false
	}
	effective := IntersectPermissions(
		run.ActorPermissions,
		run.ScenarioPermissions,
		definition.Permissions,
		node.Permissions,
	)
	return !scope.HasSideEffect(effective.Scopes)
}
