package delivery

import (
	"fmt"
	"strconv"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const (
	defaultArchitectureReviewer = "review.architecture"
	defaultSecurityReviewer     = "review.security"
	defaultReliabilityReviewer  = "review.reliability"
	defaultReviewAdjudicator    = "review.adjudicator"
	defaultReviewInputTokens    = int64(32_000)
	defaultReviewOutputTokens   = int64(4_096)
	defaultReviewTotalTokens    = int64(40_000)
	defaultReviewToolCalls      = int64(16)
	defaultReviewCostMicros     = int64(1_000_000)
	defaultReviewTimeout        = 2 * time.Hour
)

type defaultReviewerPanel struct {
	definitionID string
	categories   []string
}

type defaultReviewPolicySpec struct {
	kind       SubjectKind
	reviewers  []defaultReviewerPanel
	categories []string
}

var defaultReviewPolicySpecs = []defaultReviewPolicySpec{
	{
		kind: SubjectRequirement,
		reviewers: []defaultReviewerPanel{
			{defaultArchitectureReviewer, []string{"problem_scope", "constraints", "consistency"}},
			{defaultReliabilityReviewer, []string{"acceptance_testability", "blocking_questions"}},
		},
		categories: []string{"problem_scope", "constraints", "consistency", "acceptance_testability", "blocking_questions"},
	},
	{
		kind: SubjectRequirementAnalysis,
		reviewers: []defaultReviewerPanel{
			{defaultArchitectureReviewer, []string{"goal_alignment", "domain_rules", "scope_boundaries"}},
			{defaultReliabilityReviewer, []string{"testability", "success_metrics", "blocking_questions"}},
		},
		categories: []string{"goal_alignment", "domain_rules", "scope_boundaries", "testability", "success_metrics", "blocking_questions"},
	},
	{
		kind: SubjectTechnicalProposal,
		reviewers: []defaultReviewerPanel{
			{defaultArchitectureReviewer, []string{"architecture", "compatibility"}},
			{defaultSecurityReviewer, []string{"security"}},
			{defaultReliabilityReviewer, []string{"reliability", "operability", "reversibility"}},
		},
		categories: []string{"architecture", "compatibility", "security", "reliability", "operability", "reversibility"},
	},
	{
		kind: SubjectSystemDesign,
		reviewers: []defaultReviewerPanel{
			{defaultArchitectureReviewer, []string{"boundaries", "contracts", "data_ownership"}},
			{defaultSecurityReviewer, []string{"security"}},
			{defaultReliabilityReviewer, []string{"concurrency", "recovery", "observability"}},
		},
		categories: []string{"boundaries", "contracts", "data_ownership", "security", "concurrency", "recovery", "observability"},
	},
	{
		kind: SubjectImplementationPlan,
		reviewers: []defaultReviewerPanel{
			{defaultArchitectureReviewer, []string{"implementation_scope", "dependency_order", "contracts"}},
			{defaultReliabilityReviewer, []string{"testing", "migration", "rollback", "completion_criteria"}},
		},
		categories: []string{"implementation_scope", "dependency_order", "contracts", "testing", "migration", "rollback", "completion_criteria"},
	},
	{
		kind: SubjectChangeSet,
		reviewers: []defaultReviewerPanel{
			{defaultArchitectureReviewer, []string{"correctness", "scope", "compatibility"}},
			{defaultSecurityReviewer, []string{"security"}},
			{defaultReliabilityReviewer, []string{"testing", "concurrency", "operability"}},
		},
		categories: []string{"correctness", "scope", "compatibility", "security", "testing", "concurrency", "operability"},
	},
	{
		kind: SubjectValidationBundle,
		reviewers: []defaultReviewerPanel{
			{defaultArchitectureReviewer, []string{"coverage", "behavioral_contracts"}},
			{defaultReliabilityReviewer, []string{"test_results", "regression_risk", "failure_analysis"}},
		},
		categories: []string{"coverage", "behavioral_contracts", "test_results", "regression_risk", "failure_analysis"},
	},
	{
		kind: SubjectDeliveryBundle,
		reviewers: []defaultReviewerPanel{
			{defaultArchitectureReviewer, []string{"subject_consistency", "compatibility"}},
			{defaultSecurityReviewer, []string{"security"}},
			{defaultReliabilityReviewer, []string{"release_readiness", "monitoring", "rollback", "residual_risk"}},
		},
		categories: []string{"subject_consistency", "compatibility", "security", "release_readiness", "monitoring", "rollback", "residual_risk"},
	},
}

// DefaultReviewPolicies binds the standard Feature Delivery panel to exact Definitions.
func DefaultReviewPolicies(
	definitions []agentapi.Definition,
	createdAt time.Time,
) ([]ReviewPolicy, error) {
	byID := make(map[string]agentapi.Definition, len(definitions))
	for _, definition := range definitions {
		prepared, err := agentapi.Prepare(definition)
		if err != nil {
			return nil, fmt.Errorf("prepare default reviewer %q: %w", definition.ID, err)
		}
		if _, duplicate := byID[prepared.ID]; duplicate {
			return nil, fmt.Errorf("duplicate default reviewer %q: %w", prepared.ID, ErrInvalid)
		}
		byID[prepared.ID] = prepared
	}

	policies := make([]ReviewPolicy, 0, len(defaultReviewPolicySpecs))
	adjudicator, ok := byID[defaultReviewAdjudicator]
	if !ok {
		return nil, fmt.Errorf("default adjudicator %q is unavailable: %w", defaultReviewAdjudicator, ErrInvalid)
	}
	for _, spec := range defaultReviewPolicySpecs {
		reviewers := make([]ReviewerSpec, 0, len(spec.reviewers))
		for _, panel := range spec.reviewers {
			definition, ok := byID[panel.definitionID]
			if !ok {
				return nil, fmt.Errorf("default reviewer %q is unavailable: %w", panel.definitionID, ErrInvalid)
			}
			reviewers = append(reviewers, ReviewerSpec{
				ID: definition.ID,
				Agent: agentapi.DefinitionRef{
					ID: definition.ID, Version: definition.Version,
				},
				DefinitionHash: definition.ContentHash,
				Categories:     append([]string(nil), panel.categories...),
				Required:       true,
				ReadOnly:       true,
			})
		}
		policy := ReviewPolicy{
			ID:          "default." + string(spec.kind),
			SubjectKind: spec.kind,
			Reviewers:   reviewers,
			Adjudicator: &AdjudicatorSpec{
				Agent: agentapi.DefinitionRef{
					ID: adjudicator.ID, Version: adjudicator.Version,
				},
				DefinitionHash: adjudicator.ContentHash,
				ReadOnly:       true,
			},
			BlockingSeverities:     []Severity{SeverityCritical, SeverityHigh},
			RequiredCategories:     append([]string(nil), spec.categories...),
			MaxParallelism:         len(reviewers),
			MaxInputTokens:         defaultReviewInputTokens * int64(len(reviewers)+1),
			MaxOutputTokens:        defaultReviewOutputTokens * int64(len(reviewers)+1),
			MaxTotalTokens:         defaultReviewTotalTokens * int64(len(reviewers)+1),
			MaxToolCalls:           defaultReviewToolCalls * int64(len(reviewers)+1),
			MaxCostMicros:          defaultReviewCostMicros * int64(len(reviewers)+1),
			MaxRetries:             int64(len(reviewers) + 1),
			Timeout:                defaultReviewTimeout,
			OptionalReviewerAction: OptionalReviewerContinue,
			CreatedAt:              createdAt,
		}
		version, err := defaultReviewPolicyVersion(policy)
		if err != nil {
			return nil, err
		}
		policy.Version = version
		prepared, err := PrepareReviewPolicy(policy)
		if err != nil {
			return nil, fmt.Errorf("prepare default policy for %q: %w", spec.kind, err)
		}
		policies = append(policies, prepared)
	}
	return policies, nil
}

// Built-in versions follow panel content so restarts do not reuse a changed policy version.
func defaultReviewPolicyVersion(policy ReviewPolicy) (int64, error) {
	policy.Version = 0
	policy.ContentHash = ""
	policy.CreatedAt = time.Time{}
	hash, err := hashJSON(policy)
	if err != nil {
		return 0, fmt.Errorf("hash default review policy %q: %w", policy.ID, err)
	}
	version, err := strconv.ParseInt(hash[:15], 16, 64)
	if err != nil {
		return 0, fmt.Errorf("derive default review policy %q version: %w", policy.ID, err)
	}
	if version == 0 {
		return 1, nil
	}
	return version, nil
}
