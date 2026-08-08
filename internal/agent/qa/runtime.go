package qa

import (
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/log"
)

// QAModels owns model clients used by QA preparation and session maintenance.
type QAModels struct {
	primary *llm.LLMClient
	fast    *llm.LLMClient
}

func (models *QAModels) Primary() *llm.LLMClient {
	if models == nil {
		return nil
	}
	return models.primary
}

func (models *QAModels) Fast() *llm.LLMClient {
	if models == nil {
		return nil
	}
	return models.fast
}

// NewQAModels pins helper-model choices used outside the execution loop.
func NewQAModels(settings *config.PlatformSettings) *QAModels {
	client := llm.NewLLMClientWithHTTPAndProvider(
		settings.LLMBaseURL,
		settings.LLMAPIKey,
		settings.LLMModel,
		settings.LLMProvider,
		settings.LLMMaxTokens,
		nil,
	)
	fastClient := client
	if settings.FastLLMConfigured() {
		fastProvider := settings.LLMProvider
		if fastProvider == "" {
			fastProvider = "openai"
		}
		fastClient = llm.NewLLMClientWithHTTPAndProvider(
			settings.LLMBaseURL,
			settings.LLMAPIKey,
			settings.LLMFastModel,
			fastProvider,
			settings.LLMMaxTokens,
			nil,
		)
		log.Infof("[qa] fast model enabled for preprocess/queryterms: %s @ %s (%s)",
			settings.LLMFastModel, settings.LLMBaseURL, fastProvider)
	}
	return &QAModels{primary: client, fast: fastClient}
}
