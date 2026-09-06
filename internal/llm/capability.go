package llm

import "strings"

// APIStyle identifies the provider wire protocol used by an LLM client.
type APIStyle string

const (
	APIStyleChatCompletions APIStyle = "chat_completions"
	APIStyleMessages        APIStyle = "messages"
	APIStyleResponses       APIStyle = "responses"
)

// CompletionLimitField is the provider field used for the logical completion
// limit. A request must set exactly one completion-limit field.
type CompletionLimitField string

const (
	CompletionLimitMaxTokens           CompletionLimitField = "max_tokens"
	CompletionLimitMaxCompletionTokens CompletionLimitField = "max_completion_tokens"
	CompletionLimitMaxOutputTokens     CompletionLimitField = "max_output_tokens"
)

// ReasoningMode controls a provider reasoning/thinking switch when the
// capability profile explicitly supports one.
type ReasoningMode string

const (
	ReasoningDefault  ReasoningMode = "default"
	ReasoningEnabled  ReasoningMode = "enabled"
	ReasoningDisabled ReasoningMode = "disabled"
)

// ReasoningWireField identifies the provider-specific reasoning control.
type ReasoningWireField string

const (
	ReasoningWireNone     ReasoningWireField = ""
	ReasoningWireEffort   ReasoningWireField = "reasoning_effort"
	ReasoningWireThinking ReasoningWireField = "thinking"
)

// ModelCapabilityProfile is the explicit contract between the logical LLM
// request and a provider/model wire format. Unknown models deliberately use a
// conservative chat-completions/max_tokens profile.
type ModelCapabilityProfile struct {
	Provider                     string
	Model                        string
	APIStyle                     APIStyle
	CompletionLimitField         CompletionLimitField
	SupportsThinkingToggle       bool
	SupportsReasoningEffort      bool
	SupportsReasoningUsageDetail bool
	ReasoningWireField           ReasoningWireField
	ReasoningUsageIncludesOutput bool
}

// DefaultModelCapability resolves a safe adapter profile. The current
// OpenAI-compatible DeepSeek route intentionally falls back to max_tokens;
// provider=\"openai\" alone does not prove that max_completion_tokens is
// accepted by a gateway.
func DefaultModelCapability(provider, model string) ModelCapabilityProfile {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	profile := ModelCapabilityProfile{
		Provider:                     provider,
		Model:                        model,
		APIStyle:                     APIStyleChatCompletions,
		CompletionLimitField:         CompletionLimitMaxTokens,
		ReasoningWireField:           ReasoningWireNone,
		ReasoningUsageIncludesOutput: true,
	}
	if provider == "anthropic" {
		profile.APIStyle = APIStyleMessages
		// Anthropic's thinking block is not enabled implicitly. It requires a
		// provider-specific thinking budget, so leave it opt-in until a profile
		// supplies the complete request contract.
		profile.ReasoningUsageIncludesOutput = false
		return profile
	}
	if provider == "openai" && isOpenAIReasoningModel(model) {
		profile.CompletionLimitField = CompletionLimitMaxCompletionTokens
		profile.SupportsReasoningEffort = true
		profile.ReasoningWireField = ReasoningWireEffort
		profile.ReasoningUsageIncludesOutput = true
	}
	return profile
}

// OpenAIReasoningCapability returns an explicit profile for an OpenAI
// Chat-Completions reasoning model. It is intended for callers that have
// verified the gateway contract rather than for arbitrary OpenAI-compatible
// endpoints.
func OpenAIReasoningCapability(model string) ModelCapabilityProfile {
	profile := DefaultModelCapability("openai", model)
	profile.CompletionLimitField = CompletionLimitMaxCompletionTokens
	profile.SupportsReasoningEffort = true
	profile.ReasoningWireField = ReasoningWireEffort
	profile.ReasoningUsageIncludesOutput = true
	return profile
}

func isOpenAIReasoningModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"o1", "o3", "o4", "gpt-5"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return true
		}
	}
	return false
}

func (profile ModelCapabilityProfile) normalized() ModelCapabilityProfile {
	profile.Provider = strings.ToLower(strings.TrimSpace(profile.Provider))
	profile.Model = strings.TrimSpace(profile.Model)
	if profile.APIStyle == "" {
		profile.APIStyle = APIStyleChatCompletions
	}
	if profile.CompletionLimitField == "" {
		profile.CompletionLimitField = CompletionLimitMaxTokens
	}
	if profile.ReasoningWireField == "" {
		profile.SupportsThinkingToggle = false
		profile.SupportsReasoningEffort = false
	}
	return profile
}
