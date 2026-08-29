package investigation

import (
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/platform/config"
)

// BudgetVectorFromPlatformSettings freezes platform hard limits at Run creation time.
func BudgetVectorFromPlatformSettings(settings config.PlatformSettings) BudgetVector {
	return BudgetVector{
		InputTokens:  settings.InvestigationMaxInputTokens,
		OutputTokens: settings.InvestigationMaxOutputTokens,
		TotalTokens:  settings.InvestigationMaxTotalTokens,
		ToolCalls:    int(settings.InvestigationMaxToolCalls),
		Duration:     time.Duration(settings.InvestigationMaxDuration),
		CostMicros:   settings.InvestigationMaxCostMicros,
	}
}

// BudgetPolicy is the frozen run policy resolved from platform settings. A
// request contract may pin a stricter profile but never raises the hard limit.
type BudgetPolicy struct {
	Limit     BudgetVector
	Profile   BudgetProfile
	MaxRounds int
	MaxTasks  int
}

// BudgetPolicyFromPlatformSettings resolves the platform budget policy. An
// unknown profile is an error rather than a silent fallback.
func BudgetPolicyFromPlatformSettings(settings config.PlatformSettings) (BudgetPolicy, error) {
	profile, err := ParseBudgetProfile(settings.InvestigationBudgetProfile)
	if err != nil {
		return BudgetPolicy{}, fmt.Errorf("budget profile: %w", err)
	}
	return BudgetPolicy{
		Limit:     BudgetVectorFromPlatformSettings(settings),
		Profile:   profile,
		MaxRounds: settings.InvestigationMaxRounds,
		MaxTasks:  settings.InvestigationMaxTasks,
	}, nil
}
