package featuredelivery

import (
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestReviewLifecycleTransitions(t *testing.T) {
	if !CanTransitionReviewRound(RoundCreated, RoundRunning) ||
		!CanTransitionReviewRound(RoundRunning, RoundEvaluating) ||
		!CanTransitionReviewRound(RoundEvaluating, RoundCompleted) ||
		CanTransitionReviewRound(RoundCompleted, RoundRunning) {
		t.Fatal("round transition graph is invalid")
	}
	if !CanTransitionReviewAssignment(AssignmentQueued, AssignmentRunning) ||
		!CanTransitionReviewAssignment(AssignmentRunning, AssignmentSucceeded) ||
		CanTransitionReviewAssignment(AssignmentSucceeded, AssignmentRunning) {
		t.Fatal("assignment transition graph is invalid")
	}
}

func TestEvaluateReviewGateDoesNotUseMajorityVoting(t *testing.T) {
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	policy := testReviewPolicy(t)
	round := ReviewRound{
		ID:       "round-1",
		Subject:  ReviewSubject{Kind: SubjectSystemDesign, ID: "art-1", Version: 1, ContentHash: "subject-hash"},
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
		Status: RoundEvaluating,
	}
	assignments := []ReviewAssignment{
		{ID: "a-1", RoundID: round.ID, ReviewerID: "architecture", Required: true, Status: AssignmentSucceeded},
		{ID: "a-2", RoundID: round.ID, ReviewerID: "security", Required: true, Status: AssignmentSucceeded},
	}
	reports := []ReviewReport{
		{
			ID: "r-1", RoundID: round.ID, AssignmentID: "a-1", ReviewerID: "architecture",
			SubjectHash: round.Subject.ContentHash, ContentHash: "report-1",
			Coverage: []CoverageItem{{Category: "architecture", Covered: true}, {Category: "security", Covered: true}},
			Findings: []Finding{{
				ID: "finding-high", Category: "architecture", Severity: SeverityHigh,
				Evidence: []FindingEvidence{{Kind: "subject", Ref: "modules[0]", Hash: "evidence-hash", Summary: "cycle"}},
			}},
		},
		{
			ID: "r-2", RoundID: round.ID, AssignmentID: "a-2", ReviewerID: "security",
			SubjectHash: round.Subject.ContentHash, ContentHash: "report-2",
			Coverage: []CoverageItem{{Category: "security", Covered: true}},
		},
	}
	result, err := EvaluateReviewGate(ReviewEvaluation{
		Round: round, Policy: policy, Assignments: assignments, Reports: reports,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GateRevise || len(result.BlockingIDs) != 1 || result.BlockingIDs[0] != "finding-high" {
		t.Fatalf("gate result = %+v", result)
	}
}

func TestEvaluateReviewGateRequiresSuccessfulReviewersAndEvidence(t *testing.T) {
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	policy := testReviewPolicy(t)
	round := ReviewRound{
		ID:       "round-1",
		Subject:  ReviewSubject{Kind: SubjectSystemDesign, ID: "art-1", Version: 1, ContentHash: "subject-hash"},
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
		Status: RoundEvaluating,
	}
	assignments := []ReviewAssignment{
		{ID: "a-1", RoundID: round.ID, ReviewerID: "architecture", Required: true, Status: AssignmentSucceeded},
		{ID: "a-2", RoundID: round.ID, ReviewerID: "security", Required: true, Status: AssignmentFailed},
	}
	reports := []ReviewReport{{
		ID: "r-1", RoundID: round.ID, AssignmentID: "a-1", ReviewerID: "architecture",
		SubjectHash: round.Subject.ContentHash, ContentHash: "report-1",
		Coverage: []CoverageItem{{Category: "architecture", Covered: true}},
	}}
	result, err := EvaluateReviewGate(ReviewEvaluation{
		Round: round, Policy: policy, Assignments: assignments, Reports: reports,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GateIncomplete {
		t.Fatalf("required reviewer failure gate = %+v", result)
	}

	assignments[1].Status = AssignmentSucceeded
	reports = append(reports, ReviewReport{
		ID: "r-2", RoundID: round.ID, AssignmentID: "a-2", ReviewerID: "security",
		SubjectHash: round.Subject.ContentHash, ContentHash: "report-2",
		Coverage: []CoverageItem{{Category: "security", Covered: true}},
		Findings: []Finding{{ID: "unsupported-high", Category: "security", Severity: SeverityHigh}},
	})
	result, err = EvaluateReviewGate(ReviewEvaluation{
		Round: round, Policy: policy, Assignments: assignments, Reports: reports,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GateHumanRequired {
		t.Fatalf("unsupported high finding gate = %+v", result)
	}
}

func TestEvaluateReviewGateIsIndependentOfReportOrder(t *testing.T) {
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	policy := testReviewPolicy(t)
	round := ReviewRound{
		ID:       "round-1",
		Subject:  ReviewSubject{Kind: SubjectSystemDesign, ID: "art-1", Version: 1, ContentHash: "subject-hash"},
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
		Status: RoundEvaluating,
	}
	assignments := []ReviewAssignment{
		{ID: "a-1", RoundID: round.ID, ReviewerID: "architecture", Required: true, Status: AssignmentSucceeded},
		{ID: "a-2", RoundID: round.ID, ReviewerID: "security", Required: true, Status: AssignmentSucceeded},
	}
	reports := []ReviewReport{
		{ID: "r-1", RoundID: round.ID, AssignmentID: "a-1", ReviewerID: "architecture", SubjectHash: "subject-hash", ContentHash: "h1", Coverage: []CoverageItem{{Category: "architecture", Covered: true}}},
		{ID: "r-2", RoundID: round.ID, AssignmentID: "a-2", ReviewerID: "security", SubjectHash: "subject-hash", ContentHash: "h2", Coverage: []CoverageItem{{Category: "security", Covered: true}}},
	}
	first, err := EvaluateReviewGate(ReviewEvaluation{Round: round, Policy: policy, Assignments: assignments, Reports: reports}, now)
	if err != nil {
		t.Fatal(err)
	}
	reports[0], reports[1] = reports[1], reports[0]
	second, err := EvaluateReviewGate(ReviewEvaluation{Round: round, Policy: policy, Assignments: assignments, Reports: reports}, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash != second.ContentHash || first.Decision != GatePass {
		t.Fatalf("order changed gate: first=%+v second=%+v", first, second)
	}
}

func testReviewPolicy(t *testing.T) ReviewPolicy {
	t.Helper()
	policy, err := PrepareReviewPolicy(ReviewPolicy{
		ID: "system-design-review", Version: 1, SubjectKind: SubjectSystemDesign,
		Reviewers: []ReviewerSpec{
			{ID: "architecture", Agent: agentapi.DefinitionRef{ID: "review.architecture", Version: 1}, DefinitionHash: "agent-hash-1", Categories: []string{"architecture"}, Required: true, ReadOnly: true},
			{ID: "security", Agent: agentapi.DefinitionRef{ID: "review.security", Version: 1}, DefinitionHash: "agent-hash-2", Categories: []string{"security"}, Required: true, ReadOnly: true},
		},
		BlockingSeverities: []Severity{SeverityCritical, SeverityHigh},
		RequiredCategories: []string{"architecture", "security"},
		MaxParallelism:     2,
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}
