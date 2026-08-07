package featuredelivery

import (
	"errors"
	"slices"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestDefaultReviewPoliciesBindEverySubjectToExactReviewers(t *testing.T) {
	definitions := defaultReviewerDefinitions(t, 7)
	first, err := DefaultReviewPolicies(
		definitions,
		time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DefaultReviewPolicies(
		definitions,
		time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}

	expectedPanels := map[SubjectKind][]string{
		SubjectRequirement:         {defaultArchitectureReviewer, defaultReliabilityReviewer},
		SubjectRequirementAnalysis: {defaultArchitectureReviewer, defaultReliabilityReviewer},
		SubjectTechnicalProposal:   {defaultArchitectureReviewer, defaultSecurityReviewer, defaultReliabilityReviewer},
		SubjectSystemDesign:        {defaultArchitectureReviewer, defaultSecurityReviewer, defaultReliabilityReviewer},
		SubjectImplementationPlan:  {defaultArchitectureReviewer, defaultReliabilityReviewer},
		SubjectChangeSet:           {defaultArchitectureReviewer, defaultSecurityReviewer, defaultReliabilityReviewer},
		SubjectValidationBundle:    {defaultArchitectureReviewer, defaultReliabilityReviewer},
		SubjectDeliveryBundle:      {defaultArchitectureReviewer, defaultSecurityReviewer, defaultReliabilityReviewer},
	}
	definitionsByID := make(map[string]agentapi.Definition, len(definitions))
	for _, definition := range definitions {
		definitionsByID[definition.ID] = definition
	}
	if len(first) != len(expectedPanels) || len(second) != len(expectedPanels) {
		t.Fatalf("default policies = %d/%d, want %d", len(first), len(second), len(expectedPanels))
	}

	seen := make(map[SubjectKind]struct{}, len(first))
	for index, policy := range first {
		panel, ok := expectedPanels[policy.SubjectKind]
		if !ok {
			t.Fatalf("unexpected policy subject %q", policy.SubjectKind)
		}
		if _, duplicate := seen[policy.SubjectKind]; duplicate {
			t.Fatalf("duplicate policy subject %q", policy.SubjectKind)
		}
		seen[policy.SubjectKind] = struct{}{}
		if policy.ID != "default."+string(policy.SubjectKind) ||
			len(policy.Reviewers) != len(panel) ||
			policy.MaxParallelism != len(panel) ||
			!slices.Equal(policy.BlockingSeverities, []Severity{SeverityCritical, SeverityHigh}) {
			t.Fatalf("policy %q = %+v", policy.SubjectKind, policy)
		}
		for reviewerIndex, reviewer := range policy.Reviewers {
			definition := definitionsByID[panel[reviewerIndex]]
			if reviewer.ID != definition.ID ||
				reviewer.Agent != (agentapi.DefinitionRef{ID: definition.ID, Version: definition.Version}) ||
				reviewer.DefinitionHash != definition.ContentHash ||
				!reviewer.Required || !reviewer.ReadOnly {
				t.Fatalf("policy %q reviewer %d = %+v", policy.SubjectKind, reviewerIndex, reviewer)
			}
		}
		adjudicator := definitionsByID[defaultReviewAdjudicator]
		if policy.Adjudicator == nil ||
			policy.Adjudicator.Agent != (agentapi.DefinitionRef{
				ID:      adjudicator.ID,
				Version: adjudicator.Version,
			}) ||
			policy.Adjudicator.DefinitionHash != adjudicator.ContentHash ||
			!policy.Adjudicator.ReadOnly {
			t.Fatalf("policy %q adjudicator = %+v", policy.SubjectKind, policy.Adjudicator)
		}
		if policy.Version != second[index].Version ||
			policy.ContentHash != second[index].ContentHash ||
			policy.CreatedAt == second[index].CreatedAt {
			t.Fatalf("policy identity changed with CreatedAt: first=%+v second=%+v", policy, second[index])
		}
	}
}

func TestDefaultReviewPoliciesRejectMissingOrDuplicateReviewers(t *testing.T) {
	definitions := defaultReviewerDefinitions(t, 7)
	for _, test := range []struct {
		name        string
		definitions []agentapi.Definition
	}{
		{name: "missing", definitions: definitions[:2]},
		{name: "duplicate", definitions: append(definitions, definitions[0])},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DefaultReviewPolicies(test.definitions, time.Now())
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want invalid", err)
			}
		})
	}
}

func defaultReviewerDefinitions(t *testing.T, version int64) []agentapi.Definition {
	t.Helper()
	ids := []string{
		defaultArchitectureReviewer,
		defaultSecurityReviewer,
		defaultReliabilityReviewer,
		defaultReviewAdjudicator,
	}
	definitions := make([]agentapi.Definition, 0, len(ids))
	for _, id := range ids {
		inputSchema := "review.request"
		outputSchema := "review.report"
		if id == defaultReviewAdjudicator {
			inputSchema = "review.adjudication.request"
			outputSchema = "review.adjudication"
		}
		definition, err := agentapi.Prepare(agentapi.Definition{
			ID: id, Version: version,
			Prompt:       agentapi.PromptSpec{System: "Review independently.", Version: "v1"},
			InputSchema:  agentapi.SchemaRef{ID: inputSchema, Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: outputSchema, Version: 1},
			Model: agentapi.ModelPolicy{
				Provider: "openai", Model: "model", MaxOutputTokens: 1024,
			},
			Budget: agentapi.BudgetPolicy{
				Timeout: time.Minute, MaxSteps: 4, ContextTokens: 32000,
			},
			Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		definitions = append(definitions, definition)
	}
	return definitions
}
