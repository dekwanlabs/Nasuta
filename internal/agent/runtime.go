package agent

import (
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/log"
)

// QARuntime owns the reusable execution mechanism used by the QA scenario.
type QARuntime struct {
	llm            *llm.LLMClient
	fastLLM        *llm.LLMClient
	agent          *Agent
	registry       *Registry
	executor       *ToolExecutor
	writeAvailable bool
	hub            *RunHub
	runStore       *RunStore
}

type QARuntimeDeps struct {
	Tools          *Service
	Registry       *Registry
	Cfg            config.Config
	Platform       *config.PlatformSettings
	Sessions       *memory.SessionStore
	History        SessionHistory
	WriteAvailable bool
	RunStore       *RunStore
}

// NewQARuntime pins model, tool, budget, and observation dependencies.
func NewQARuntime(deps QARuntimeDeps) *QARuntime {
	settings := deps.Platform
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

	registry := deps.Registry
	if registry == nil {
		registry = NewRegistry(deps.Tools, deps.Cfg, deps.Sessions, deps.History)
	}
	runHub := NewRunHub(deps.RunStore)
	executor := NewToolExecutor(registry)
	loop := NewAgent(client, executor, AgentConfig{
		Timeout:             time.Duration(settings.AgentTimeout),
		MaxSteps:            settings.AgentMaxSteps,
		AnswerReserve:       time.Duration(settings.AgentAnswerReserve),
		AnswerMaxTokens:     settings.LLMAnswerMaxTokens,
		ConclusionMaxTokens: settings.LLMConclusionMaxTokens,
		ContextWindow:       settings.LLMContextWindow,
		MaxContinueRounds:   settings.LLMMaxContinueRounds,
		DomainKnowledge:     settings.DomainKnowledge,
		HistoryLimit:        0,
	}, runHub, runHub)
	loop.SetOnFirstAnswerToken(func(runID string) {
		runHub.EmitPhase(runID, "找到啦，我来把答案写出来 ✍️")
	})
	return &QARuntime{
		llm: client, fastLLM: fastClient, agent: loop,
		registry: registry, executor: executor,
		writeAvailable: deps.WriteAvailable,
		hub:            runHub, runStore: deps.RunStore,
	}
}
