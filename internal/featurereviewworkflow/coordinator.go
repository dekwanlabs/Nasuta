package featurereviewworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
)

type workflowService interface {
	Execute(context.Context, agentworkflow.ExecuteRequest) (agentworkflow.Result, error)
	Resume(context.Context, string) (agentworkflow.ResumeResult, error)
	GetRun(context.Context, string, int64, bool) (*agentworkflow.WorkflowRunRecord, error)
	Cancel(context.Context, string, int64, bool) (agentworkflow.CancelTransition, error)
	PublishDefinitions([]agentworkflow.WorkflowDefinition, bool) error
}

type reviewCoordinatorService interface {
	LoadReviewWorkflowSnapshot(context.Context, string, bool) (*featuredelivery.ReviewWorkflowSnapshot, error)
	StartReviewWorkflow(context.Context, string, string, bool) (*featuredelivery.ReviewWorkflowSnapshot, error)
	CompleteReviewWorkflow(context.Context, string, bool) (*featuredelivery.ReviewGateResult, error)
	FailReviewWorkflow(context.Context, string, error, bool) error
	CancelReviewRound(context.Context, string, bool) error
}

// Coordinator keeps Review Round and Workflow terminal states convergent.
type Coordinator struct {
	reviews   reviewCoordinatorService
	workflows workflowService
}

func NewCoordinator(
	reviews reviewCoordinatorService,
	workflows workflowService,
) *Coordinator {
	return &Coordinator{reviews: reviews, workflows: workflows}
}

// ReconcileRecoveredRun mirrors one recovered Workflow terminal state into Review.
func (coordinator *Coordinator) ReconcileRecoveredRun(
	ctx context.Context,
	runID string,
	status agentworkflow.RunStatus,
	cause error,
) (bool, error) {
	if !strings.HasPrefix(strings.TrimSpace(runID), runIDPrefix) {
		return false, nil
	}
	if coordinator == nil || coordinator.reviews == nil {
		return true, featuredelivery.ErrUnavailable
	}
	roundID, err := roundIDFromRunID(runID)
	if err != nil {
		return true, err
	}
	switch status {
	case agentworkflow.RunSucceeded:
		_, err = coordinator.reviews.CompleteReviewWorkflow(
			context.WithoutCancel(ctx),
			roundID,
			true,
		)
	case agentworkflow.RunCancelled:
		err = coordinator.reviews.CancelReviewRound(
			context.WithoutCancel(ctx),
			roundID,
			true,
		)
	case agentworkflow.RunFailed, agentworkflow.RunTimedOut:
		if cause == nil {
			cause = fmt.Errorf("review workflow recovery ended with status %q", status)
		}
		err = coordinator.reviews.FailReviewWorkflow(
			context.WithoutCancel(ctx),
			roundID,
			cause,
			true,
		)
	default:
		return false, nil
	}
	return true, err
}

func (coordinator *Coordinator) Execute(
	ctx context.Context,
	roundID string,
	actor agentapi.Actor,
	admin bool,
) (*featuredelivery.ReviewGateResult, error) {
	if coordinator == nil || coordinator.reviews == nil || coordinator.workflows == nil {
		return nil, featuredelivery.ErrUnavailable
	}
	if !admin {
		return nil, featuredelivery.ErrForbidden
	}
	if actor.UserID <= 0 {
		return nil, featuredelivery.ErrInvalid
	}
	snapshot, err := coordinator.reviews.LoadReviewWorkflowSnapshot(ctx, roundID, true)
	if err != nil {
		return nil, err
	}
	if snapshot.Round.Status == featuredelivery.RoundCompleted {
		return coordinator.reviews.CompleteReviewWorkflow(ctx, roundID, true)
	}
	definition, err := DefinitionForRound(snapshot.Policy, snapshot.Round)
	if err != nil {
		return nil, err
	}
	if err := coordinator.workflows.PublishDefinitions(
		[]agentworkflow.WorkflowDefinition{definition},
		true,
	); err != nil {
		return nil, err
	}
	runID, err := RunID(snapshot.Round.ID)
	if err != nil {
		return nil, err
	}
	if snapshot.Round.WorkflowRunID != "" && snapshot.Round.WorkflowRunID != runID {
		return nil, fmt.Errorf(
			"review round %q is bound to workflow run %q, expected %q: %w",
			roundID, snapshot.Round.WorkflowRunID, runID, featuredelivery.ErrConflict,
		)
	}
	switch snapshot.Round.Status {
	case featuredelivery.RoundCreated:
		if _, err := coordinator.reviews.StartReviewWorkflow(ctx, roundID, runID, true); err != nil {
			return nil, err
		}
		return coordinator.executeNew(ctx, definition, roundID, runID, actor)
	case featuredelivery.RoundRunning, featuredelivery.RoundEvaluating:
		return coordinator.resume(ctx, definition, roundID, runID, actor)
	case featuredelivery.RoundFailed, featuredelivery.RoundCancelled:
		return nil, featuredelivery.ErrConflict
	default:
		return nil, featuredelivery.ErrConflict
	}
}

