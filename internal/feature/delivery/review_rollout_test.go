package delivery

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReviewPolicyRolloutSelectionIsStableAndBounded(t *testing.T) {
	rule, err := prepareReviewPolicyRolloutRule(ReviewPolicyRolloutRule{
		SubjectKind: SubjectSystemDesign, RuleVersion: 3,
		CandidatePolicyID: "candidate-policy", CandidatePolicyVersion: 2,
		PercentageBPS: 2500, Salt: "rollout-2026-08", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	stableKey := StableReviewPolicySelectionKey(42, SubjectSystemDesign, "artifact-1")
	first, firstCandidate, err := selectReviewPolicyRollout(rule, stableKey)
	if err != nil {
		t.Fatal(err)
	}
	second, secondCandidate, err := selectReviewPolicyRollout(rule, stableKey)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || firstCandidate != secondCandidate {
		t.Fatalf("unstable selection: first=%+v/%t second=%+v/%t", first, firstCandidate, second, secondCandidate)
	}
	if first.RuleVersion != rule.RuleVersion || first.RuleHash != rule.RuleHash ||
		first.CandidatePolicyID != rule.CandidatePolicyID ||
		first.CandidatePolicyVersion != rule.CandidatePolicyVersion ||
		first.PercentageBasisPoints != rule.PercentageBPS ||
		first.BucketBasisPoints < 0 ||
		first.BucketBasisPoints >= reviewPolicyRolloutBucketCount ||
		first.StableKeyHash == "" || strings.Contains(first.StableKeyHash, "42") ||
		strings.Contains(first.StableKeyHash, "artifact-1") {
		t.Fatalf("selection = %+v", first)
	}
}

func TestReviewPolicyRolloutHonorsPercentageBoundaries(t *testing.T) {
	for _, test := range []struct {
		name       string
		percentage int
		candidate  bool
		reason     string
	}{
		{name: "zero", percentage: 0, reason: "rollout_default"},
		{name: "all", percentage: reviewPolicyRolloutBucketCount, candidate: true, reason: "rollout_candidate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rule, err := prepareReviewPolicyRolloutRule(ReviewPolicyRolloutRule{
				SubjectKind: SubjectSystemDesign, RuleVersion: 1,
				CandidatePolicyID: "candidate-policy", CandidatePolicyVersion: 2,
				PercentageBPS: test.percentage, Salt: test.name, Active: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			selection, candidate, err := selectReviewPolicyRollout(rule, "stable-key")
			if err != nil {
				t.Fatal(err)
			}
			if candidate != test.candidate || selection.Reason != test.reason {
				t.Fatalf("selection = %+v, candidate = %t", selection, candidate)
			}
		})
	}
}

func TestReviewPolicyRolloutRuleHashIsDeterministicAndValidated(t *testing.T) {
	base := ReviewPolicyRolloutRule{
		SubjectKind: SubjectSystemDesign, RuleVersion: 3,
		CandidatePolicyID: "candidate-policy", CandidatePolicyVersion: 2,
		PercentageBPS: 2500, Salt: "stable-hash", Active: true,
		CreatedBy: 7, CreatedAt: time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC),
	}
	first, err := prepareReviewPolicyRolloutRule(base)
	if err != nil {
		t.Fatal(err)
	}
	base.CreatedBy = 99
	base.CreatedAt = base.CreatedAt.Add(time.Hour)
	second, err := prepareReviewPolicyRolloutRule(base)
	if err != nil {
		t.Fatal(err)
	}
	if first.RuleHash == "" || first.RuleHash != second.RuleHash {
		t.Fatalf("rule hashes = %q and %q", first.RuleHash, second.RuleHash)
	}
	second.PercentageBPS++
	if _, err := prepareReviewPolicyRolloutRule(second); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mutated rule hash error = %v, want ErrInvalid", err)
	}
}

func TestReviewPolicyRolloutRequiresStableKey(t *testing.T) {
	rule, err := prepareReviewPolicyRolloutRule(ReviewPolicyRolloutRule{
		SubjectKind: SubjectSystemDesign, RuleVersion: 1,
		CandidatePolicyID: "candidate-policy", CandidatePolicyVersion: 2,
		PercentageBPS: 2500, Salt: "stable-key", Active: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectReviewPolicyRollout(rule, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing stable key error = %v, want ErrInvalid", err)
	}
}

func TestStableReviewPolicySelectionKeyPrefersUserAndFallsBackToSubject(t *testing.T) {
	if got := StableReviewPolicySelectionKey(
		42, SubjectKind(" system_design_artifact "), " artifact-1 ",
	); got != "subject:system_design_artifact\x00user:42" {
		t.Fatalf("user stable key = %q", got)
	}
	if got := StableReviewPolicySelectionKey(
		0, SubjectSystemDesign, " artifact-1 ",
	); got != "subject:system_design_artifact\x00id:artifact-1" {
		t.Fatalf("subject stable key = %q", got)
	}
	if got := StableReviewPolicySelectionKey(0, SubjectSystemDesign, "  "); got != "" {
		t.Fatalf("empty stable key = %q", got)
	}
}
