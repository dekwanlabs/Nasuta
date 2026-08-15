package qa

import (
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/log"
)

// Models owns model clients used by QA preparation and session maintenance.
type Models struct {
	primary *llm.LLMClient
	fast    *llm.LLMClient
}

func (models *Models) Primary() *llm.LLMClient {
	if models == nil {
		return nil
	}
	return models.primary
}

func (models *Models) Fast() *llm.LLMClient {
	if models == nil {
		return nil
	}
	return models.fast
}

// NewModels pins helper-model choices used outside the execution loop.
func NewModels(settings *config.PlatformSettings) *Models {
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
	return &Models{primary: client, fast: fastClient}
}
