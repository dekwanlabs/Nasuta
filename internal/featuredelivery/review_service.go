package featuredelivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

type ReviewAssignmentRequest struct {
	Round      ReviewRound
	Policy     ReviewPolicy
	Assignment ReviewAssignment
	Actor      agentapi.Actor
}

// ReviewRunner executes one isolated reviewer without access to peer reports.
type ReviewRunner interface {
	Run(context.Context, ReviewAssignmentRequest) (ReviewReport, error)
}

// RuntimeReviewRunner adapts the generic Agent Runtime to structured reviews.
type RuntimeReviewRunner struct {
	runtime agentapi.Runtime
}

func NewRuntimeReviewRunner(runtime agentapi.Runtime) *RuntimeReviewRunner {
	return &RuntimeReviewRunner{runtime: runtime}
}

func (runner *RuntimeReviewRunner) Run(ctx context.Context, request ReviewAssignmentRequest) (ReviewReport, error) {
	if runner == nil || runner.runtime == nil {
		return ReviewReport{}, ErrUnavailable
	}
	input, err := json.Marshal(struct {
		Subject    ReviewSubject `json:"subject"`
		Categories []string      `json:"categories"`
		PolicyHash string        `json:"policy_hash"`
	}{
		Subject: request.Round.Subject, Categories: request.Assignment.Categories,
		PolicyHash: request.Policy.ContentHash,
	})
	if err != nil {
		return ReviewReport{}, fmt.Errorf("marshal reviewer input: %w", err)
	}
	result, err := runner.runtime.Run(ctx, agentapi.RunRequest{
		RunID: request.Assignment.ID,
		Agent: request.Assignment.Agent,
		Input: input,
		Actor: request.Actor,
		Correlation: agentapi.Correlation{
			WorkflowRunID: request.Round.ID,
			NodeID:        request.Assignment.ReviewerID,
		},
	})
	if err != nil {
		return ReviewReport{}, err
	}
	if result.Status != agentapi.RunSucceeded {
		if result.Error != nil {
			return ReviewReport{}, fmt.Errorf("reviewer %q failed (%s): %s", request.Assignment.ReviewerID, result.Error.Code, result.Error.Message)
		}
		return ReviewReport{}, fmt.Errorf("reviewer %q ended with status %q", request.Assignment.ReviewerID, result.Status)
	}
	raw := result.Output
	if len(raw) == 0 {
		raw = json.RawMessage(result.Text)
	}
	var report ReviewReport
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return ReviewReport{}, fmt.Errorf("decode reviewer %q report: %w", request.Assignment.ReviewerID, err)
	}
	report.RoundID = request.Round.ID
	report.AssignmentID = request.Assignment.ID
	report.ReviewerID = request.Assignment.ReviewerID
	report.SubjectHash = request.Round.Subject.ContentHash
	return report, nil
}

func (service *Service) SetReviewRunner(runner ReviewRunner) {
	service.reviewer = runner
}

