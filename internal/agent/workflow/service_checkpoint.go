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
		if err := applyCheckpointNode(definition, state, nodes, nodeID, &progress); err != nil {
			return Progress{}, err
		}
	}
	return progress, nil
}

func applyCheckpointNode(
	definition Definition,
	state *RunState,
	nodes map[string]NodeDefinition,
	nodeID string,
	progress *Progress,
) error {
	run := state.Nodes[nodeID]
	node, ok := nodes[nodeID]
	if !ok {
		return fmt.Errorf(
			"workflow run %q checkpoint contains unknown node %q",
			state.Run.ID, nodeID,
		)
	}
	if run.Kind != node.Kind {
		return fmt.Errorf(
			"workflow run %q node %q kind changed from %q to %q",
			state.Run.ID, nodeID, run.Kind, node.Kind,
		)
	}
	switch run.Status {
	case RunSucceeded:
		return nil
	case RunWaitingHuman:
		if node.Kind != NodeHumanApproval {
			return fmt.Errorf(
				"workflow run %q non-human node %q is waiting for approval",
				state.Run.ID, nodeID,
			)
		}
		progress.WaitingHuman[nodeID] = struct{}{}
		return nil
	case RunFailed, RunCancelled, RunTimedOut:
		if run.Status == RunFailed &&
			retryableCheckpoint(run.ErrorCode) &&
			recoveryRetryAllowed(definition, state.Run, node, run.Attempt) {
			if run.EndedAt == nil || run.FirstStartedAt.IsZero() {
				return fmt.Errorf(
					"workflow run %q node %q retry timing is incomplete",
					state.Run.ID, nodeID,
				)
			}
			progress.NodeAttempts[nodeID] = NodeAttemptProgress{
				NextAttempt:    run.Attempt + 1,
				FirstStartedAt: run.FirstStartedAt,
				NotBefore:      run.EndedAt.Add(node.Retry.Backoff),
			}
			return nil
		}
		if node.Optional && definition.FailurePolicy.Mode == CollectAvailable {
			progress.FailedOptional[nodeID] = struct{}{}
			progress.FailedOptionalReasons[nodeID] = checkpointStopReason(
				run.ErrorCode,
			)
			return nil
		}
		return &checkpointTerminalError{
			workflowRunID: state.Run.ID,
			nodeID:        nodeID,
			status:        run.Status,
		}
	default:
		return fmt.Errorf(
			"workflow run %q node %q checkpoint status %q cannot be resumed",
			state.Run.ID, nodeID, run.Status,
		)
	}
}

func checkpointStopReason(errorCode string) StopReason {
	switch errorCode {
	case "no_affordable_task":
		return StopNoAffordableTask
	case "workflow_budget_exhausted":
		return StopBudgetExhausted
	case "needs_clarification":
		return StopNeedsClarification
	case "evidence_insufficient":
		return StopEvidenceInsufficient
	default:
		return StopCapabilityUnavailable
	}
}

func interruptedUsage(_ NodeDefinition, attempt int) Usage {
	// A takeover record cannot infer provider token/cost/tool usage from a
	// running attempt. Do not turn the legacy NodeBudget projection hint into
	// fabricated consumption; only a resumed attempt itself is a retry.
	if attempt <= 1 {
		return Usage{}
	}
	return Usage{Retries: 1}
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
