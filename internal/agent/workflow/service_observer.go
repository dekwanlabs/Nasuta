package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const limitationsDetailPersistFailedCode = "limitations_detail_persist_failed"

type workflowArtifactPersistenceError struct {
	code string
	err  error
}

func (err *workflowArtifactPersistenceError) Error() string {
	return err.err.Error()
}

func (err *workflowArtifactPersistenceError) Unwrap() error {
	return err.err
}

func persistenceFailureCode(err error) string {
	var artifactErr *workflowArtifactPersistenceError
	if errors.As(err, &artifactErr) && artifactErr.code != "" {
		return artifactErr.code
	}
	return "node_persistence_failed"
}

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
	err := observer.store.StartNode(persistCtx, NodeRunRecord{
		WorkflowRunID: request.WorkflowRunID, NodeID: request.Node.ID, Attempt: request.Attempt,
		Kind: request.Node.Kind, InputHandoffIDs: inputIDs, Status: RunRunning,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("%w: start node %q: %w", ErrNodePersistence, request.Node.ID, err)
	}
	return nil
}

func (observer *storeRunObserver) NodeSucceeded(
	ctx context.Context,
	request NodeRequest,
	result NodeResult,
	decision *GateDecision,
) error {
	// Secondary artifacts are persisted before the node's success transition.
	// This makes detail retention part of the success contract rather than a
	// best-effort side effect.
	for _, artifact := range result.Handoff.Artifacts {
		persistCtx, cancel := persistenceContext(ctx)
		err := observer.store.PutWorkflowArtifact(persistCtx, artifact)
		cancel()
		if err == nil {
			continue
		}
		persistErr := fmt.Errorf(
			"persist workflow artifact %q: %w", artifact.ID, err,
		)
		if artifact.Kind == LimitationsDetailArtifactKind {
			persistErr = &workflowArtifactPersistenceError{
				code: limitationsDetailPersistFailedCode,
				err:  persistErr,
			}
		}
		return observer.closeAfterPersistenceFailure(
			ctx, request, result, persistErr,
		)
	}

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
	return observer.closeAfterPersistenceFailure(ctx, request, result, err)
}

func (observer *storeRunObserver) closeAfterPersistenceFailure(
	ctx context.Context,
	request NodeRequest,
	result NodeResult,
	persistErr error,
) error {
	failCtx, failCancel := persistenceContext(ctx)
	failErr := observer.store.FailNode(
		failCtx,
		request.WorkflowRunID,
		request.Node.ID,
		request.Attempt,
		result.AgentRunID,
		RunFailed,
		persistenceFailureCode(persistErr),
		result.Usage,
		time.Now().UTC(),
	)
	failCancel()
	if failErr != nil {
		return errors.Join(
			fmt.Errorf("%w: persist node %q success: %w", ErrNodePersistence, request.Node.ID, persistErr),
			fmt.Errorf("close node after success persistence failure: %w", failErr),
		)
	}
	return fmt.Errorf(
		"%w: persist node %q success: %w",
		ErrNodePersistence,
		request.Node.ID,
		persistErr,
	)
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
	err := observer.store.FailNode(
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
	if err != nil {
		return fmt.Errorf("%w: fail node %q: %w", ErrNodePersistence, request.Node.ID, err)
	}
	return nil
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
	case StopEvidenceInsufficient:
		return RunFailed, "evidence_insufficient"
	}
	if errors.Is(runErr, ErrHumanApprovalRequired) {
		return RunWaitingHuman, "human_approval_required"
	}
	if errors.Is(runErr, ErrNodePersistence) {
		return RunFailed, persistenceFailureCode(runErr)
	}
	if errors.Is(runErr, ErrRunPersistence) {
		return RunFailed, "workflow_persistence_failed"
	}
	if errors.Is(runErr, ErrConflict) {
		return RunFailed, "workflow_conflict"
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
	switch errorStopReason(runErr) {
	case StopNeedsClarification:
		return RunFailed, "needs_clarification"
	case StopNoAffordableTask:
		return RunFailed, "no_affordable_task"
	case StopCapabilityUnavailable:
		return RunFailed, "capability_unavailable"
	case StopEvidenceInsufficient:
		return RunFailed, "evidence_insufficient"
	}
	if errors.Is(runErr, ErrHumanApprovalRequired) {
		return RunWaitingHuman, "human_approval_required"
	}
	if errors.Is(runErr, ErrNoAffordableTask) {
		return RunFailed, "no_affordable_task"
	}
	if errors.Is(runErr, ErrBudgetExhausted) {
		return RunFailed, "workflow_budget_exhausted"
	}
	if errors.Is(runErr, ErrNodePersistence) {
		return RunFailed, persistenceFailureCode(runErr)
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
