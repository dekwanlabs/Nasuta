package featuredelivery

import (
	"context"
	"errors"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// ReviewWorkflowSnapshot pins the domain facts used by one Workflow definition.
type ReviewWorkflowSnapshot struct {
	Round       ReviewRound
	Policy      ReviewPolicy
	Assignments []ReviewAssignment
}

// LoadReviewWorkflowSnapshot resolves the latest Attempt for every pinned reviewer.
func (service *Service) LoadReviewWorkflowSnapshot(
	ctx context.Context,
	roundID string,
	admin bool,
) (*ReviewWorkflowSnapshot, error) {
	if !admin {
		return nil, ErrForbidden
	}
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	round, err := service.store.GetReviewRound(ctx, roundID)
	if err != nil {
		return nil, err
	}
	policy, err := service.store.GetReviewPolicy(ctx, round.PolicyID, round.PolicyVersion)
	if err != nil {
		return nil, err
	}
	if policy.ContentHash != round.PolicyHash || policy.SubjectKind != round.Subject.Kind {
		return nil, fmt.Errorf("review round policy snapshot does not match published policy: %w", ErrConflict)
	}
	if err := ValidateReviewRoundSnapshot(*policy, *round); err != nil {
		return nil, err
	}
	assignments := make([]ReviewAssignment, 0, len(round.Reviewers))
	for _, reviewer := range round.Reviewers {
		assignment, err := service.store.GetLatestReviewAssignment(ctx, round.ID, reviewer.ID)
		if err != nil {
			return nil, err
		}
		if err := validateLatestReviewAssignment(reviewer, *assignment); err != nil {
			return nil, err
		}
		assignments = append(assignments, *assignment)
	}
	return &ReviewWorkflowSnapshot{
		Round: *round, Policy: *policy, Assignments: assignments,
	}, nil
}

// StartReviewWorkflow fixes the Workflow binding before any parallel node runs.
func (service *Service) StartReviewWorkflow(
	ctx context.Context,
	roundID, workflowRunID string,
	admin bool,
) (*ReviewWorkflowSnapshot, error) {
	snapshot, err := service.LoadReviewWorkflowSnapshot(ctx, roundID, admin)
	if err != nil {
		return nil, err
	}
	if reviewAssignmentsNeedRunner(snapshot.Assignments) && service.reviewRunner() == nil {
		return nil, ErrUnavailable
	}
	if err := service.store.BindReviewRoundWorkflow(
		ctx,
		snapshot.Round.ID,
		workflowRunID,
		service.now(),
	); err != nil {
		return nil, err
	}
	snapshot.Round.WorkflowRunID = workflowRunID
	switch snapshot.Round.Status {
	case RoundCreated:
		if err := service.store.TransitionReviewRound(
			ctx,
			snapshot.Round.ID,
			RoundCreated,
			RoundRunning,
			service.now(),
		); err != nil {
			return nil, err
		}
		snapshot.Round.Status = RoundRunning
		if _, err := service.appendReviewEvent(
			context.WithoutCancel(ctx),
			snapshot.Round.ID,
			ReviewEventRoundStarted,
			"review round started",
			nil,
		); err != nil {
			return nil, service.failReviewRound(ctx, snapshot.Round.ID, RoundRunning, err)
		}
	case RoundRunning, RoundEvaluating, RoundCompleted:
	case RoundFailed, RoundCancelled:
		return nil, ErrConflict
	default:
		return nil, ErrConflict
	}
	return snapshot, nil
}

// ExecuteReviewAssignmentAttempt runs one Workflow Attempt and preserves prior success.
func (service *Service) ExecuteReviewAssignmentAttempt(
	ctx context.Context,
	roundID, reviewerID, workflowRunID, agentRunID string,
	attempt int,
	actor agentapi.Actor,
	admin bool,
) (*ReviewReport, error) {
	report, _, err := service.ExecuteReviewAssignmentAttemptWithUsage(
		ctx, roundID, reviewerID, workflowRunID, agentRunID, attempt, actor, admin,
	)
	return report, err
}

// ExecuteReviewAssignmentAttemptWithUsage returns the report and model usage for one Attempt.
func (service *Service) ExecuteReviewAssignmentAttemptWithUsage(
	ctx context.Context,
	roundID, reviewerID, workflowRunID, agentRunID string,
	attempt int,
	actor agentapi.Actor,
	admin bool,
) (*ReviewReport, ReviewUsage, error) {
	if actor.UserID <= 0 || attempt <= 0 || agentRunID == "" {
		return nil, ReviewUsage{}, ErrInvalid
	}
	snapshot, err := service.LoadReviewWorkflowSnapshot(ctx, roundID, admin)
	if err != nil {
		return nil, ReviewUsage{}, err
	}
	if snapshot.Round.WorkflowRunID != workflowRunID {
		return nil, ReviewUsage{}, fmt.Errorf("review round workflow binding changed: %w", ErrConflict)
	}
	report, err := service.store.GetSuccessfulReviewReport(ctx, roundID, reviewerID)
	if err == nil {
		if report.RoundID != roundID || report.ReviewerID != reviewerID ||
			report.SubjectHash != snapshot.Round.Subject.ContentHash {
			return nil, ReviewUsage{}, fmt.Errorf(
				"successful review report does not match workflow snapshot: %w",
				ErrConflict,
			)
		}
		return report, ReviewUsage{}, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, ReviewUsage{}, err
	}
	if snapshot.Round.Status != RoundRunning {
		return nil, ReviewUsage{}, ErrConflict
	}
	latest, ok := reviewAssignmentByReviewer(snapshot.Assignments, reviewerID)
	if !ok {
		return nil, ReviewUsage{}, ErrNotFound
	}
	alreadyRunning := latest.Attempt == attempt &&
		latest.Status == AssignmentRunning &&
		latest.AgentRunID == agentRunID
	assignment, err := service.store.StartReviewAssignmentAttempt(
		ctx,
		roundID,
		reviewerID,
		attempt,
		agentRunID,
		service.now(),
	)
	if err != nil {
		return nil, ReviewUsage{}, err
	}
	if !alreadyRunning {
		if _, err := service.appendReviewEvent(
			context.WithoutCancel(ctx),
			roundID,
			ReviewEventAssignmentStarted,
			"review assignment started",
			reviewAssignmentEventDetail(*assignment, ""),
		); err != nil {
			_ = service.store.TransitionReviewAssignment(
				context.WithoutCancel(ctx),
				assignment.ID,
				AssignmentRunning,
				AssignmentFailed,
				agentRunID,
				"event_persistence_failed",
				service.now(),
			)
			return nil, ReviewUsage{}, err
		}
	}
	runner := service.reviewRunner()
	if runner == nil {
		return nil, ReviewUsage{}, ErrUnavailable
	}
	reviewContext, err := service.buildReviewContext(ctx, snapshot.Round.Subject)
	if err != nil {
		return nil, ReviewUsage{}, err
	}
	request := ReviewAssignmentRequest{
		Round: snapshot.Round, Policy: snapshot.Policy, Assignment: *assignment,
		Context: reviewContext, Actor: actor,
	}
	var runResult ReviewRunResult
	var runErr error
	if usageRunner, ok := runner.(ReviewRunnerWithUsage); ok {
		runResult, runErr = usageRunner.RunWithUsage(ctx, request)
	} else {
		runResult.Report, runErr = runner.Run(ctx, request)
	}
	reportValue := runResult.Report
	usage := runResult.Usage
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, usage, runErr
		}
		errorCode := reviewErrorCode(runErr)
		if err := service.store.TransitionReviewAssignment(
			context.WithoutCancel(ctx),
			assignment.ID,
			AssignmentRunning,
			AssignmentFailed,
			agentRunID,
			errorCode,
			service.now(),
		); err != nil {
			return nil, usage, errors.Join(runErr, err)
		}
		if _, err := service.appendReviewEvent(
			context.WithoutCancel(ctx),
			roundID,
			ReviewEventAssignmentFailed,
			"review assignment failed",
			reviewAssignmentEventDetail(*assignment, errorCode),
		); err != nil {
			return nil, usage, errors.Join(runErr, err)
		}
		return nil, usage, runErr
	}
	if reportValue.CompletedAt.IsZero() {
		reportValue.CompletedAt = service.now()
	}
	prepared, err := PrepareReviewReport(
		reportValue,
		*assignment,
		snapshot.Round.Subject.ContentHash,
	)
	if err != nil {
		_ = service.store.TransitionReviewAssignment(
			context.WithoutCancel(ctx),
			assignment.ID,
			AssignmentRunning,
			AssignmentFailed,
			agentRunID,
			"invalid_report",
			service.now(),
		)
		return nil, usage, err
	}
	if err := service.store.CompleteReviewAssignment(
		context.WithoutCancel(ctx),
		prepared,
	); err != nil {
		return nil, usage, err
	}
	if _, err := service.appendReviewEvent(
		context.WithoutCancel(ctx),
		roundID,
		ReviewEventAssignmentSucceeded,
		"review assignment succeeded",
		reviewAssignmentEventDetail(*assignment, ""),
	); err != nil {
		return nil, usage, err
	}
	return &prepared, usage, nil
}

