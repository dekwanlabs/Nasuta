package investigation

import (
	"errors"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/platform/config"
)

func TestParseBudgetProfileRejectsUnknown(t *testing.T) {
	if _, err := ParseBudgetProfile("turbo"); err == nil {
		t.Fatal("unknown profile unexpectedly parsed")
	}
	if profile, err := ParseBudgetProfile("  DEEP  "); err != nil || profile != ProfileDeep {
		t.Fatalf("deep profile = %q, err = %v", profile, err)
	}
}

func TestAllocateRoleBudgetUsesTheRunTotalForVerifierAndComposition(t *testing.T) {
	runLimit := BudgetVector{
		InputTokens:  300_000,
		OutputTokens: 128_000,
		TotalTokens:  512_000,
		ToolCalls:    40,
		Duration:     10 * time.Minute,
		CostMicros:   1000,
	}
	// Role shares only split token/cost capacity. Duration and tool calls stay
	// zero so a 10% share never becomes a 1-minute child deadline or a
	// shrunken tool-call quota.
	want := BudgetVector{InputTokens: 30_000, OutputTokens: 12_800, TotalTokens: 51_200, CostMicros: 100}
	for _, stage := range []BudgetStage{StageVerification, StageComposition} {
		got, err := AllocateRoleBudget(ProfileInteractive, runLimit, stage)
		if err != nil {
			t.Fatalf("AllocateRoleBudget(%q): %v", stage, err)
		}
		if got != want {
			t.Fatalf("AllocateRoleBudget(%q) = %+v, want %+v", stage, got, want)
		}
		if got.Duration != 0 {
			t.Fatalf("AllocateRoleBudget(%q) duration = %v, want 0", stage, got.Duration)
		}
		if got.ToolCalls != 0 {
			t.Fatalf("AllocateRoleBudget(%q) tool calls = %d, want 0", stage, got.ToolCalls)
		}
	}
}

func TestAllocateStageLimitsScalesOnlyPositiveDimensions(t *testing.T) {
	limits, err := AllocateStageLimits(ProfileInteractive, BudgetVector{
		InputTokens: 1000,
		ToolCalls:   10,
		CostMicros:  100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := limits[StageExecution].InputTokens; got != 700 {
		t.Fatalf("execution input tokens = %d, want 700", got)
	}
	if got := limits[StageComposition].ToolCalls; got != 1 {
		t.Fatalf("composition tool calls = %d, want 1", got)
	}
	if got := limits[StageFallback].CostMicros; got != 5 {
		t.Fatalf("fallback cost micros = %d, want 5", got)
	}
	// Unbounded dimensions stay zero and never invent a cap.
	if got := limits[StagePlanning].OutputTokens; got != 0 {
		t.Fatalf("planning output tokens = %d, want 0", got)
	}
	if got := limits[StagePlanning].Duration; got != 0 {
		t.Fatalf("planning duration = %v, want 0", got)
	}
}

func TestBudgetPolicyFromPlatformSettings(t *testing.T) {
	settings := config.PlatformSettings{
		InvestigationMaxInputTokens:  2000,
		InvestigationMaxOutputTokens: 800,
		InvestigationMaxTotalTokens:  1600,
		InvestigationMaxToolCalls:    12,
		InvestigationMaxDuration:     config.Duration(1),
		InvestigationMaxRounds:       4,
		InvestigationMaxTasks:        8,
		InvestigationBudgetProfile:   "interactive",
	}
	policy, err := BudgetPolicyFromPlatformSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Profile != ProfileInteractive || policy.MaxRounds != 4 || policy.MaxTasks != 8 {
		t.Fatalf("policy = %#v", policy)
	}
	if policy.Limit.InputTokens != 2000 || policy.Limit.TotalTokens != 1600 || policy.Limit.ToolCalls != 12 {
		t.Fatalf("policy limit = %#v", policy.Limit)
	}

	settings.InvestigationBudgetProfile = "nope"
	if _, err := BudgetPolicyFromPlatformSettings(settings); err == nil {
		t.Fatal("unknown platform profile unexpectedly resolved")
	}
}

func TestRunBudgetSnapshotCarriesPolicy(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetRunPolicy(3, 5, DefaultBudgetPolicyVersion, ProfileDeep); err != nil {
		t.Fatal(err)
	}
	run := ledger.Snapshot().Run
	if run.MaxRounds != 3 || run.MaxTasks != 5 || run.PolicyVersion != DefaultBudgetPolicyVersion || run.Profile != string(ProfileDeep) {
		t.Fatalf("run policy snapshot = %#v", run)
	}
}

func TestPlanAdmissionRejectsInsufficientMinimumBudget(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{InputTokens: 1})); err != nil {
		t.Fatal(err)
	}
	// Composition is hard-reserved first; the remaining run budget must still
	// cover the task plus planning, verification and fallback overhead.
	runLimit := BudgetVector{InputTokens: 100}
	ledger, err := NewBudgetLedger(runLimit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(StageComposition, "composition", BudgetVector{InputTokens: 90}); err != nil {
		t.Fatal(err)
	}
	overhead := BudgetVector{InputTokens: 30}
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	_, err = (PlanCompiler{
		Catalog:  catalog,
		Schemas:  testSchemas(),
		Ledger:   ledger,
		Overhead: overhead,
	}).CompileGenerated(contract)
	if err == nil || !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("minimum budget error = %v, want ErrBudgetExceeded", err)
	}
}

func TestPlanAdmissionPassesSufficientMinimumBudget(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{InputTokens: 1})); err != nil {
		t.Fatal(err)
	}
	runLimit := BudgetVector{InputTokens: 100}
	ledger, err := NewBudgetLedger(runLimit)
	if err != nil {
		t.Fatal(err)
	}
	overhead := BudgetVector{InputTokens: 30}
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	plan, err := (PlanCompiler{
		Catalog:  catalog,
		Schemas:  testSchemas(),
		Ledger:   ledger,
		Overhead: overhead,
	}).CompileGenerated(contract)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("plan task count = %d, want 1", len(plan.Tasks))
	}
}

func TestBudgetPolicyUsesInvestigationOutputAsRunLimit(t *testing.T) {
	settings := config.PlatformSettings{
		LLMAnswerMaxTokens:           12000,
		InvestigationMaxOutputTokens: 8000,
		InvestigationBudgetProfile:   "interactive",
	}
	policy, err := BudgetPolicyFromPlatformSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	if policy.Limit.OutputTokens != 8000 {
		t.Fatalf("run output limit = %d, want 8000", policy.Limit.OutputTokens)
	}
}

func TestCompositionProtectionDoesNotReserveEntireRun(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 12000})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetRunPolicy(1, 10, DefaultBudgetPolicyVersion, ProfileInteractive); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		CompositionBudget: BudgetVector{OutputTokens: 12000},
	})
	reservation, err := coordinator.reserveComposition(ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.Release()
	if got := ledger.Snapshot().Run.Reserved.OutputTokens; got != composerMinimumOutputTokens {
		t.Fatalf("composition protection = %d, want %d", got, composerMinimumOutputTokens)
	}
}
