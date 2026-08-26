package app

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	"github.com/dekwanlabs/nasuta/tool"
)

// TestBuildInvestigationCoordinator proves the production assembly constructs a
// coordinator wired to the live schema catalog, tool registry, and agent runtime.
// It deliberately stops at candidate generation: the non-provider execution loop is
// already covered end to end in the investigation package's closed_loop_test.go.
func TestBuildInvestigationCoordinator(t *testing.T) {
	platform := qaRuntimeTestPlatform(t)
	settings := enabledAgentSettings()

	definitions, err := defaultAgentDefinitions(settings, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.agents.catalog.Publish(definitions); err != nil {
		t.Fatal(err)
	}

	platform.agents.runtime = staticWorkflowAgentRuntime{}
	platform.registry = tool.NewRegistry()

	coordinator, err := platform.buildInvestigationCoordinator(settings)
	if err != nil {
		t.Fatal(err)
	}
	if coordinator.Catalog == nil {
		t.Fatal("coordinator catalog is nil")
	}
	if coordinator.Executors == nil {
		t.Fatal("coordinator executors is nil")
	}
	if coordinator.Store == nil {
		t.Fatal("coordinator store is nil")
	}
	if coordinator.Schemas == nil {
		t.Fatal("coordinator schemas is nil")
	}
	if coordinator.BudgetProfile != investigation.ProfileInteractive {
		t.Fatalf("coordinator budget profile = %q, want %q", coordinator.BudgetProfile, investigation.ProfileInteractive)
	}
	if coordinator.PolicyVersion != investigation.DefaultBudgetPolicyVersion {
		t.Fatalf("coordinator policy version = %q, want %q", coordinator.PolicyVersion, investigation.DefaultBudgetPolicyVersion)
	}
	if coordinator.MaxRounds != settings.InvestigationMaxRounds {
		t.Fatalf("coordinator max rounds = %d, want %d", coordinator.MaxRounds, settings.InvestigationMaxRounds)
	}
	if coordinator.MaxParallelism != settings.InvestigationMaxParallelism {
		t.Fatalf("coordinator max parallelism = %d, want %d", coordinator.MaxParallelism, settings.InvestigationMaxParallelism)
	}

	contract := investigation.InvestigationContract{
		Version:       investigation.InvestigationContractVersion,
		ID:            "code-entrypoint",
		Question:      "where does hsas-aiot-service call the AI model?",
		EvidenceGoals: []investigation.EvidenceGoal{{ID: "g1", Kind: investigation.GoalKindEntrypoint, Required: true}},
	}
	candidates, err := coordinator.Catalog.GenerateCandidates(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) == 0 {
		t.Fatal("default templates produced no candidates for an entrypoint contract")
	}
}