// EvaluateReviewWorkflow enters the durable evaluation phase and resumes adjudication.
func (service *Service) EvaluateReviewWorkflow(
	ctx context.Context,
	roundID string,
	actor agentapi.Actor,
	admin bool,
) ([]ReviewReport, error) {
	reports, _, err := service.EvaluateReviewWorkflowWithUsage(ctx, roundID, actor, admin)
	return reports, err
}

// EvaluateReviewWorkflowWithUsage returns reports and all adjudication usage.
func (service *Service) EvaluateReviewWorkflowWithUsage(
	ctx context.Context,
	roundID string,
	actor agentapi.Actor,
	admin bool,
) ([]ReviewReport, ReviewUsage, error) {
	var usage ReviewUsage
	if actor.UserID <= 0 {
		return nil, usage, ErrInvalid
	}
	snapshot, err := service.LoadReviewWorkflowSnapshot(ctx, roundID, admin)
	if err != nil {
		return nil, usage, err
	}
	switch snapshot.Round.Status {
	case RoundRunning:
		if err := service.store.TransitionReviewRound(
			ctx,
			roundID,
			RoundRunning,
			RoundEvaluating,
			service.now(),
		); err != nil {
			return nil, usage, err
		}
		if _, err := service.appendReviewEvent(
			context.WithoutCancel(ctx),
			roundID,
			ReviewEventRoundEvaluating,
			"review round evaluating",
			nil,
		); err != nil {
			return nil, usage, service.failReviewRound(ctx, roundID, RoundEvaluating, err)
		}
	case RoundEvaluating:
	default:
		return nil, usage, ErrConflict
	}
	evaluation, err := service.loadReviewEvaluation(ctx, roundID)
	if err != nil {
		return nil, usage, err
	}
	result, err := EvaluateReviewGate(evaluation, service.now())
	if err != nil {
		return nil, usage, err
	}
	if len(result.ConflictIDs) > 0 && evaluation.Policy.Adjudicator != nil {
		reviewContext, err := service.buildReviewContext(ctx, evaluation.Round.Subject)
		if err != nil {
			return nil, usage, err
		}
		adjudicationUsage, err := service.executeReviewAdjudications(
			ctx,
			service.adjudicationRunner(),
			actor,
			reviewContext,
			evaluation,
			result.ConflictIDs,
		)
		usage = addReviewUsage(usage, adjudicationUsage)
		if err != nil {
			return nil, usage, err
		}
		evaluation, err = service.loadReviewEvaluation(ctx, roundID)
		if err != nil {
			return nil, usage, err
		}
	}
	return evaluation.Reports, usage, nil
}

