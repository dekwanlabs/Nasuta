package catalog

import (
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

func DefaultQA(settings *config.PlatformSettings) (agentapi.Definition, error) {
	return DefaultQAVersion(settings, 1)
}

func DefaultQAVersion(settings *config.PlatformSettings, version int64) (agentapi.Definition, error) {
	systemPrompt := settings.DomainKnowledge
	if systemPrompt == "" {
		systemPrompt = prompts.Text(prompts.AgentCatalogFallbackQA)
	}
	return agentapi.Prepare(agentapi.Definition{
		ID: "qa.answerer", Version: version, DisplayName: "QA Answerer",
		Purpose: "Answer questions using bounded, attributable evidence.",
		Prompt: agentapi.PromptSpec{
			System:  systemPrompt,
			Version: "qa-loop-v1",
		},
		InputSchema:  agentapi.SchemaRef{ID: "qa.request", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "qa.answer", Version: 1},
		Model: agentapi.ModelPolicy{
			Provider: settings.LLMProvider, Model: settings.LLMModel,
			MaxOutputTokens: settings.LLMAnswerMaxTokens,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout:       time.Duration(settings.AgentTimeout),
			MaxSteps:      settings.AgentMaxSteps,
			ContextTokens: settings.LLMContextWindow,
		},
		Tools:       agentapi.ToolPolicy{AllowWrite: true},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read", "knowledge.write"}},
	})
}
