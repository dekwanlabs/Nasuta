package featuredelivery

import (
	"context"
	"errors"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestExecuteReviewAssignmentAttemptRetriesAndReusesSuccess(t *testing.T) {
	store := executionReviewFixture(t)
	service := NewService(store, nil, time.Second)
	now := time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC)
	service.now = func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	calls := 0
	service.SetReviewRunner(reviewRunnerFunc(func(
		_ context.Context,
		request ReviewAssignmentRequest,
	) (ReviewReport, error) {
		calls++
		if calls == 1 {
			return ReviewReport{}, runtimeReviewError{
				code: "temporary", message: "try again", retryable: true,
			}
		}
		return executionReviewReport(request, false), nil
	}))
	if _, err := service.StartReviewWorkflow(
		context.Background(),
		store.round.ID,
		"review.round-1",
		true,
	); err != nil {
		t.Fatal(err)
	}
	reviewerID := store.policy.Reviewers[0].ID
	actor := agentapi.Actor{UserID: 7}

	if _, err := service.ExecuteReviewAssignmentAttempt(
		context.Background(),
		store.round.ID,
		reviewerID,
		"review.round-1",
		"reviewagent.first",
		1,
		actor,
		true,
	); err == nil {
		t.Fatal("first attempt unexpectedly succeeded")
	}
	report, err := service.ExecuteReviewAssignmentAttempt(
		context.Background(),
		store.round.ID,
		reviewerID,
		"review.round-1",
		"reviewagent.second",
		2,
		actor,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := service.ExecuteReviewAssignmentAttempt(
		context.Background(),
		store.round.ID,
		reviewerID,
		"review.round-1",
		"reviewagent.third",
		3,
		actor,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || reused.ID != report.ID || reused.ContentHash != report.ContentHash {
		t.Fatalf("calls = %d, report = %+v, reused = %+v", calls, report, reused)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	attempts := make([]ReviewAssignment, 0, 2)
	for _, assignment := range store.assignments {
		if assignment.ReviewerID == reviewerID {
			attempts = append(attempts, assignment)
		}
	}
	if len(attempts) != 2 ||
		attempts[0].Attempt != 1 || attempts[0].Status != AssignmentFailed ||
		attempts[1].Attempt != 2 || attempts[1].Status != AssignmentSucceeded ||
		attempts[1].AgentRunID != "reviewagent.second" {
		t.Fatalf("attempts = %+v", attempts)
	}
}

func TestExecuteReviewAssignmentAttemptSkipsReusedAssignment(t *testing.T) {
	store := executionReviewFixture(t)
	targetAssignment := store.assignments[0]
	sourceAssignment := targetAssignment
	sourceAssignment.ID = "source-assignment"
	sourceAssignment.RoundID = "source-round"
	sourceAssignment.Status = AssignmentRunning
	source, err := PrepareReviewReport(ReviewReport{
		RoundID: sourceAssignment.RoundID, AssignmentID: sourceAssignment.ID,
		ReviewerID:  sourceAssignment.ReviewerID,
		SubjectHash: store.round.Subject.ContentHash,
		Coverage: []CoverageItem{{
			Category: sourceAssignment.Categories[0], Covered: true,
		}},
		Summary:     "The immutable subject is unchanged.",
		CompletedAt: time.Date(2026, 8, 6, 14, 0, 0, 0, time.UTC),
	}, sourceAssignment, store.round.Subject.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	target, err := PrepareReusedReviewReport(
		source,
		targetAssignment,
		store.round.Subject.ContentHash,
		ReviewReportReuseRef{
			SourceReportID: source.ID, SourceRoundID: source.RoundID,
			SourceAssignmentID: source.AssignmentID,
			Reason:             "The review inputs are unchanged.",
		},
		time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	store.assignments[0].Status = AssignmentReused
	store.assignments[0].CompletedAt = &target.CompletedAt
	store.reports = append(store.reports, target)
	calls := 0
	service := NewService(store, nil, time.Second)
	service.SetReviewRunner(reviewRunnerFunc(func(
		context.Context,
		ReviewAssignmentRequest,
	) (ReviewReport, error) {
		calls++
		return ReviewReport{}, nil
	}))
	if _, err := service.StartReviewWorkflow(
		context.Background(),
		store.round.ID,
		"review.round-1",
		true,
	); err != nil {
		t.Fatal(err)
	}
	got, err := service.ExecuteReviewAssignmentAttempt(
		context.Background(),
		store.round.ID,
		targetAssignment.ReviewerID,
		"review.round-1",
		"reviewagent.reused",
		1,
		agentapi.Actor{UserID: 7},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || got.ID != target.ID || got.ReportHash != source.ReportHash {
		t.Fatalf("calls = %d, report = %+v", calls, got)
	}
}

func TestReviewWorkflowCompletesFullyReusedPanelWithoutRunner(t *testing.T) {
	store := executionReviewFixture(t)
	reusedAt := time.Date(2026, 8, 7, 14, 0, 0, 0, time.UTC)
	for index := range store.assignments {
		targetAssignment := store.assignments[index]
		sourceAssignment := targetAssignment
		sourceAssignment.ID = "source-" + targetAssignment.ID
		sourceAssignment.RoundID = "source-round"
		sourceAssignment.Status = AssignmentRunning
		source, err := PrepareReviewReport(ReviewReport{
			RoundID: sourceAssignment.RoundID, AssignmentID: sourceAssignment.ID,
			ReviewerID:  sourceAssignment.ReviewerID,
			SubjectHash: store.round.Subject.ContentHash,
			Coverage: []CoverageItem{{
				Category: sourceAssignment.Categories[0], Covered: true,
			}},
			Summary:     "The immutable review inputs are unchanged.",
			CompletedAt: reusedAt.Add(-time.Hour),
		}, sourceAssignment, store.round.Subject.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		target, err := PrepareReusedReviewReport(
			source,
			targetAssignment,
			store.round.Subject.ContentHash,
			ReviewReportReuseRef{
				SourceReportID: source.ID, SourceRoundID: source.RoundID,
				SourceAssignmentID: source.AssignmentID,
				Reason:             "The review inputs are unchanged.",
			},
			reusedAt,
		)
		if err != nil {
			t.Fatal(err)
		}
		store.assignments[index].Status = AssignmentReused
		store.assignments[index].CompletedAt = &target.CompletedAt
		store.reports = append(store.reports, target)
	}
	service := NewService(store, nil, time.Second)
	snapshot, err := service.StartReviewWorkflow(
		context.Background(),
		store.round.ID,
		"review.round-reused",
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Round.Status != RoundRunning {
		t.Fatalf("round status = %s, want %s", snapshot.Round.Status, RoundRunning)
	}
	for _, assignment := range snapshot.Assignments {
		report, err := service.ExecuteReviewAssignmentAttempt(
			context.Background(),
			store.round.ID,
			assignment.ReviewerID,
			"review.round-reused",
			"reviewagent."+assignment.ReviewerID,
			1,
			agentapi.Actor{UserID: 7},
			true,
		)
		if err != nil {
			t.Fatal(err)
		}
		if report.AssignmentID != assignment.ID || report.ReportHash == "" {
			t.Fatalf("report = %+v, assignment = %+v", report, assignment)
		}
	}
	reports, err := service.EvaluateReviewWorkflow(
		context.Background(),
		store.round.ID,
		agentapi.Actor{UserID: 7},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != len(snapshot.Assignments) {
		t.Fatalf("reports = %d, want %d", len(reports), len(snapshot.Assignments))
	}
	result, err := service.CompleteReviewWorkflow(
		context.Background(),
		store.round.ID,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GatePass {
		t.Fatalf("decision = %s, want %s", result.Decision, GatePass)
	}
}

func TestEvaluateReviewWorkflowResumesEvaluatingRound(t *testing.T) {
	store := executionReviewFixture(t)
	store.round.Status = RoundEvaluating
	store.round.WorkflowRunID = "review.round-1"
	for index := range store.assignments {
		assignment := &store.assignments[index]
		assignment.Status = AssignmentRunning
		report := executionReviewReport(ReviewAssignmentRequest{
			Round: store.round, Policy: store.policy, Assignment: *assignment,
		}, false)
		report.CompletedAt = time.Date(2026, 8, 6, 15, index, 0, 0, time.UTC)
		prepared, err := PrepareReviewReport(
			report,
			*assignment,
			store.round.Subject.ContentHash,
		)
		if err != nil {
			t.Fatal(err)
		}
		store.reports = append(store.reports, prepared)
		assignment.Status = AssignmentSucceeded
		assignment.CompletedAt = &prepared.CompletedAt
	}
	service := NewService(store, nil, time.Second)

	reports, err := service.EvaluateReviewWorkflow(
		context.Background(),
		store.round.ID,
		agentapi.Actor{UserID: 7},
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != len(store.policy.Reviewers) {
		t.Fatalf("reports = %d, want %d", len(reports), len(store.policy.Reviewers))
	}
	result, err := service.CompleteReviewWorkflow(
		context.Background(),
		store.round.ID,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GatePass {
		t.Fatalf("decision = %s, want %s", result.Decision, GatePass)
	}
	again, err := service.CompleteReviewWorkflow(
		context.Background(),
		store.round.ID,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if again.ContentHash != result.ContentHash {
		t.Fatalf("idempotent gate = %+v, want %+v", again, result)
	}
}

func TestFailReviewWorkflowKeepsTerminalRoundStable(t *testing.T) {
	store := executionReviewFixture(t)
	store.round.Status = RoundCompleted
	service := NewService(store, nil, time.Second)

	if err := service.FailReviewWorkflow(
		context.Background(),
		store.round.ID,
		errors.New("late failure"),
		true,
	); err != nil {
		t.Fatal(err)
	}
	if store.round.Status != RoundCompleted {
		t.Fatalf("round status = %s, want %s", store.round.Status, RoundCompleted)
	}
}