// CompleteReviewWorkflow persists the deterministic terminal Gate idempotently.
func (service *Service) CompleteReviewWorkflow(
	ctx context.Context,
	roundID string,
	admin bool,
) (*ReviewGateResult, error) {
	snapshot, err := service.LoadReviewWorkflowSnapshot(ctx, roundID, admin)
	if err != nil {
		return nil, err
	}
	if snapshot.Round.Status == RoundCompleted {
		return service.store.GetReviewGateResultByRound(ctx, roundID)
	}
	if snapshot.Round.Status != RoundEvaluating {
		return nil, ErrConflict
	}
	evaluation, err := service.loadReviewEvaluation(ctx, roundID)
	if err != nil {
		return nil, err
	}
	result, err := EvaluateReviewGate(evaluation, service.now())
	if err != nil {
		return nil, err
	}
	if err := service.store.CompleteReviewRound(ctx, result, service.now()); err != nil {
		return nil, err
	}
	if _, err := service.appendReviewEvent(
		context.WithoutCancel(ctx),
		roundID,
		ReviewEventRoundCompleted,
		"review round completed",
		reviewGateEventDetail(result),
	); err != nil {
		return nil, err
	}
	return &result, nil
}

// FailReviewWorkflow mirrors a terminal Workflow failure into the Review Round.
func (service *Service) FailReviewWorkflow(
	ctx context.Context,
	roundID string,
	cause error,
	admin bool,
) error {
	if !admin {
		return ErrForbidden
	}
	if cause == nil {
		return ErrInvalid
	}
	round, err := service.store.GetReviewRound(ctx, roundID)
	if err != nil {
		return err
	}
	switch round.Status {
	case RoundCreated, RoundRunning, RoundEvaluating:
		return service.failReviewRound(ctx, roundID, round.Status, cause)
	case RoundFailed, RoundCancelled, RoundCompleted:
		return nil
	default:
		return ErrConflict
	}
}

