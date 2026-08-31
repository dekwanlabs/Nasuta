package app

import (
	"time"

	"github.com/dekwanlabs/nasuta/config"
)

func enabledAgentSettings() *config.PlatformSettings {
	settings := &config.PlatformSettings{
		LLMBaseURL: "http://llm.invalid", LLMAPIKey: "key",
		LLMProvider: "openai", LLMModel: "model",
		LLMMaxTokens: 2048, LLMAnswerMaxTokens: 2048,
		LLMConclusionMaxTokens: 2048, LLMContextWindow: 32000,
		AgentTimeout: config.Duration(2 * time.Minute), AgentMaxSteps: 4,
		AgentAnswerReserve: config.Duration(30 * time.Second),
	}
	settings.Apply(nil)
	return settings
}
