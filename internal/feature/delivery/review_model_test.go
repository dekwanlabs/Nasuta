package delivery

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/platform"
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

func TestReviewEventsMatchPersistedRoundStatus(t *testing.T) {
	for _, test := range []struct {
		kind   ReviewEventKind
		status ReviewRoundStatus
	}{
		{ReviewEventRoundStarted, RoundRunning},
		{ReviewEventAssignmentStarted, RoundRunning},
		{ReviewEventAssignmentSucceeded, RoundRunning},
		{ReviewEventAssignmentFailed, RoundRunning},
		{ReviewEventRoundEvaluating, RoundEvaluating},
		{ReviewEventAdjudicationStarted, RoundEvaluating},
		{ReviewEventAdjudicationFinished, RoundEvaluating},
		{ReviewEventRoundCompleted, RoundCompleted},
		{ReviewEventRoundFailed, RoundFailed},
		{ReviewEventRoundCancelled, RoundCancelled},
	} {
		if !CanAppendReviewEvent(test.kind, test.status) {
			t.Fatalf("event %s rejected for status %s", test.kind, test.status)
		}
		if CanAppendReviewEvent(test.kind, RoundCreated) {
			t.Fatalf("event %s accepted for created Round", test.kind)
		}
	}
	if CanAppendReviewEvent("unknown", RoundRunning) {
		t.Fatal("unknown event kind accepted")
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

func TestEvaluateReviewGateDetectsMissingOptionalReviewerAssignment(t *testing.T) {
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	policy := testReviewPolicy(t)
	policy.Reviewers = append(policy.Reviewers, ReviewerSpec{
		ID: "reliability", Agent: agentapi.DefinitionRef{
			ID: "review.reliability", Version: 1,
		},
		DefinitionHash: "agent-hash-3",
		Categories:     []string{"reliability"},
		ReadOnly:       true,
	})
	policy.MaxInputTokens = 4
	policy.MaxOutputTokens = 4
	policy.MaxTotalTokens = 4
	policy.MaxToolCalls = 4
	policy.MaxCostMicros = 4
	policy.ContentHash = ""
	policy, err := PrepareReviewPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	round := ReviewRound{
		ID: "round-1",
		Subject: ReviewSubject{
			Kind: SubjectSystemDesign, ID: "art-1", Version: 1,
			ContentHash: "subject-hash",
		},
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
		Status: RoundEvaluating,
	}
	assignments := []ReviewAssignment{
		{
			ID: "a-1", RoundID: round.ID, ReviewerID: "architecture",
			Required: true, Status: AssignmentSucceeded,
		},
		{
			ID: "a-2", RoundID: round.ID, ReviewerID: "security",
			Required: true, Status: AssignmentSucceeded,
		},
	}
	reports := []ReviewReport{
		{
			ID: "r-1", RoundID: round.ID, AssignmentID: "a-1", ReviewerID: "architecture",
			SubjectHash: round.Subject.ContentHash, ContentHash: "report-1",
			Coverage: []CoverageItem{{Category: "architecture", Covered: true}},
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
	if result.Decision != GateIncomplete ||
		!containsString(result.CoverageGaps, "reviewer:reliability") ||
		!containsString(result.ReasonCodes, reasonOptionalReviewerIncomplete) {
		t.Fatalf("missing optional reviewer gate = %+v", result)
	}

	policy.OptionalReviewerAction = OptionalReviewerHumanRequired
	policy.ContentHash = ""
	policy, err = PrepareReviewPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	round.PolicyHash = policy.ContentHash
	result, err = EvaluateReviewGate(ReviewEvaluation{
		Round: round, Policy: policy, Assignments: assignments, Reports: reports,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GateHumanRequired ||
		!containsString(result.CoverageGaps, "reviewer:reliability") ||
		!containsString(result.ReasonCodes, reasonOptionalReviewerIncomplete) {
		t.Fatalf("missing optional reviewer human gate = %+v", result)
	}
}

func TestEvaluateReviewGateDetectsMissingRequiredReviewerAssignment(t *testing.T) {
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	policy := testReviewPolicy(t)
	round := ReviewRound{
		ID: "round-1",
		Subject: ReviewSubject{
			Kind: SubjectSystemDesign, ID: "art-1", Version: 1,
			ContentHash: "subject-hash",
		},
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
		Status: RoundEvaluating,
	}
	assignments := []ReviewAssignment{{
		ID: "a-1", RoundID: round.ID, ReviewerID: "architecture",
		Required: true, Status: AssignmentSucceeded,
	}}
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
	if result.Decision != GateIncomplete ||
		!containsString(result.ReasonCodes, reasonRequiredAssignmentIncomplete) {
		t.Fatalf("missing required reviewer gate = %+v", result)
	}
}

func TestEvaluateReviewGateRequiresHumanForBlockingThresholdConflict(t *testing.T) {
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
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
	evidence := []FindingEvidence{{
		Kind: "subject", Ref: "interfaces[0]", Hash: "evidence-hash", Summary: "contract mismatch",
	}}
	reports := []ReviewReport{
		{
			ID: "r-1", RoundID: round.ID, AssignmentID: "a-1", ReviewerID: "architecture",
			SubjectHash: round.Subject.ContentHash, ContentHash: "report-1",
			Coverage: []CoverageItem{{Category: "architecture", Covered: true}},
			Findings: []Finding{{
				ID: "finding-high", Fingerprint: "same-fingerprint",
				Category: "architecture", Severity: SeverityHigh, Evidence: evidence,
			}},
		},
		{
			ID: "r-2", RoundID: round.ID, AssignmentID: "a-2", ReviewerID: "security",
			SubjectHash: round.Subject.ContentHash, ContentHash: "report-2",
			Coverage: []CoverageItem{{Category: "security", Covered: true}},
			Findings: []Finding{{
				ID: "finding-medium", Fingerprint: "same-fingerprint",
				Category: "architecture", Severity: SeverityMedium, Evidence: evidence,
			}},
		},
	}
	result, err := EvaluateReviewGate(ReviewEvaluation{
		Round: round, Policy: policy, Assignments: assignments, Reports: reports,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GateHumanRequired ||
		!slices.Equal(result.BlockingIDs, []string{"finding-high"}) ||
		!slices.Equal(result.ConflictIDs, []string{"finding-high", "finding-medium"}) ||
		!slices.Contains(result.ReasonCodes, reasonFindingSeverityConflict) {
		t.Fatalf("gate result = %+v", result)
	}

	reports[0], reports[1] = reports[1], reports[0]
	reordered, err := EvaluateReviewGate(ReviewEvaluation{
		Round: round, Policy: policy, Assignments: assignments, Reports: reports,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if reordered.ContentHash != result.ContentHash {
		t.Fatalf("report order changed conflict result: first=%+v reordered=%+v", result, reordered)
	}
}

func TestEvaluateReviewGateUsesOnlyConfirmedAdjudicationToResolveConflict(t *testing.T) {
	now := time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		decision AdjudicationDecision
		want     GateDecision
	}{
		{AdjudicationConfirmed, GateRevise},
		{AdjudicationNotSupported, GateHumanRequired},
		{AdjudicationDistinctFindings, GateHumanRequired},
		{AdjudicationNeedsHuman, GateHumanRequired},
	} {
		t.Run(string(test.decision), func(t *testing.T) {
			evaluation := reviewConflictEvaluation(t)
			adjudication, err := PrepareReviewAdjudication(ReviewAdjudication{
				RoundID:        evaluation.Round.ID,
				SubjectHash:    evaluation.Round.Subject.ContentHash,
				PolicyHash:     evaluation.Policy.ContentHash,
				Fingerprint:    "same-fingerprint",
				FindingIDs:     []string{"finding-medium", "finding-high"},
				Agent:          evaluation.Policy.Adjudicator.Agent,
				DefinitionHash: evaluation.Policy.Adjudicator.DefinitionHash,
				Decision:       test.decision,
				Rationale:      "Evidence requires the conservative disposition.",
				CreatedAt:      now,
			})
			if err != nil {
				t.Fatal(err)
			}
			evaluation.Adjudications = []ReviewAdjudication{adjudication}

			result, err := EvaluateReviewGate(evaluation, now)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decision != test.want || len(result.AdjudicationHashes) != 1 {
				t.Fatalf("gate result = %+v", result)
			}
			if test.want == GateRevise && len(result.ConflictIDs) != 0 {
				t.Fatalf("confirmed adjudication retained conflicts: %+v", result)
			}
			if test.want == GateHumanRequired &&
				!slices.Equal(result.ConflictIDs, []string{"finding-high", "finding-medium"}) {
				t.Fatalf("unresolved adjudication changed conflicts: %+v", result)
			}
		})
	}
}

func TestEvaluateReviewGateRejectsMismatchedAdjudicationSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 6, 11, 30, 0, 0, time.UTC)
	base := reviewConflictEvaluation(t)
	for _, test := range []struct {
		name   string
		mutate func(*ReviewAdjudication)
	}{
		{
			name: "round",
			mutate: func(value *ReviewAdjudication) {
				value.RoundID = "other-round"
			},
		},
		{
			name: "subject",
			mutate: func(value *ReviewAdjudication) {
				value.SubjectHash = "other-subject"
			},
		},
		{
			name: "policy",
			mutate: func(value *ReviewAdjudication) {
				value.PolicyHash = "other-policy"
			},
		},
		{
			name: "fingerprint",
			mutate: func(value *ReviewAdjudication) {
				value.Fingerprint = "other-fingerprint"
			},
		},
		{
			name: "finding ids",
			mutate: func(value *ReviewAdjudication) {
				value.FindingIDs = []string{"finding-high", "other-finding"}
			},
		},
		{
			name: "agent",
			mutate: func(value *ReviewAdjudication) {
				value.Agent.ID = "review.other"
			},
		},
		{
			name: "definition",
			mutate: func(value *ReviewAdjudication) {
				value.DefinitionHash = "other-definition"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			adjudication := ReviewAdjudication{
				RoundID:        base.Round.ID,
				SubjectHash:    base.Round.Subject.ContentHash,
				PolicyHash:     base.Policy.ContentHash,
				Fingerprint:    "same-fingerprint",
				FindingIDs:     []string{"finding-high", "finding-medium"},
				Agent:          base.Policy.Adjudicator.Agent,
				DefinitionHash: base.Policy.Adjudicator.DefinitionHash,
				Decision:       AdjudicationConfirmed,
				Rationale:      "The blocking evidence is confirmed.",
				CreatedAt:      now,
			}
			test.mutate(&adjudication)
			prepared, err := PrepareReviewAdjudication(adjudication)
			if err != nil {
				t.Fatal(err)
			}
			evaluation := base
			evaluation.Adjudications = []ReviewAdjudication{prepared}
			if _, err := EvaluateReviewGate(evaluation, now); !errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v, want conflict", err)
			}
		})
	}
}

func TestEvaluateReviewGateDoesNotConflictWhenMatchingFindingsAreBothBlocking(t *testing.T) {
	now := time.Date(2026, 8, 6, 10, 30, 0, 0, time.UTC)
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
	evidence := []FindingEvidence{{
		Kind: "subject", Ref: "interfaces[0]", Hash: "evidence-hash", Summary: "contract mismatch",
	}}
	reports := []ReviewReport{
		{
			ID: "r-1", RoundID: round.ID, AssignmentID: "a-1", ReviewerID: "architecture",
			SubjectHash: round.Subject.ContentHash, ContentHash: "report-1",
			Coverage: []CoverageItem{{Category: "architecture", Covered: true}},
			Findings: []Finding{{
				ID: "finding-critical", Fingerprint: "same-fingerprint",
				Category: "architecture", Severity: SeverityCritical, Evidence: evidence,
			}},
		},
		{
			ID: "r-2", RoundID: round.ID, AssignmentID: "a-2", ReviewerID: "security",
			SubjectHash: round.Subject.ContentHash, ContentHash: "report-2",
			Coverage: []CoverageItem{{Category: "security", Covered: true}},
			Findings: []Finding{{
				ID: "finding-high", Fingerprint: "same-fingerprint",
				Category: "architecture", Severity: SeverityHigh, Evidence: evidence,
			}},
		},
	}
	result, err := EvaluateReviewGate(ReviewEvaluation{
		Round: round, Policy: policy, Assignments: assignments, Reports: reports,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GateRevise ||
		!slices.Equal(result.BlockingIDs, []string{"finding-critical", "finding-high"}) ||
		len(result.ConflictIDs) != 0 ||
		slices.Contains(result.ReasonCodes, reasonFindingSeverityConflict) {
		t.Fatalf("gate result = %+v", result)
	}
}

func TestPrepareReviewPolicyRequiresReadOnlyAdjudicator(t *testing.T) {
	policy := testReviewPolicy(t)
	policy.ContentHash = ""
	policy.Adjudicator.ReadOnly = false

	if _, err := PrepareReviewPolicy(policy); !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want invalid", err)
	}
}

func TestPrepareReviewPolicyPinsAdjudicatorDefinitionInContentHash(t *testing.T) {
	policy := testReviewPolicy(t)
	changed := policy
	adjudicator := *policy.Adjudicator
	adjudicator.DefinitionHash = "changed-adjudicator-hash"
	changed.Adjudicator = &adjudicator
	changed.ContentHash = ""

	prepared, err := PrepareReviewPolicy(changed)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ContentHash == policy.ContentHash {
		t.Fatal("adjudicator definition hash did not change policy content hash")
	}
	changed.ContentHash = policy.ContentHash
	if _, err := PrepareReviewPolicy(changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func reviewConflictEvaluation(t *testing.T) ReviewEvaluation {
	t.Helper()
	policy := testReviewPolicy(t)
	round := ReviewRound{
		ID: "round-1",
		Subject: ReviewSubject{
			Kind: SubjectSystemDesign, ID: "art-1", Version: 1,
			ContentHash: "subject-hash",
		},
		PolicyID: policy.ID, PolicyVersion: policy.Version,
		PolicyHash: policy.ContentHash, Status: RoundEvaluating,
	}
	assignments := []ReviewAssignment{
		{
			ID: "a-1", RoundID: round.ID, ReviewerID: "architecture",
			Required: true, Status: AssignmentSucceeded,
		},
		{
			ID: "a-2", RoundID: round.ID, ReviewerID: "security",
			Required: true, Status: AssignmentSucceeded,
		},
	}
	evidence := []FindingEvidence{{
		Kind: "subject", Ref: "interfaces[0]",
		Hash: "evidence-hash", Summary: "contract mismatch",
	}}
	return ReviewEvaluation{
		Round: round, Policy: policy, Assignments: assignments,
		Reports: []ReviewReport{
			{
				ID: "r-1", RoundID: round.ID, AssignmentID: "a-1",
				ReviewerID: "architecture", SubjectHash: round.Subject.ContentHash,
				ContentHash: "report-1",
				Coverage:    []CoverageItem{{Category: "architecture", Covered: true}},
				Findings: []Finding{{
					ID: "finding-high", Fingerprint: "same-fingerprint",
					Category: "architecture", Severity: SeverityHigh, Evidence: evidence,
				}},
			},
			{
				ID: "r-2", RoundID: round.ID, AssignmentID: "a-2",
				ReviewerID: "security", SubjectHash: round.Subject.ContentHash,
				ContentHash: "report-2",
				Coverage:    []CoverageItem{{Category: "security", Covered: true}},
				Findings: []Finding{{
					ID: "finding-medium", Fingerprint: "same-fingerprint",
					Category: "architecture", Severity: SeverityMedium, Evidence: evidence,
				}},
			},
		},
	}
}

func TestEvaluateReviewGateResolutionsRemoveFindingSeverityConflict(t *testing.T) {
	now := time.Date(2026, 8, 6, 10, 45, 0, 0, time.UTC)
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
	evidence := []FindingEvidence{{
		Kind: "subject", Ref: "interfaces[0]", Hash: "evidence-hash", Summary: "contract mismatch",
	}}
	reports := []ReviewReport{
		{
			ID: "r-1", RoundID: round.ID, AssignmentID: "a-1", ReviewerID: "architecture",
			SubjectHash: round.Subject.ContentHash, ContentHash: "report-1",
			Coverage: []CoverageItem{{Category: "architecture", Covered: true}},
			Findings: []Finding{{
				ID: "finding-high", Fingerprint: "same-fingerprint",
				Category: "architecture", Severity: SeverityHigh, Evidence: evidence,
			}},
		},
		{
			ID: "r-2", RoundID: round.ID, AssignmentID: "a-2", ReviewerID: "security",
			SubjectHash: round.Subject.ContentHash, ContentHash: "report-2",
			Coverage: []CoverageItem{{Category: "security", Covered: true}},
			Findings: []Finding{{
				ID: "finding-medium", Fingerprint: "same-fingerprint",
				Category: "architecture", Severity: SeverityMedium, Evidence: evidence,
			}},
		},
	}
	for _, resolution := range []FindingResolutionKind{
		ResolutionFixed,
		ResolutionWaived,
		ResolutionInvalidated,
		ResolutionSuperseded,
	} {
		t.Run(string(resolution), func(t *testing.T) {
			result, err := EvaluateReviewGate(ReviewEvaluation{
				Round: round, Policy: policy, Assignments: assignments, Reports: reports,
				Resolutions: []FindingResolution{{
					FindingID: "finding-high", Resolution: resolution,
					SubjectHash: round.Subject.ContentHash,
				}},
			}, now)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decision != GatePass || len(result.BlockingIDs) != 0 ||
				len(result.ConflictIDs) != 0 ||
				slices.Contains(result.ReasonCodes, reasonFindingSeverityConflict) {
				t.Fatalf("gate result = %+v", result)
			}
		})
	}
}

func TestEvaluateReviewGateUsesLatestResolutionFact(t *testing.T) {
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Minute)
	for _, test := range []struct {
		name        string
		resolutions []FindingResolution
		want        GateDecision
	}{
		{
			name: "latest expired waiver reopens finding",
			resolutions: []FindingResolution{
				{
					ID: "resolution-1", FindingID: "finding-high",
					Resolution: ResolutionInvalidated, SubjectHash: "subject-hash",
					CreatedAt: now.Add(-2 * time.Hour),
				},
				{
					ID: "resolution-2", FindingID: "finding-high",
					Resolution: ResolutionWaived, SubjectHash: "subject-hash",
					ExpiresAt: &expired, CreatedAt: now.Add(-time.Hour),
				},
			},
			want: GateHumanRequired,
		},
		{
			name: "latest invalidation closes finding",
			resolutions: []FindingResolution{
				{
					ID: "resolution-1", FindingID: "finding-high",
					Resolution: ResolutionWaived, SubjectHash: "subject-hash",
					ExpiresAt: &expired, CreatedAt: now.Add(-2 * time.Hour),
				},
				{
					ID: "resolution-2", FindingID: "finding-high",
					Resolution: ResolutionInvalidated, SubjectHash: "subject-hash",
					CreatedAt: now.Add(-time.Hour),
				},
			},
			want: GatePass,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			evaluation := reviewConflictEvaluation(t)
			evaluation.Resolutions = test.resolutions
			result, err := EvaluateReviewGate(evaluation, now)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decision != test.want {
				t.Fatalf("decision = %q, want %q; result = %+v", result.Decision, test.want, result)
			}
		})
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

func TestPrepareReviewReportCanonicalizesAndDeduplicatesFindingFingerprints(t *testing.T) {
	assignment := ReviewAssignment{
		ID: "assignment-1", RoundID: "round-1", ReviewerID: "security",
		Status: AssignmentRunning,
	}
	now := time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC)
	finding := Finding{
		Category: "security", Severity: SeverityHigh,
		Claim: "Authorization is not enforced.", Impact: "Users can cross tenant boundaries.",
		Recommendation: "Enforce authorization before the query.",
		Confidence:     0.9,
		Evidence: []FindingEvidence{{
			Kind: "code", Ref: "handler.go:42", Hash: "source-hash", Summary: "query precedes authorization",
		}},
		Fingerprint: "agent-controlled-value",
	}
	report := ReviewReport{
		RoundID: "round-1", AssignmentID: assignment.ID, ReviewerID: assignment.ReviewerID,
		SubjectHash: "subject-hash", CompletedAt: now,
		Findings: []Finding{finding},
	}
	prepared, err := PrepareReviewReport(report, assignment, "subject-hash")
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Findings[0].Fingerprint == finding.Fingerprint ||
		len(prepared.Findings[0].Fingerprint) != sha256.Size*2 {
		t.Fatalf("fingerprint = %q", prepared.Findings[0].Fingerprint)
	}

	duplicate := finding
	duplicate.Claim = "  authorization\nIS   not enforced.  "
	report.Findings = append(report.Findings, duplicate)
	if _, err := PrepareReviewReport(
		report,
		assignment,
		"subject-hash",
	); err == nil || !strings.Contains(err.Error(), "duplicate finding fingerprint") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestPrepareReusedReviewReportPreservesSemanticHashWithNewIdentity(t *testing.T) {
	sourceAssignment := ReviewAssignment{
		ID: "assignment-source", RoundID: "round-source", ReviewerID: "security",
		Status: AssignmentRunning,
	}
	source, err := PrepareReviewReport(ReviewReport{
		RoundID: sourceAssignment.RoundID, AssignmentID: sourceAssignment.ID,
		ReviewerID: sourceAssignment.ReviewerID, SubjectHash: "subject-hash",
		Coverage: []CoverageItem{{Category: "security", Covered: true}},
		Findings: []Finding{{
			Category: "security", Severity: SeverityHigh,
			Claim: "Authorization is missing.", Impact: "Tenant data can leak.",
			Recommendation: "Enforce authorization.", Confidence: 0.9,
			Evidence: []FindingEvidence{{
				Kind: "code", Ref: "handler.go:42", Hash: "evidence-hash",
				Summary: "The query runs before authorization.",
			}},
		}},
		Summary:     "One blocking finding.",
		CompletedAt: time.Date(2026, 8, 6, 11, 0, 0, 0, time.UTC),
	}, sourceAssignment, "subject-hash")
	if err != nil {
		t.Fatal(err)
	}
	targetAssignment := ReviewAssignment{
		ID: "assignment-target", RoundID: "round-target", ReviewerID: "security",
		Status: AssignmentQueued,
	}
	target, err := PrepareReusedReviewReport(
		source,
		targetAssignment,
		"subject-hash",
		ReviewReportReuseRef{
			SourceReportID: source.ID, SourceRoundID: source.RoundID,
			SourceAssignmentID: source.AssignmentID,
			Reason:             "The immutable inputs are unchanged.",
		},
		time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	if target.ReportHash != source.ReportHash ||
		target.ContentHash == source.ContentHash ||
		target.ID == source.ID ||
		target.Findings[0].ID == source.Findings[0].ID ||
		target.RoundID != targetAssignment.RoundID ||
		target.AssignmentID != targetAssignment.ID ||
		target.Reuse == nil ||
		target.Reuse.SourceReportID != source.ID {
		t.Fatalf("source = %+v, target = %+v", source, target)
	}
}

func TestEvaluateReviewGateAcceptsReusedAssignment(t *testing.T) {
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	policy := testReviewPolicy(t)
	round := ReviewRound{
		ID: "round-1",
		Subject: ReviewSubject{
			Kind: SubjectSystemDesign, ID: "art-1", Version: 1,
			ContentHash: "subject-hash",
		},
		PolicyID: policy.ID, PolicyVersion: policy.Version,
		PolicyHash: policy.ContentHash, Status: RoundEvaluating,
	}
	assignments := []ReviewAssignment{
		{
			ID: "a-1", RoundID: round.ID, ReviewerID: "architecture",
			Required: true, Status: AssignmentReused,
		},
		{
			ID: "a-2", RoundID: round.ID, ReviewerID: "security",
			Required: true, Status: AssignmentSucceeded,
		},
	}
	reports := []ReviewReport{
		{
			ID: "r-1", RoundID: round.ID, AssignmentID: "a-1",
			ReviewerID: "architecture", SubjectHash: round.Subject.ContentHash,
			ContentHash: "report-1",
			Coverage:    []CoverageItem{{Category: "architecture", Covered: true}},
		},
		{
			ID: "r-2", RoundID: round.ID, AssignmentID: "a-2",
			ReviewerID: "security", SubjectHash: round.Subject.ContentHash,
			ContentHash: "report-2",
			Coverage:    []CoverageItem{{Category: "security", Covered: true}},
		},
	}
	result, err := EvaluateReviewGate(ReviewEvaluation{
		Round: round, Policy: policy, Assignments: assignments, Reports: reports,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GatePass {
		t.Fatalf("gate = %+v", result)
	}
}

func TestPrepareReviewReportRedactsBeforeFingerprintAndContentHash(t *testing.T) {
	assignment := ReviewAssignment{
		ID: "assignment-1", RoundID: "round-1", ReviewerID: "security",
		Status: AssignmentRunning,
	}
	report := ReviewReport{
		RoundID: "round-1", AssignmentID: assignment.ID, ReviewerID: assignment.ReviewerID,
		SubjectHash: "subject-hash",
		Coverage: []CoverageItem{{
			Category: "security", Covered: true,
			Summary: "Authorization: Bearer coverage-secret",
		}},
		Uncertainties: []Uncertainty{{
			Category: "security", Summary: "DATABASE_PASSWORD=uncertainty-secret",
		}},
		Summary:     "access_token=summary-secret",
		CompletedAt: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
		Findings: []Finding{{
			Category: "security", Severity: SeverityHigh,
			Claim:          "postgres://app:claim-secret@db/service",
			Impact:         "client_secret=impact-secret",
			Recommendation: "Authorization: Bearer recommendation-secret",
			Confidence:     0.9,
			Location: &FindingLocation{
				Path:  "https://app:path-secret@example.com/config",
				Field: "api_key=field-secret",
			},
			Evidence: []FindingEvidence{{
				Kind: "code", Ref: "mysql://app:reference-secret@db/service",
				Hash: "immutable-evidence-hash", Summary: "token=evidence-secret",
			}},
		}},
	}

	prepared, err := PrepareReviewReport(report, assignment, report.SubjectHash)
	if err != nil {
		t.Fatal(err)
	}
	assertReviewSecretsAbsent(t, prepared, []string{
		"coverage-secret", "uncertainty-secret", "summary-secret", "claim-secret",
		"impact-secret", "recommendation-secret", "path-secret", "field-secret",
		"reference-secret", "evidence-secret",
	})
	if got := prepared.Findings[0].Evidence[0].Hash; got != "immutable-evidence-hash" {
		t.Fatalf("evidence hash = %q", got)
	}

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var alreadyRedacted ReviewReport
	if err := json.Unmarshal(
		[]byte(platform.RedactSensitiveText(string(raw))),
		&alreadyRedacted,
	); err != nil {
		t.Fatal(err)
	}
	reprepared, err := PrepareReviewReport(alreadyRedacted, assignment, report.SubjectHash)
	if err != nil {
		t.Fatal(err)
	}
	if reprepared.ContentHash != prepared.ContentHash ||
		reprepared.Findings[0].Fingerprint != prepared.Findings[0].Fingerprint ||
		reprepared.Findings[0].ContentHash != prepared.Findings[0].ContentHash {
		t.Fatalf("hashes differ after pre-redaction: first=%+v second=%+v", prepared, reprepared)
	}
}

func TestPrepareReviewAdjudicationRedactsBeforeContentHash(t *testing.T) {
	adjudication := ReviewAdjudication{
		RoundID: "round-1", SubjectHash: "subject-hash", PolicyHash: "policy-hash",
		Fingerprint: "finding-fingerprint", FindingIDs: []string{"finding-2", "finding-1"},
		Agent:          agentapi.DefinitionRef{ID: "review.adjudicator", Version: 1},
		DefinitionHash: strings.Repeat("d", 64),
		Decision:       AdjudicationNeedsHuman,
		Rationale:      "Authorization: Bearer rationale-secret",
		ErrorCode:      "client_secret=error-secret",
		CreatedAt:      time.Date(2026, 8, 6, 12, 5, 0, 0, time.UTC),
	}

	prepared, err := PrepareReviewAdjudication(adjudication)
	if err != nil {
		t.Fatal(err)
	}
	assertReviewSecretsAbsent(t, prepared, []string{"rationale-secret", "error-secret"})

	raw, err := json.Marshal(adjudication)
	if err != nil {
		t.Fatal(err)
	}
	var alreadyRedacted ReviewAdjudication
	if err := json.Unmarshal(
		[]byte(platform.RedactSensitiveText(string(raw))),
		&alreadyRedacted,
	); err != nil {
		t.Fatal(err)
	}
	reprepared, err := PrepareReviewAdjudication(alreadyRedacted)
	if err != nil {
		t.Fatal(err)
	}
	if reprepared.ID != prepared.ID || reprepared.ContentHash != prepared.ContentHash {
		t.Fatalf("hash differs after pre-redaction: first=%+v second=%+v", prepared, reprepared)
	}
}

func assertReviewSecretsAbsent(t *testing.T, value any, secrets []string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal review value: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("secret %q leaked: %s", secret, encoded)
		}
	}
	if !strings.Contains(string(encoded), platform.RedactedValue) {
		t.Fatalf("redaction marker missing: %s", encoded)
	}
}

func TestImplementationReviewSubjectHashesRespectBundleBoundaries(t *testing.T) {
	run, design, plan := implementationReviewFixture()
	change, err := BuildChangeSetReviewSubject(run)
	if err != nil {
		t.Fatal(err)
	}
	validation, err := BuildValidationBundleReviewSubject(run)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := BuildDeliveryBundleReviewSubject(run, design, plan)
	if err != nil {
		t.Fatal(err)
	}

	run.ChangeSet.ValidationResults[0].OutputSummary = "different validation output"
	changedValidationChange, err := BuildChangeSetReviewSubject(run)
	if err != nil {
		t.Fatal(err)
	}
	changedValidation, err := BuildValidationBundleReviewSubject(run)
	if err != nil {
		t.Fatal(err)
	}
	changedValidationDelivery, err := BuildDeliveryBundleReviewSubject(run, design, plan)
	if err != nil {
		t.Fatal(err)
	}
	if changedValidationChange.ContentHash != change.ContentHash {
		t.Fatal("validation changes invalidated the independent change set subject")
	}
	if changedValidation.ContentHash == validation.ContentHash {
		t.Fatal("validation changes did not invalidate the validation bundle")
	}
	if changedValidationDelivery.ContentHash == delivery.ContentHash {
		t.Fatal("validation changes did not invalidate the delivery bundle")
	}

	for _, test := range []struct {
		name   string
		mutate func(*ImplementationRun, *Artifact, *Artifact)
	}{
		{
			name: "design",
			mutate: func(_ *ImplementationRun, design, _ *Artifact) {
				design.ContentHash = "changed-design-hash"
			},
		},
		{
			name: "plan",
			mutate: func(_ *ImplementationRun, _, plan *Artifact) {
				plan.ContentHash = "changed-plan-hash"
			},
		},
		{
			name: "change_set",
			mutate: func(run *ImplementationRun, _, _ *Artifact) {
				run.ChangeSet.PatchSHA256 = strings.Repeat("b", sha256.Size*2)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidateRun, candidateDesign, candidatePlan := implementationReviewFixture()
			test.mutate(&candidateRun, &candidateDesign, &candidatePlan)
			candidate, err := BuildDeliveryBundleReviewSubject(
				candidateRun,
				candidateDesign,
				candidatePlan,
			)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.ContentHash == delivery.ContentHash {
				t.Fatalf("%s changes did not invalidate the delivery bundle", test.name)
			}
		})
	}
}

func testReviewPolicy(t *testing.T) ReviewPolicy {
	return testReviewPolicyForSubject(t, SubjectSystemDesign)
}

func testReviewPolicyForSubject(t *testing.T, kind SubjectKind) ReviewPolicy {
	t.Helper()
	policy, err := PrepareReviewPolicy(ReviewPolicy{
		ID: "review-" + string(kind), Version: 1, SubjectKind: kind,
		Reviewers: []ReviewerSpec{
			{ID: "architecture", Agent: agentapi.DefinitionRef{ID: "review.architecture", Version: 1}, DefinitionHash: "agent-hash-1", Categories: []string{"architecture"}, Required: true, ReadOnly: true},
			{ID: "security", Agent: agentapi.DefinitionRef{ID: "review.security", Version: 1}, DefinitionHash: "agent-hash-2", Categories: []string{"security"}, Required: true, ReadOnly: true},
		},
		Adjudicator: &AdjudicatorSpec{
			Agent:          agentapi.DefinitionRef{ID: "review.adjudicator", Version: 1},
			DefinitionHash: "adjudicator-hash",
			ReadOnly:       true,
		},
		BlockingSeverities:     []Severity{SeverityCritical, SeverityHigh},
		RequiredCategories:     []string{"architecture", "security"},
		MaxParallelism:         2,
		MaxInputTokens:         3,
		MaxOutputTokens:        3,
		MaxTotalTokens:         3,
		MaxToolCalls:           3,
		MaxCostMicros:          3,
		MaxRetries:             1,
		Timeout:                time.Minute,
		OptionalReviewerAction: OptionalReviewerContinue,
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func implementationReviewFixture() (ImplementationRun, Artifact, Artifact) {
	design := Artifact{
		ID: "design-1", RequestID: "feat-1", Kind: KindSystemDesign,
		Version: 2, ContentHash: "design-hash",
		DocumentJSON:     json.RawMessage(`{"components":["api"]}`),
		RenderedMarkdown: "# System Design",
	}
	plan := Artifact{
		ID: "plan-1", RequestID: "feat-1", Kind: KindImplementationPlan,
		Version: 3, ParentArtifactID: design.ID, ContentHash: "plan-hash",
		DocumentJSON:     json.RawMessage(`{"steps":["implement"]}`),
		RenderedMarkdown: "# Implementation Plan",
	}
	run := ImplementationRun{
		ID: "run-1", RequestID: "feat-1",
		DesignArtifactID: design.ID, PlanArtifactID: plan.ID,
		Repo: "nasuta", BaseCommit: strings.Repeat("1", 40),
		Status: RunSucceeded, RequestedBy: 7,
		ChangeSet: &ChangeSet{
			RunID: "run-1", WorktreeHead: strings.Repeat("2", 40),
			PatchRelPath: "run-1/change.patch",
			PatchSHA256:  strings.Repeat("a", sha256.Size*2),
			PatchBytes:   128, FilesChanged: 1, Additions: 4, Deletions: 1,
			Files: []ChangedFile{{
				Path: "internal/feature/delivery/review.go", Status: "M",
				Additions: 4, Deletions: 1,
			}},
			PlanDeviations: []PlanDeviation{{
				Path:   "internal/feature/delivery/review.go",
				Reason: "No deviation", Explained: true,
			}},
			ValidationResults: []ValidationResult{{
				Sequence: 1, Argv: []string{"go", "test", "./..."},
				Status: "passed", OutputSummary: "ok",
				OutputRelPath: "run-1/validation-01.log",
				OutputSHA256:  strings.Repeat("c", sha256.Size*2), OutputBytes: 2,
			}},
			ProviderSummary: "implemented review bundles",
		},
	}
	return run, design, plan
}
