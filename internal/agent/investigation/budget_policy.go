package investigation

import (
	"fmt"
	"strings"
	"time"
)

// BudgetProfile names a frozen stage-allocation strategy. A profile only scales
// dimensions the platform already bounded; it never invents a new hard limit.
type BudgetProfile string

const (
	ProfileInteractive BudgetProfile = "interactive"
	ProfileDeep        BudgetProfile = "deep"
)

// DefaultBudgetPolicyVersion is the policy revision frozen into a run snapshot
// when the caller does not pin one.
const DefaultBudgetPolicyVersion = "v1"

// StageAllocation records the starting share of the run limit reserved for each
// stage. Planning, execution and verification may be reallocated from the unused
// pool in later rounds; composition and fallback stay hard reserves.
type StageAllocation struct {
	Planning     float64
	Execution    float64
	Verification float64
	Composition  float64
	Fallback     float64
}

var budgetProfiles = map[BudgetProfile]StageAllocation{
	ProfileInteractive: {
		Planning: 0.05, Execution: 0.70, Verification: 0.10, Composition: 0.10, Fallback: 0.05,
	},
	ProfileDeep: {
		Planning: 0.05, Execution: 0.70, Verification: 0.10, Composition: 0.10, Fallback: 0.05,
	},
}

// ParseBudgetProfile normalizes a persisted or request-supplied profile name.
func ParseBudgetProfile(value string) (BudgetProfile, error) {
	profile := BudgetProfile(strings.ToLower(strings.TrimSpace(value)))
	if _, ok := budgetProfiles[profile]; !ok {
		return "", fmt.Errorf("unknown budget profile %q", value)
	}
	return profile, nil
}

func (profile BudgetProfile) Allocation() (StageAllocation, error) {
	allocation, ok := budgetProfiles[profile]
	if !ok {
		return StageAllocation{}, fmt.Errorf("unknown budget profile %q", profile)
	}
	return allocation, nil
}

// AllocateStageLimits derives each stage cap from the run hard limit. Dimensions
// left unbounded by the platform (zero) stay zero so a profile cannot turn an
// unbounded cost or empty token budget into a new cap.
func AllocateStageLimits(profile BudgetProfile, runLimit BudgetVector) (map[BudgetStage]BudgetVector, error) {
	allocation, err := profile.Allocation()
	if err != nil {
		return nil, err
	}
	if err := validateBudgetVector(runLimit); err != nil {
		return nil, err
	}
	return map[BudgetStage]BudgetVector{
		StagePlanning:     scaleBudgetVector(runLimit, allocation.Planning),
		StageExecution:    scaleBudgetVector(runLimit, allocation.Execution),
		StageVerification: scaleBudgetVector(runLimit, allocation.Verification),
		StageComposition:  scaleBudgetVector(runLimit, allocation.Composition),
		StageFallback:     scaleBudgetVector(runLimit, allocation.Fallback),
	}, nil
}

// scaleBudgetVector scales only positive dimensions. Zero dimensions keep their
// run-level semantics: zero budget for tokens/tool calls/duration and unlimited
// for cost.
func scaleBudgetVector(limit BudgetVector, ratio float64) BudgetVector {
	return BudgetVector{
		InputTokens:  scalePositive(limit.InputTokens, ratio),
		OutputTokens: scalePositive(limit.OutputTokens, ratio),
		TotalTokens:  scalePositive(limit.TotalTokens, ratio),
		ToolCalls:    int(scalePositive(int64(limit.ToolCalls), ratio)),
		Duration:     time.Duration(scalePositive(int64(limit.Duration), ratio)),
		CostMicros:   scalePositive(limit.CostMicros, ratio),
	}
}

func scalePositive(value int64, ratio float64) int64 {
	if value <= 0 {
		return 0
	}
	return int64(float64(value) * ratio)
}
