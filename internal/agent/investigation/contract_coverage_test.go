package investigation

import (
	"errors"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestContractBuilderOverviewDerivesAllRequiredFacets(t *testing.T) {
	contract, err := (ContractBuilder{}).Build(ContractRequest{
		ID:       "overview",
		Question: "give me a full overview of hsas-aiot-service",
		Kind:     domain.QueryOverview,
		Entities: []string{"hsas-aiot-service"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if contract.Version != InvestigationContractVersion {
		t.Fatalf("contract version = %d, want %d", contract.Version, InvestigationContractVersion)
	}
	if len(contract.EvidenceGoals) != len(domain.RequiredFacetsFor(domain.QueryOverview)) {
		t.Fatalf("goal count = %d", len(contract.EvidenceGoals))
	}
	for _, goal := range contract.EvidenceGoals {
		if len(goal.Facets) != 1 || goal.Facets[0] != goal.Kind {
			t.Fatalf("goal %q facets = %v", goal.ID, goal.Facets)
		}
		if !goal.Required {
			t.Fatalf("goal %q is not required", goal.ID)
		}
	}
}

func TestDefaultCatalogCoversEveryRequiredFacet(t *testing.T) {
	contract, err := (ContractBuilder{}).Build(ContractRequest{
		ID:       "overview",
		Question: "give me a full overview of hsas-aiot-service",
		Kind:     domain.QueryOverview,
		Entities: []string{"hsas-aiot-service"},
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewTaskTemplateCatalog()
	if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	candidates, err := catalog.GenerateCandidates(contract)
	if err != nil {
		if errors.Is(err, ErrCapabilityGap) {
			t.Fatalf("default catalog has a facet coverage gap: %v", err)
		}
		t.Fatal(err)
	}
	covered := make(map[string]bool, len(contract.EvidenceGoals))
	for _, candidate := range candidates {
		for _, goalID := range candidate.EvidenceGoalIDs {
			covered[goalID] = true
		}
	}
	for _, goal := range contract.EvidenceGoals {
		if !covered[goal.ID] {
			t.Fatalf("required goal %q has no candidate task", goal.ID)
		}
	}
}

func TestContractBuilderAllQueryKindsDeriveRequiredFacets(t *testing.T) {
	for _, kind := range []domain.QueryKind{
		domain.QueryFocusedFact,
		domain.QueryOverview,
		domain.QueryFlow,
		domain.QueryComparison,
		domain.QueryInventory,
		domain.QueryRuntimeDiagnosis,
		domain.QueryCodeReview,
	} {
		t.Run(string(kind), func(t *testing.T) {
			contract, err := (ContractBuilder{}).Build(ContractRequest{
				ID:       "contract",
				Question: "analyze hsas-aiot-service",
				Kind:     kind,
				Entities: []string{"hsas-aiot-service"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(contract.EvidenceGoals) == 0 {
				t.Fatal("contract has no evidence goals")
			}
			catalog := NewTaskTemplateCatalog()
			if err := RegisterDefaultInvestigationTemplates(catalog); err != nil {
				t.Fatal(err)
			}
			candidates, err := catalog.GenerateCandidates(contract)
			if err != nil {
				t.Fatalf("candidate generation failed: %v", err)
			}
			covered := make(map[string]bool, len(contract.EvidenceGoals))
			for _, candidate := range candidates {
				for _, goalID := range candidate.EvidenceGoalIDs {
					covered[goalID] = true
				}
			}
			for _, goal := range contract.EvidenceGoals {
				if goal.Required && !covered[goal.ID] {
					t.Fatalf("required goal %q has no candidate task", goal.ID)
				}
			}
		})
	}
}

func TestDefaultCatalogSchemasResolveFromStandardCatalog(t *testing.T) {
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []agentapi.SchemaRef{
		{ID: DefaultTaskInputSchema, Version: 1},
		{ID: DefaultTaskOutputSchema, Version: 1},
	} {
		if _, err := registry.Resolve(ref); err != nil {
			t.Fatalf("default schema %s not resolvable: %v", ref.ID, err)
		}
	}
}

func TestEvidenceAndClaimIDsAreShortCitationTokens(t *testing.T) {
	evidence := evidenceID(EvidenceCandidate{
		SourceKind: "code", Target: "service-a", Section: "L10-L20",
		Version: "v1", TimeRange: "release-1", ContentHash: "abc123",
	})
	if len(evidence) != 16 || !strings.HasPrefix(evidence, "evidence_") {
		t.Fatalf("evidence id = %q (len %d), want 16 chars prefixed with evidence_", evidence, len(evidence))
	}
	claim := claimID(ClaimCandidate{GoalID: "g1", Text: "the flow is supported"})
	if len(claim) != 16 || !strings.HasPrefix(claim, "claim_") {
		t.Fatalf("claim id = %q (len %d), want 16 chars prefixed with claim_", claim, len(claim))
	}
	if evidence == strings.Replace(claim, "claim_", "evidence_", 1) {
		t.Fatal("evidence and claim handles must not collide across namespaces")
	}
}
