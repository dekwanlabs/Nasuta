package app

import (
	"testing"

	"github.com/dekwanlabs/nasuta/platform/config"
)

func TestFeatureGenerationUsesLargestConfiguredAnswerBudget(t *testing.T) {
	settings := &config.PlatformSettings{
		LLMMaxTokens: 4000, LLMAnswerMaxTokens: 6000, LLMConclusionMaxTokens: 12000,
	}
	if got := featureGenerationTokenBudget(settings); got != 12000 {
		t.Fatalf("feature generation token budget=%d, want 12000", got)
	}
}
