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

// StageAllocation records non-token stage controls and the optional
// composition-protection share. Token dimensions are normalized to the shared
// Run limit by the Coordinator and are not split across stages.
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

// AllocateRoleBudget returns the share of the frozen Run budget assigned to
// one downstream role. The returned vector preserves the meaning of zero
// dimensions (unbounded) and is therefore safe to use for input, output, and
// aggregate-token reservations without inventing limits that the Run does not
// have.
//
// Role budgets are allocated from the shared Run and actual usage is still
// charged to that Run ledger. Verifier capacity is protected before any
// Investigator starts, while Synthesizer capacity is requested only when the
// workflow reaches Composition. Agent-backed role Runs also receive the token
// slice as their local input/output/total ceiling.
//
// Only token (and cost) dimensions are split by role. Duration and tool calls
// are NOT scaled: the Run duration belongs to the whole workflow, and handing a
// role a 10% slice of it turns a 10-minute run into a 1-minute child deadline
// (which, after the runtime answer reserve, leaves a single model call with
// only ~30s). Role Runs therefore inherit their own definition timeout instead
// of a shrunken share of the Run clock.
func AllocateRoleBudget(profile BudgetProfile, runLimit BudgetVector, stage BudgetStage) (BudgetVector, error) {
	allocation, err := profile.Allocation()
	if err != nil {
		return BudgetVector{}, err
	}
	if err := validateBudgetVector(runLimit); err != nil {
		return BudgetVector{}, err
	}
	var ratio float64
	switch stage {
	case StageVerification:
		ratio = allocation.Verification
	case StageComposition:
		ratio = allocation.Composition
	default:
		return BudgetVector{}, fmt.Errorf("budget stage %q is not a role allocation", stage)
	}
	return scaleRoleTokenBudget(runLimit, ratio), nil
}

// scaleRoleTokenBudget scales only the token and cost dimensions for a
// downstream role. Duration and tool calls stay zero so a role share never
// becomes a shrunken child deadline or tool-call quota.
func scaleRoleTokenBudget(limit BudgetVector, ratio float64) BudgetVector {
	return BudgetVector{
		InputTokens:  scalePositive(limit.InputTokens, ratio),
		OutputTokens: scalePositive(limit.OutputTokens, ratio),
		TotalTokens:  scalePositive(limit.TotalTokens, ratio),
		CostMicros:   scalePositive(limit.CostMicros, ratio),
	}
}

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

// splitBudgetVector returns one deterministic slice of a pool. Positive
// dimensions are divided as evenly as possible; earlier slices receive the
// integer remainder. Zero keeps the run-level "unbounded" meaning.
func splitBudgetVector(pool BudgetVector, index, count int) BudgetVector {
	if index < 0 || count <= 0 || index >= count {
		return BudgetVector{}
	}
	return BudgetVector{
		InputTokens:  splitBudgetInt64(pool.InputTokens, index, count),
		OutputTokens: splitBudgetInt64(pool.OutputTokens, index, count),
		TotalTokens:  splitBudgetInt64(pool.TotalTokens, index, count),
		ToolCalls:    int(splitBudgetInt64(int64(pool.ToolCalls), index, count)),
		Duration:     time.Duration(splitBudgetInt64(int64(pool.Duration), index, count)),
		CostMicros:   splitBudgetInt64(pool.CostMicros, index, count),
	}
}

func splitBudgetInt64(value int64, index, count int) int64 {
	if value <= 0 {
		return 0
	}
	divisor := int64(count)
	base := value / divisor
	remainder := value % divisor
	if int64(index) < remainder {
		return base + 1
	}
	return base
}

// capBudgetToLimit lets a task-specific limit narrow a role pool without
// allowing the task to expand it. A zero task limit means that dimension is
// not constrained by the task.
func capBudgetToLimit(grant, limit BudgetVector) BudgetVector {
	return BudgetVector{
		InputTokens:  capPositiveBudget(grant.InputTokens, limit.InputTokens),
		OutputTokens: capPositiveBudget(grant.OutputTokens, limit.OutputTokens),
		TotalTokens:  capPositiveBudget(grant.TotalTokens, limit.TotalTokens),
		ToolCalls:    capPositiveBudgetInt(grant.ToolCalls, limit.ToolCalls),
		Duration:     capPositiveBudgetDuration(grant.Duration, limit.Duration),
		CostMicros:   capBudgetCost(grant.CostMicros, limit.CostMicros),
	}
}

func capPositiveBudget(grant, limit int64) int64 {
	if limit > 0 && (grant == 0 || grant > limit) {
		return limit
	}
	return grant
}

func capPositiveBudgetInt(grant, limit int) int {
	if limit > 0 && (grant == 0 || grant > limit) {
		return limit
	}
	return grant
}

func capPositiveBudgetDuration(grant, limit time.Duration) time.Duration {
	if limit > 0 && (grant == 0 || grant > limit) {
		return limit
	}
	return grant
}

func capBudgetCost(grant, limit int64) int64 {
	if limit > 0 && (grant == 0 || grant > limit) {
		return limit
	}
	return grant
}

func scalePositive(value int64, ratio float64) int64 {
	if value <= 0 {
		return 0
	}
	return int64(float64(value) * ratio)
}
