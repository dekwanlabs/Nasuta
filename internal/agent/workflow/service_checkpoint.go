package workflow

import (
	"context"
	"fmt"
	"sort"

	"github.com/dekwanlabs/nasuta/internal/scope"
)

func progressFromState(
	definition Definition,
	state *RunState,
) (Progress, error) {
	if state == nil {
		return Progress{}, fmt.Errorf("workflow checkpoint is required")
	}
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
	}
	progress := Progress{
		StartedAt:             state.Run.StartedAt,
		Input:                 state.Input,
		NodeOutputs:           state.NodeOutputs,
		Gates:                 state.Gates,
		FailedOptional:        make(map[string]struct{}),
		FailedOptionalReasons: make(map[string]StopReason),
		WaitingHuman:          make(map[string]struct{}),
		NodeAttempts:          make(map[string]NodeAttemptProgress),
		Usage:                 state.Run.Usage,
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
			return Progress{}, fmt.Errorf(
				"workflow run %q checkpoint contains unknown node %q",
				state.Run.ID, nodeID,
			)
		}
		if run.Kind != node.Kind {
			return Progress{}, fmt.Errorf(
				"workflow run %q node %q kind changed from %q to %q",
				state.Run.ID, nodeID, run.Kind, node.Kind,
			)
		}
		switch run.Status {
		case RunSucceeded:
		case RunWaitingHuman:
			if node.Kind != NodeHumanApproval {
				return Progress{}, fmt.Errorf(
					"workflow run %q non-human node %q is waiting for approval",
					state.Run.ID, nodeID,
				)
			}
			progress.WaitingHuman[nodeID] = struct{}{}
		case RunFailed, RunCancelled, RunTimedOut:
			if run.Status == RunFailed &&
				retryableCheckpoint(run.ErrorCode) &&
				recoveryRetryAllowed(definition, state.Run, node, run.Attempt) {
				if run.EndedAt == nil || run.FirstStartedAt.IsZero() {
					return Progress{}, fmt.Errorf(
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
				progress.FailedOptionalReasons[nodeID] = checkpointStopReason(
					run.ErrorCode,
				)
				continue
			}
			return Progress{}, &checkpointTerminalError{
				workflowRunID: state.Run.ID,
				nodeID:        nodeID,
				status:        run.Status,
			}
		default:
			return Progress{}, fmt.Errorf(
				"workflow run %q node %q checkpoint status %q cannot be resumed",
				state.Run.ID, nodeID, run.Status,
			)
		}
	}
	return progress, nil
}

func checkpointStopReason(errorCode string) StopReason {
	switch errorCode {
	case "no_affordable_task":
		return StopNoAffordableTask
	case "workflow_budget_exhausted":
		return StopBudgetExhausted
	case "needs_clarification":
		return StopNeedsClarification
	default:
		return StopCapabilityUnavailable
	}
}

func interruptedUsage(node NodeDefinition, attempt int) Usage {
	totalTokens := node.Budget.MaxTotalTokens
	if totalTokens == 0 {
		totalTokens = node.Budget.MaxInputTokens + node.Budget.MaxOutputTokens
	}
	usage := Usage{
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

func retryableCheckpoint(errorCode string) bool {
	return errorCode == nodeRetryableErrorCode ||
		errorCode == nodeRestartedRetryableErrorCode
}

func recoveryRetryAllowed(
	definition Definition,
	run RunRecord,
	node NodeDefinition,
	attempt int,
) bool {
	if attempt <= 0 || attempt >= node.Retry.MaxAttempts ||
		(node.Kind != NodeAgent &&
			node.Kind != NodeJoin &&
			node.Kind != NodeVerifier &&
			!(node.Kind == NodeTransform && node.RetrySafe)) {
		return false
	}
	effective := IntersectPermissions(
		run.ActorPermissions,
		run.ScenarioPermissions,
		definition.Permissions,
		node.Permissions,
	)
	return !scope.HasSideEffect(effective.Scopes) ||
		node.Kind == NodeAgent && node.RetrySafe
}
