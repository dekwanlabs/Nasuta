package app

import (
	"context"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/codingagent"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	"github.com/dekwanlabs/nasuta/internal/feature/pipeline"
	"github.com/dekwanlabs/nasuta/internal/feature/reviewworkflow"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/transport/featurehttp"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/config"
)

type featureDeliveryRuntime struct {
	service         *delivery.Service
	api             *featurehttp.Handler
	implementations *delivery.ImplementationManager
	codingReason    string
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

	var generator *delivery.Generator
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
		generator = delivery.NewGenerator(
			platform.knowledge,
			client,
			platform.settings.LLMProvider,
			platform.settings.LLMModel,
			generationMaxTokens,
		)
	} else {
		log.Warnf("[feature-delivery] artifact generation disabled (LLM unavailable)")
	}

	service := delivery.NewService(
		deliveryStore,
		generator,
		time.Duration(platform.settings.FeatureGenerationTimeout),
	)
	pipelineDefinition, err := pipeline.DefaultDefinition(pipeline.WorkflowVersion)
	if err != nil {
		return fmt.Errorf("prepare feature pipeline workflow: %w", err)
	}
	if err := platform.workflowCatalog.Publish([]workflow.WorkflowDefinition{pipelineDefinition}); err != nil {
		return fmt.Errorf("publish feature pipeline workflow: %w", err)
	}
	platform.featureDelivery.service = service
	platform.featureDelivery.api = featurehttp.New(service)
	platform.featureDelivery.api.SetPipelineStarter(
		pipeline.NewStarter(platform.workflowService),
	)
	approvals := pipeline.NewApprovalCoordinator(service, platform.workflowService)
	platform.featureDelivery.api.SetArtifactReviewer(approvals)
	if platform.workflowAPI != nil {
		platform.workflowAPI.SetApprovalDecider(approvals)
	}
	settings, runtime, definitions, err := platform.currentFeatureReviewRuntime()
	if err != nil {
		return err
	}
	if err := platform.configureFeatureReviewRuntime(
		settings,
		runtime,
		definitions,
	); err != nil {
		return err
	}
	platform.configureFeatureImplementation(deliveryStore, service)
	platform.workflowPipeline = pipeline.NewExecutor(service)
	if err := platform.configureAgentWorkflowRuntime(runtime); err != nil {
		return err
	}
	log.Infof("[feature-delivery] persistence enabled")
	return nil
}

func (platform *Platform) currentFeatureReviewRuntime() (
	*config.PlatformSettings,
	agentapi.Runtime,
	[]agentapi.Definition,
	error,
) {
	platform.qaMu.RLock()
	if platform.settings == nil {
		platform.qaMu.RUnlock()
		return nil, nil, nil, nil
	}
	settings := *platform.settings
	runtime := platform.definitionRuntime
	version := platform.agentDefinitionVer
	catalog := platform.agentCatalog
	platform.qaMu.RUnlock()

	if !settings.LLMEnabled() {
		return &settings, nil, nil, nil
	}
	if runtime == nil || version <= 0 || catalog == nil {
		return nil, nil, nil, fmt.Errorf("configure agent review runtime: agent definitions are not published")
	}
	definitions, err := defaultAgentDefinitions(&settings, version)
	if err != nil {
		return nil, nil, nil, err
	}
	for index, definition := range definitions {
		published, err := catalog.Resolve(agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf("resolve published reviewer definition %q: %w", definition.ID, err)
		}
		if published.ContentHash != definition.ContentHash {
			return nil, nil, nil, fmt.Errorf(
				"published reviewer definition %q version %d does not match active settings",
				definition.ID,
				definition.Version,
			)
		}
		definitions[index] = published
	}
	return &settings, runtime, definitions, nil
}

