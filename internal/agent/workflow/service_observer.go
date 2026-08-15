package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type storeRunObserver struct {
	store persistence
}

func (observer *storeRunObserver) NodeStarted(ctx context.Context, request NodeRequest) error {
	inputIDs := make([]string, 0, len(request.Inputs))
	for _, input := range request.Inputs {
		inputIDs = append(inputIDs, input.ID)
	}
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	return observer.store.StartNode(persistCtx, NodeRunRecord{
		WorkflowRunID: request.WorkflowRunID, NodeID: request.Node.ID, Attempt: request.Attempt,
		Kind: request.Node.Kind, InputHandoffIDs: inputIDs, Status: RunRunning,
		StartedAt: time.Now().UTC(),
	})
}

func (observer *storeRunObserver) NodeSucceeded(
	ctx context.Context,
	request NodeRequest,
	result NodeResult,
	decision *GateDecision,
) error {
	persistCtx, cancel := persistenceContext(ctx)
	err := observer.store.SucceedNode(
		persistCtx,
		request.WorkflowRunID,
		request.Node.ID,
		request.Attempt,
		result.AgentRunID,
		result.Handoff,
		decision,
		result.Usage,
		time.Now().UTC(),
	)
	cancel()
	if err == nil {
		return nil
	}
	failCtx, failCancel := persistenceContext(ctx)
	failErr := observer.store.FailNode(
		failCtx,
		request.WorkflowRunID,
		request.Node.ID,
		request.Attempt,
		result.AgentRunID,
		RunFailed,
		"node_persistence_failed",
		result.Usage,
		time.Now().UTC(),
	)
	failCancel()
	if failErr != nil {
		return errors.Join(err, fmt.Errorf("close node after success persistence failure: %w", failErr))
	}
	return err
}

func (observer *storeRunObserver) NodeFailed(
	ctx context.Context,
	request NodeRequest,
	result NodeResult,
	runErr error,
) error {
	status, errorCode := nodeResultStatus(request, runErr)
	persistCtx, cancel := persistenceContext(ctx)
	defer cancel()
	return observer.store.FailNode(
		persistCtx,
		request.WorkflowRunID,
		request.Node.ID,
		request.Attempt,
		result.AgentRunID,
		status,
		errorCode,
		result.Usage,
		time.Now().UTC(),
	)
}

func persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
}

func resultStatus(runErr error) (RunStatus, string) {
	if runErr == nil {
		return RunSucceeded, ""
	}
	switch errorStopReason(runErr) {
	case StopNeedsClarification:
		return RunFailed, "needs_clarification"
	case StopNoAffordableTask:
		return RunFailed, "no_affordable_task"
	case StopCapabilityUnavailable:
		return RunFailed, "capability_unavailable"
	}
	if errors.Is(runErr, ErrHumanApprovalRequired) {
		return RunWaitingHuman, "human_approval_required"
	}
	if errors.Is(runErr, ErrBudgetExhausted) {
		return RunFailed, "workflow_budget_exhausted"
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		return RunTimedOut, "workflow_timeout"
	}
	if errors.Is(runErr, context.Canceled) {
		return RunCancelled, "workflow_cancelled"
	}
	return RunFailed, "workflow_failed"
}

func nodeResultStatus(request NodeRequest, runErr error) (RunStatus, string) {
	if errors.Is(runErr, ErrHumanApprovalRequired) {
		return RunWaitingHuman, "human_approval_required"
	}
	if errors.Is(runErr, ErrNoAffordableTask) {
		return RunFailed, "no_affordable_task"
	}
	if errors.Is(runErr, ErrBudgetExhausted) {
		return RunFailed, "workflow_budget_exhausted"
	}
	if errors.Is(runErr, context.DeadlineExceeded) {
		return RunTimedOut, "node_timeout"
	}
	if errors.Is(runErr, context.Canceled) {
		return RunCancelled, "workflow_cancelled"
	}
	if retryableNodeFailure(request, runErr) {
		return RunFailed, nodeRetryableErrorCode
	}
	return RunFailed, "node_failed"
}
