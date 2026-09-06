package llm

import "testing"

func TestDefaultModelCapabilityUsesConservativeProfiles(t *testing.T) {
	tests := []struct {
		name                 string
		provider             string
		model                string
		style                APIStyle
		completionLimitField CompletionLimitField
		reasoningWire        ReasoningWireField
		supportsEffort       bool
	}{
		{
			name:                 "unknown openai-compatible model",
			provider:             "openai",
			model:                "deepseek-v4-flash",
			style:                APIStyleChatCompletions,
			completionLimitField: CompletionLimitMaxTokens,
			reasoningWire:        ReasoningWireNone,
		},
		{
			name:                 "openai reasoning model",
			provider:             "openai",
			model:                "o3-mini",
			style:                APIStyleChatCompletions,
			completionLimitField: CompletionLimitMaxCompletionTokens,
			reasoningWire:        ReasoningWireEffort,
			supportsEffort:       true,
		},
		{
			name:                 "anthropic",
			provider:             "anthropic",
			model:                "claude-sonnet-4",
			style:                APIStyleMessages,
			completionLimitField: CompletionLimitMaxTokens,
			reasoningWire:        ReasoningWireNone,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := DefaultModelCapability(test.provider, test.model)
			if got.APIStyle != test.style || got.CompletionLimitField != test.completionLimitField ||
				got.ReasoningWireField != test.reasoningWire || got.SupportsReasoningEffort != test.supportsEffort {
				t.Fatalf("capability = %+v", got)
			}
		})
	}
}

func TestOpenAIReasoningCapabilityIsExplicit(t *testing.T) {
	got := OpenAIReasoningCapability("custom-reasoning-gateway")
	if got.Provider != "openai" || got.Model != "custom-reasoning-gateway" {
		t.Fatalf("provider/model = %q/%q", got.Provider, got.Model)
	}
	if got.CompletionLimitField != CompletionLimitMaxCompletionTokens {
		t.Fatalf("completion limit field = %s", got.CompletionLimitField)
	}
	if got.ReasoningWireField != ReasoningWireEffort || !got.SupportsReasoningEffort {
		t.Fatalf("reasoning capability = %+v", got)
	}
}