func (platform *Platform) configureFeatureReviewRuntime(
	settings *config.PlatformSettings,
	runtime agentapi.Runtime,
	definitions []agentapi.Definition,
) error {
	service := platform.featureDelivery.service
	if service == nil {
		return nil
	}
	if settings == nil || !settings.LLMEnabled() {
		service.SetReviewConfiguration(nil, nil)
		service.SetAdjudicationRunner(nil)
		platform.workflowReview = nil
		platform.reviewCoordinator = nil
		if platform.featureDelivery.api != nil {
			platform.featureDelivery.api.SetReviewCoordinator(nil)
		}
		log.Warnf("[feature-delivery] agent review disabled (LLM unavailable)")
		return nil
	}
	if runtime == nil {
		return fmt.Errorf("configure agent review runtime: configured LLM has no definition runtime")
	}
	policies, defaults, err := service.InstallDefaultReviewPolicySet(
		context.Background(),
		definitions,
	)
	if err != nil {
		return fmt.Errorf("publish default review policies: %w", err)
	}
	if platform.workflowService == nil {
		return fmt.Errorf("configure agent review runtime: workflow service is unavailable")
	}
	workflowDefinitions := make([]workflow.WorkflowDefinition, 0, len(policies))
	for _, policy := range policies {
		definition, err := reviewworkflow.Definition(policy)
		if err != nil {
			return fmt.Errorf("prepare review workflow for policy %q: %w", policy.ID, err)
		}
		workflowDefinitions = append(workflowDefinitions, definition)
	}
	if err := platform.workflowService.PublishDefinitions(workflowDefinitions, true); err != nil {
		return fmt.Errorf("publish default review workflows: %w", err)
	}
	service.SetReviewConfiguration(delivery.NewRuntimeReviewRunner(runtime), defaults)
	service.SetAdjudicationRunner(delivery.NewRuntimeAdjudicationRunner(runtime))
	platform.workflowReview = reviewworkflow.NewExecutor(service)
	platform.reviewCoordinator = reviewworkflow.NewCoordinator(
		service,
		platform.workflowService,
	)
	if platform.featureDelivery.api != nil {
		platform.featureDelivery.api.SetReviewCoordinator(platform.reviewCoordinator)
	}
	log.Infof(
		"[feature-delivery] agent review runtime enabled (default_policies=%d workflows=%d)",
		len(defaults),
		len(workflowDefinitions),
	)
	return nil
}

func featureGenerationTokenBudget(settings *config.PlatformSettings) int {
	if settings == nil {
		return 0
	}
	// Structured artifacts must finish in one response, so reserve the largest configured answer budget.
	return max(settings.LLMMaxTokens, settings.LLMAnswerMaxTokens, settings.LLMConclusionMaxTokens)
}

func (platform *Platform) configureFeatureImplementation(deliveryStore delivery.Store, service *delivery.Service) {
	if len(platform.settings.CodingEnabledProviders) == 0 {
		platform.featureDelivery.codingReason = "not_configured"
		log.Warnf("[feature-delivery] coding disabled (no provider configured)")
		return
	}
	if err := platform.settings.ValidateCodingSettings(); err != nil {
		platform.featureDelivery.codingReason = "invalid_configuration"
		log.Warnf("[feature-delivery] coding disabled: %v", err)
		return
	}
	workspaces, err := delivery.NewWorkspaceManager(deliveryStore, platform.cfg.CodingWorkRoot)
	if err != nil {
		platform.featureDelivery.codingReason = "workspace_unavailable"
		log.Warnf("[feature-delivery] coding disabled (workspace unavailable): %v", err)
		return
	}
	gitManager, err := delivery.NewGitManager(platform.cfg.WorkspaceRoot, platform.cfg.CodingWorkRoot, workspaces)
	if err != nil {
		platform.featureDelivery.codingReason = "git_unavailable"
		log.Warnf("[feature-delivery] coding disabled (Git unavailable): %v", err)
		return
	}
	runner := codingagent.New(codingagent.Config{
		CodexBin:         platform.cfg.CodexBin,
		ClaudeBin:        platform.cfg.ClaudeBin,
		EnabledProviders: platform.settings.CodingEnabledProviders,
	})
	manager := delivery.NewImplementationManager(
		deliveryStore,
		workspaces,
		gitManager,
		runner,
		delivery.ImplementationConfig{
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
	platform.featureDelivery.codingReason = ""
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

func (runtime *featureDeliveryRuntime) status(ctx context.Context) delivery.FeatureDeliveryStatus {
	if runtime.service == nil {
		return delivery.FeatureDeliveryStatus{
			Coding: delivery.CodingCapabilityStatus{
				Reason:    "persistence_unavailable",
				Providers: map[string]delivery.CodingProviderStatus{},
			},
		}
	}
	status := runtime.service.Status(ctx)
	if status.Coding.Enabled || status.Coding.Reason != "" {
		return status
	}
	status.Coding.Reason = runtime.codingReason
	return status
}