func (coordinator *Coordinator) Cancel(
	ctx context.Context,
	roundID string,
	actor agentapi.Actor,
	admin bool,
) error {
	if coordinator == nil || coordinator.reviews == nil || coordinator.workflows == nil {
		return featuredelivery.ErrUnavailable
	}
	if !admin {
		return featuredelivery.ErrForbidden
	}
	if actor.UserID <= 0 {
		return featuredelivery.ErrInvalid
	}
	snapshot, err := coordinator.reviews.LoadReviewWorkflowSnapshot(ctx, roundID, true)
	if err != nil {
		return err
	}
	if snapshot.Round.WorkflowRunID != "" {
		if _, err := coordinator.workflows.Cancel(
			ctx,
			snapshot.Round.WorkflowRunID,
			actor.UserID,
			true,
		); err != nil && !errors.Is(err, agentworkflow.ErrNotFound) {
			return err
		}
	}
	return coordinator.reviews.CancelReviewRound(ctx, roundID, true)
}

func (coordinator *Coordinator) executeNew(
	ctx context.Context,
	definition agentworkflow.WorkflowDefinition,
	roundID, runID string,
	actor agentapi.Actor,
) (*featuredelivery.ReviewGateResult, error) {
	input, err := json.Marshal(Request{RoundID: roundID})
	if err != nil {
		return nil, fmt.Errorf("marshal feature review request: %w", err)
	}
	permissions := agentapi.PermissionPolicy{Scopes: []string{platformscope.FeatureDelivery}}
	_, runErr := coordinator.workflows.Execute(ctx, agentworkflow.ExecuteRequest{
		RunID:               runID,
		Workflow:            agentworkflow.DefinitionRef{ID: definition.ID, Version: definition.Version},
		Input:               input,
		Actor:               actor,
		ActorPermissions:    permissions,
		Scenario:            "feature.review",
		ScenarioPermissions: permissions,
	})
	if runErr != nil {
		return nil, coordinator.syncFailure(ctx, roundID, runID, actor.UserID, runErr)
	}
	return coordinator.reviews.CompleteReviewWorkflow(ctx, roundID, true)
}

func (coordinator *Coordinator) resume(
	ctx context.Context,
	definition agentworkflow.WorkflowDefinition,
	roundID, runID string,
	actor agentapi.Actor,
) (*featuredelivery.ReviewGateResult, error) {
	run, err := coordinator.workflows.GetRun(ctx, runID, actor.UserID, true)
	if errors.Is(err, agentworkflow.ErrNotFound) {
		return coordinator.executeNew(ctx, definition, roundID, runID, actor)
	}
	if err != nil {
		return nil, err
	}
	switch run.Status {
	case agentworkflow.RunRunning:
		resumed, resumeErr := coordinator.workflows.Resume(ctx, runID)
		if resumeErr != nil {
			return nil, coordinator.syncFailure(ctx, roundID, runID, actor.UserID, resumeErr)
		}
		if resumed.Status != agentworkflow.RunSucceeded {
			return nil, coordinator.syncTerminal(ctx, roundID, resumed.Status, nil)
		}
	case agentworkflow.RunSucceeded:
	case agentworkflow.RunCancelled, agentworkflow.RunFailed, agentworkflow.RunTimedOut:
		return nil, coordinator.syncTerminal(ctx, roundID, run.Status, nil)
	default:
		return nil, fmt.Errorf(
			"review workflow run %q has unsupported status %q: %w",
			runID, run.Status, featuredelivery.ErrConflict,
		)
	}
	return coordinator.reviews.CompleteReviewWorkflow(ctx, roundID, true)
}

func (coordinator *Coordinator) syncFailure(
	ctx context.Context,
	roundID, runID string,
	userID int64,
	cause error,
) error {
	run, err := coordinator.workflows.GetRun(
		context.WithoutCancel(ctx), runID, userID, true,
	)
	if err == nil {
		return coordinator.syncTerminal(ctx, roundID, run.Status, cause)
	}
	if failErr := coordinator.reviews.FailReviewWorkflow(
		context.WithoutCancel(ctx), roundID, cause, true,
	); failErr != nil {
		return errors.Join(cause, failErr, err)
	}
	return cause
}

func (coordinator *Coordinator) syncTerminal(
	ctx context.Context,
	roundID string,
	status agentworkflow.RunStatus,
	cause error,
) error {
	if cause == nil {
		cause = fmt.Errorf("review workflow ended with status %q", status)
	}
	switch status {
	case agentworkflow.RunCancelled:
		if err := coordinator.reviews.CancelReviewRound(
			context.WithoutCancel(ctx), roundID, true,
		); err != nil {
			return errors.Join(cause, err)
		}
	case agentworkflow.RunFailed, agentworkflow.RunTimedOut:
		if err := coordinator.reviews.FailReviewWorkflow(
			context.WithoutCancel(ctx), roundID, cause, true,
		); err != nil {
			return errors.Join(cause, err)
		}
	case agentworkflow.RunSucceeded:
		return cause
	default:
		return errors.Join(cause, featuredelivery.ErrConflict)
	}
	return cause
}