func (service *Service) loadReviewEvaluation(
	ctx context.Context,
	roundID string,
) (ReviewEvaluation, error) {
	evaluation, err := service.store.LoadFullReviewEvaluation(ctx, roundID)
	if err != nil {
		return ReviewEvaluation{}, err
	}
	if err := service.attachReviewSubjectGateFacts(ctx, &evaluation); err != nil {
		return ReviewEvaluation{}, err
	}
	return evaluation, nil
}

func validateLatestReviewAssignment(
	reviewer ReviewerSpec,
	assignment ReviewAssignment,
) error {
	if assignment.ID == "" || assignment.RoundID == "" ||
		assignment.ReviewerID != reviewer.ID ||
		assignment.Agent != reviewer.Agent ||
		assignment.DefinitionHash != reviewer.DefinitionHash ||
		assignment.Required != reviewer.Required ||
		!equalCanonicalStrings(assignment.Categories, reviewer.Categories) ||
		assignment.Attempt <= 0 {
		return fmt.Errorf(
			"review assignment %q does not match policy snapshot: %w",
			assignment.ID,
			ErrConflict,
		)
	}
	switch assignment.Status {
	case AssignmentQueued, AssignmentRunning, AssignmentSucceeded, AssignmentReused,
		AssignmentFailed, AssignmentCancelled:
		return nil
	default:
		return fmt.Errorf("review assignment %q status is invalid: %w", assignment.ID, ErrConflict)
	}
}

func reviewAssignmentByReviewer(
	assignments []ReviewAssignment,
	reviewerID string,
) (ReviewAssignment, bool) {
	for _, assignment := range assignments {
		if assignment.ReviewerID == reviewerID {
			return assignment, true
		}
	}
	return ReviewAssignment{}, false
}

func reviewAssignmentsNeedRunner(assignments []ReviewAssignment) bool {
	for _, assignment := range assignments {
		if !successfulReviewAssignment(assignment.Status) {
			return true
		}
	}
	return false
}