func (service *Service) CreateArtifactReviewRound(
	ctx context.Context,
	requestID, artifactID string,
	policy ReviewPolicy,
	userID int64,
	admin bool,
) (*ReviewRound, []ReviewAssignment, error) {
	artifact, err := service.GetArtifact(ctx, requestID, artifactID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	subject, err := BuildArtifactReviewSubject(*artifact)
	if err != nil {
		return nil, nil, err
	}
	return service.createReviewRound(ctx, subject, policy, userID)
}

func (service *Service) CreateChangeSetReviewRound(
	ctx context.Context,
	runID string,
	policy ReviewPolicy,
	userID int64,
	admin bool,
) (*ReviewRound, []ReviewAssignment, error) {
	run, err := service.GetImplementation(ctx, runID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	if run.Status != RunSucceeded {
		return nil, nil, ErrConflict
	}
	subject, err := BuildChangeSetReviewSubject(*run)
	if err != nil {
		return nil, nil, err
	}
	return service.createReviewRound(ctx, subject, policy, userID)
}

func (service *Service) createReviewRound(
	ctx context.Context,
	subject ReviewSubject,
	policy ReviewPolicy,
	userID int64,
) (*ReviewRound, []ReviewAssignment, error) {
	if service == nil || service.store == nil {
		return nil, nil, ErrUnavailable
	}
	prepared, err := PrepareReviewPolicy(policy)
	if err != nil {
		return nil, nil, err
	}
	if prepared.SubjectKind != subject.Kind {
		return nil, nil, fmt.Errorf("review policy does not match subject kind: %w", ErrConflict)
	}
	now := service.now()
	if prepared.CreatedAt.IsZero() {
		prepared.CreatedAt = now
	}
	if err := service.store.SaveReviewPolicy(ctx, prepared); err != nil {
		return nil, nil, err
	}
	roundID, err := NewID("round")
	if err != nil {
		return nil, nil, err
	}
	round := ReviewRound{
		ID: roundID, Subject: subject,
		PolicyID: prepared.ID, PolicyVersion: prepared.Version, PolicyHash: prepared.ContentHash,
		Status: RoundCreated, CreatedBy: userID, CreatedAt: now,
	}
	assignments := make([]ReviewAssignment, 0, len(prepared.Reviewers))
	for _, reviewer := range prepared.Reviewers {
		assignmentID, err := NewID("assignment")
		if err != nil {
			return nil, nil, err
		}
		assignments = append(assignments, ReviewAssignment{
			ID: assignmentID, RoundID: round.ID, ReviewerID: reviewer.ID,
			Agent: reviewer.Agent, DefinitionHash: reviewer.DefinitionHash,
			Categories: append([]string(nil), reviewer.Categories...), Required: reviewer.Required,
			Status: AssignmentQueued, Attempt: 1, CreatedAt: now,
		})
	}
	if err := service.store.CreateReviewRound(ctx, round, assignments); err != nil {
		return nil, nil, err
	}
	return &round, assignments, nil
}

func (service *Service) ExecuteReviewRound(ctx context.Context, roundID string, actor agentapi.Actor) (*ReviewGateResult, error) {
	if service == nil || service.store == nil || service.reviewer == nil {
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
	now := service.now()
	if err := service.store.TransitionReviewRound(ctx, round.ID, RoundCreated, RoundRunning, now); err != nil {
		return nil, err
	}
	round.Status = RoundRunning
	assignments, err := service.store.ListReviewAssignments(ctx, round.ID, ReviewAssignmentCursor{}, maxReviewersPerPolicy)
	if err != nil {
		return nil, err
	}
	if len(assignments) != len(policy.Reviewers) {
		return nil, fmt.Errorf("review round assignment snapshot is incomplete: %w", ErrConflict)
	}

	sem := make(chan struct{}, policy.MaxParallelism)
	var wg sync.WaitGroup
	for index := range assignments {
		assignment := assignments[index]
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			startedAt := service.now()
			if err := service.store.TransitionReviewAssignment(
				ctx, assignment.ID, AssignmentQueued, AssignmentRunning, assignment.ID, "", startedAt,
			); err != nil {
				return
			}
			assignment.Status = AssignmentRunning
			assignment.AgentRunID = assignment.ID
			report, runErr := service.reviewer.Run(ctx, ReviewAssignmentRequest{
				Round: *round, Policy: *policy, Assignment: assignment, Actor: actor,
			})
			if runErr != nil {
				_ = service.store.TransitionReviewAssignment(
					context.WithoutCancel(ctx), assignment.ID, AssignmentRunning, AssignmentFailed,
					assignment.ID, "reviewer_failed", service.now(),
				)
				return
			}
			if report.CompletedAt.IsZero() {
				report.CompletedAt = service.now()
			}
			prepared, prepareErr := PrepareReviewReport(report, assignment, round.Subject.ContentHash)
			if prepareErr != nil {
				_ = service.store.TransitionReviewAssignment(
					context.WithoutCancel(ctx), assignment.ID, AssignmentRunning, AssignmentFailed,
					assignment.ID, "invalid_report", service.now(),
				)
				return
			}
			if err := service.store.CompleteReviewAssignment(context.WithoutCancel(ctx), prepared); err != nil {
				_ = service.store.TransitionReviewAssignment(
					context.WithoutCancel(ctx), assignment.ID, AssignmentRunning, AssignmentFailed,
					assignment.ID, "report_persistence_failed", service.now(),
				)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := service.store.TransitionReviewRound(ctx, round.ID, RoundRunning, RoundEvaluating, service.now()); err != nil {
		return nil, err
	}
	evaluation, err := service.store.LoadFullReviewEvaluation(ctx, round.ID)
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
	return &result, nil
}

func (service *Service) validateApprovalBinding(
	ctx context.Context,
	subject ReviewSubject,
	binding ReviewApprovalBinding,
	decision ReviewDecision,
	comment string,
) error {
	if binding.SubjectHash == "" || binding.ReviewRoundID == "" || binding.GateResultID == "" {
		return fmt.Errorf("subject_hash, review_round_id, and gate_result_id are required: %w", ErrInvalid)
	}
	if binding.SubjectHash != subject.ContentHash {
		return fmt.Errorf("review subject is stale: %w", ErrConflict)
	}
	round, err := service.store.GetReviewRound(ctx, binding.ReviewRoundID)
	if err != nil {
		return err
	}
	if round.Status != RoundCompleted || round.Subject.ID != subject.ID ||
		round.Subject.Kind != subject.Kind || round.Subject.ContentHash != subject.ContentHash {
		return fmt.Errorf("review round does not match current subject: %w", ErrConflict)
	}
	gate, err := service.store.GetReviewGateResult(ctx, binding.GateResultID)
	if err != nil {
		return err
	}
	if gate.RoundID != round.ID || gate.SubjectHash != subject.ContentHash || !validGateDecision(gate.Decision) {
		return fmt.Errorf("gate result does not match review round: %w", ErrConflict)
	}
	if decision == DecisionRejected {
		return nil
	}
	switch gate.Decision {
	case GatePass:
		return nil
	case GateHumanRequired:
		if comment == "" {
			return fmt.Errorf("human-required approval needs an explicit disposition: %w", ErrConflict)
		}
		return nil
	case GateRevise:
		resolutions, err := service.store.ListFindingResolutionsByIDs(ctx, gate.BlockingIDs, subject.ContentHash)
		if err != nil {
			return err
		}
		waived := make(map[string]struct{}, len(resolutions))
		now := service.now()
		for _, resolution := range resolutions {
			if resolution.Resolution != ResolutionWaived ||
				(resolution.ExpiresAt != nil && !resolution.ExpiresAt.After(now)) {
				continue
			}
			waived[resolution.FindingID] = struct{}{}
		}
		for _, findingID := range gate.BlockingIDs {
			if _, ok := waived[findingID]; !ok {
				return fmt.Errorf("blocking finding %q is not waived: %w", findingID, ErrConflict)
			}
		}
		return nil
	case GateIncomplete, GateFailed:
		return fmt.Errorf("gate decision %q cannot be approved: %w", gate.Decision, ErrConflict)
	default:
		return fmt.Errorf("unsupported gate decision %q: %w", gate.Decision, ErrConflict)
	}
}

func reviewErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "reviewer_timeout"
	case errors.Is(err, context.Canceled):
		return "reviewer_cancelled"
	default:
		return "reviewer_failed"
	}
}
