package featuredelivery

import (
	"slices"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestPrepareReviewPanelSelectsOptionalReviewersByRisk(t *testing.T) {
	policy := dynamicReviewPolicy(t)
	lowFacts, err := BuildArtifactReviewRiskFacts(Artifact{})
	if err != nil {
		t.Fatal(err)
	}
	lowFacts, lowRiskHash, lowPanel, lowPanelHash, err := PrepareReviewPanel(policy, lowFacts)
	if err != nil {
		t.Fatal(err)
	}
	if ids := reviewerIDs(lowPanel); !slices.Equal(ids, []string{"architecture", "security"}) {
		t.Fatalf("low-risk panel = %v", ids)
	}

	run, _, _ := implementationReviewFixture()
	run.ChangeSet.FilesChanged = 10
	highFacts, err := BuildImplementationReviewRiskFacts(SubjectChangeSet, run)
	if err != nil {
		t.Fatal(err)
	}
	highFacts, highRiskHash, highPanel, highPanelHash, err := PrepareReviewPanel(policy, highFacts)
	if err != nil {
		t.Fatal(err)
	}
	if ids := reviewerIDs(highPanel); !slices.Equal(
		ids,
		[]string{"architecture", "security", "operations"},
	) {
		t.Fatalf("high-risk panel = %v", ids)
	}
	if lowRiskHash == highRiskHash || lowPanelHash == highPanelHash {
		t.Fatalf(
			"risk and panel hashes did not change: low=%s/%s high=%s/%s",
			lowRiskHash,
			lowPanelHash,
			highRiskHash,
			highPanelHash,
		)
	}

	slices.Reverse(lowFacts)
	_, repeatedRiskHash, repeatedPanel, repeatedPanelHash, err := PrepareReviewPanel(policy, lowFacts)
	if err != nil {
		t.Fatal(err)
	}
	if repeatedRiskHash != lowRiskHash || repeatedPanelHash != lowPanelHash ||
		!slices.Equal(reviewerIDs(repeatedPanel), reviewerIDs(lowPanel)) {
		t.Fatalf(
			"canonical snapshot changed: risk=%s panel=%s reviewers=%v",
			repeatedRiskHash,
			repeatedPanelHash,
			reviewerIDs(repeatedPanel),
		)
	}
}

func TestValidateReviewRoundSnapshotUsesFrozenPanel(t *testing.T) {
	policy := dynamicReviewPolicy(t)
	facts, err := BuildArtifactReviewRiskFacts(Artifact{})
	if err != nil {
		t.Fatal(err)
	}
	facts, riskHash, reviewers, panelHash, err := PrepareReviewPanel(policy, facts)
	if err != nil {
		t.Fatal(err)
	}
	round := ReviewRound{
		RiskFacts: facts, RiskHash: riskHash,
		RuleVersion: policy.RiskRuleVersion,
		Reviewers:   reviewers, PanelHash: panelHash,
	}

	run, _, _ := implementationReviewFixture()
	run.ChangeSet.FilesChanged = 10
	if _, err := BuildImplementationReviewRiskFacts(SubjectChangeSet, run); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReviewRoundSnapshot(policy, round); err != nil {
		t.Fatal(err)
	}
	if ids := reviewerIDs(round.Reviewers); !slices.Equal(ids, []string{"architecture", "security"}) {
		t.Fatalf("frozen panel changed during validation: %v", ids)
	}
}

func dynamicReviewPolicy(t *testing.T) ReviewPolicy {
	t.Helper()
	policy, err := PrepareReviewPolicy(ReviewPolicy{
		ID: "dynamic-review", Version: 1, SubjectKind: SubjectSystemDesign,
		Reviewers: []ReviewerSpec{
			{
				ID: "architecture",
				Agent: agentapi.DefinitionRef{
					ID: "review.architecture", Version: 1,
				},
				DefinitionHash: "architecture-hash",
				Categories:     []string{"architecture"}, Required: true, ReadOnly: true,
			},
			{
				ID: "security",
				Agent: agentapi.DefinitionRef{
					ID: "review.security", Version: 1,
				},
				DefinitionHash: "security-hash",
				Categories:     []string{"security"}, Required: true, ReadOnly: true,
			},
			{
				ID: "operations",
				Agent: agentapi.DefinitionRef{
					ID: "review.operations", Version: 1,
				},
				DefinitionHash: "operations-hash",
				Categories:     []string{"operations"}, ReadOnly: true,
			},
		},
		BlockingSeverities:     []Severity{SeverityCritical, SeverityHigh},
		RequiredCategories:     []string{"architecture", "security"},
		MaxParallelism:         3,
		MaxInputTokens:         3,
		MaxOutputTokens:        3,
		MaxTotalTokens:         3,
		MaxToolCalls:           3,
		MaxCostMicros:          3,
		MaxRetries:             1,
		Timeout:                time.Minute,
		OptionalReviewerAction: OptionalReviewerContinue,
		RiskRuleVersion:        "change-risk.v1",
		RiskRules: []ReviewRiskRule{{
			ID: "large-change",
			Conditions: []ReviewRiskCondition{{
				Fact: RiskFactFilesChanged, Operator: RiskGreaterThanOrEqual, Value: 10,
			}},
			ReviewerIDs: []string{"operations"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func reviewerIDs(reviewers []ReviewerSpec) []string {
	ids := make([]string, 0, len(reviewers))
	for _, reviewer := range reviewers {
		ids = append(ids, reviewer.ID)
	}
	return ids
}
