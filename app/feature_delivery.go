package app

import (
	"context"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/codingagent"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/transport/featurehttp"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/config"
)

type featureDeliveryRuntime struct {
	service         *featuredelivery.Service
	api             *featurehttp.Handler
	implementations *featuredelivery.ImplementationManager
}

func (platform *Platform) initFeatureDelivery() error {
	if platform.platformDB == nil {
		log.Warnf("[feature-delivery] disabled (MySQL unavailable)")
		return nil
	}
	deliveryStore := store.NewFeatureDeliveryStore(platform.platformDB)
	if err := deliveryStore.InterruptGenerationRuns(context.Background()); err != nil {
		return fmt.Errorf("interrupt unfinished feature generation: %w", err)
	}

	var generator *featuredelivery.Generator
	if platform.settings.LLMEnabled() {
		generationMaxTokens := featureGenerationTokenBudget(platform.settings)
		client := llm.NewLLMClientWithHTTPAndProvider(
			platform.settings.LLMBaseURL,
			platform.settings.LLMAPIKey,
			platform.settings.LLMModel,
			platform.settings.LLMProvider,
			generationMaxTokens,
			nil,
		)
		generator = featuredelivery.NewGenerator(
			platform.knowledge,
			client,
			platform.settings.LLMProvider,
			platform.settings.LLMModel,
			generationMaxTokens,
		)
	} else {
		log.Warnf("[feature-delivery] artifact generation disabled (LLM unavailable)")
	}

	service := featuredelivery.NewService(
		deliveryStore,
		generator,
		time.Duration(platform.settings.FeatureGenerationTimeout),
	)
	platform.featureDelivery.service = service
	platform.featureDelivery.api = featurehttp.New(service)
	platform.configureFeatureImplementation(deliveryStore, service)
	log.Infof("[feature-delivery] persistence enabled")
	return nil
}

func featureGenerationTokenBudget(settings *config.PlatformSettings) int {
	if settings == nil {
		return 0
	}
	// Structured artifacts must finish in one response, so reserve the largest configured answer budget.
	return max(settings.LLMMaxTokens, settings.LLMAnswerMaxTokens, settings.LLMConclusionMaxTokens)
}

func (platform *Platform) configureFeatureImplementation(deliveryStore featuredelivery.Store, service *featuredelivery.Service) {
	if len(platform.settings.CodingEnabledProviders) == 0 {
		log.Warnf("[feature-delivery] coding disabled (no provider configured)")
		return
	}
	if err := platform.settings.ValidateCodingSettings(); err != nil {
		log.Warnf("[feature-delivery] coding disabled: %v", err)
		return
	}
	workspaces, err := featuredelivery.NewWorkspaceManager(deliveryStore, platform.cfg.CodingWorkRoot)
	if err != nil {
		log.Warnf("[feature-delivery] coding disabled (workspace unavailable): %v", err)
		return
	}
	gitManager, err := featuredelivery.NewGitManager(platform.cfg.WorkspaceRoot, platform.cfg.CodingWorkRoot, workspaces)
	if err != nil {
		log.Warnf("[feature-delivery] coding disabled (Git unavailable): %v", err)
		return
	}
	runner := codingagent.New(codingagent.Config{
		CodexBin:         platform.cfg.CodexBin,
		ClaudeBin:        platform.cfg.ClaudeBin,
		EnabledProviders: platform.settings.CodingEnabledProviders,
	})
	manager := featuredelivery.NewImplementationManager(
		deliveryStore,
		workspaces,
		gitManager,
		runner,
		featuredelivery.ImplementationConfig{
			Timeout:          time.Duration(platform.settings.CodingTimeout),
			WorktreeTTL:      time.Duration(platform.settings.CodingWorktreeTTL),
			MaxConcurrency:   platform.settings.CodingMaxConcurrency,
			AllowNetwork:     platform.settings.CodingAllowNetwork,
			EnabledProviders: platform.settings.CodingEnabledProviders,
			DefaultModels: map[string]string{
				"codex":  platform.settings.CodingCodexModel,
				"claude": platform.settings.CodingClaudeModel,
			},
		},
	)
	service.SetImplementationManager(manager)
	platform.featureDelivery.implementations = manager
	log.Infof(
		"[feature-delivery] coding configured deployment=single_instance isolation=local_process concurrency=%d network_allowed=%t providers=%v",
		platform.settings.CodingMaxConcurrency,
		platform.settings.CodingAllowNetwork,
		platform.settings.CodingEnabledProviders,
	)
}

func (runtime *featureDeliveryRuntime) start(ctx context.Context) {
	if runtime.implementations != nil {
		runtime.implementations.Run(ctx)
	}
}

func (runtime *featureDeliveryRuntime) status(ctx context.Context) featuredelivery.FeatureDeliveryStatus {
	if runtime.service == nil {
		return featuredelivery.FeatureDeliveryStatus{
			Coding: featuredelivery.CodingCapabilityStatus{
				Providers: map[string]featuredelivery.CodingProviderStatus{},
			},
		}
	}
	return runtime.service.Status(ctx)
}
