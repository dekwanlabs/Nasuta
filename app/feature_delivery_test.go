package app

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
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

func TestFeatureDeliveryStatusReportsCodingInitializationFailure(t *testing.T) {
	runtime := featureDeliveryRuntime{
		service:      featuredelivery.NewService(nil, nil, 0),
		codingReason: "workspace_unavailable",
	}

	status := runtime.status(context.Background())
	if status.Coding.Reason != "workspace_unavailable" {
		t.Fatalf("Coding.Reason = %q, want workspace_unavailable", status.Coding.Reason)
	}
}
