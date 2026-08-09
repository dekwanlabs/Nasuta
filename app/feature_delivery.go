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

func (p *Platform) initFeatureDelivery() error {
	if p.db == nil {
		log.Warnf("[feature-delivery] disabled (MySQL unavailable)")
		return nil
	}
	deliveryStore := store.NewFeatureDeliveryStore(p.db)
	if err := deliveryStore.InterruptGenerationRuns(context.Background()); err != nil {
		return fmt.Errorf("interrupt unfinished feature generation: %w", err)
	}

	var generator *delivery.Generator
	if p.settings.LLMEnabled() {
		generationMaxTokens := featureGenerationTokenBudget(p.settings)
		client := llm.NewLLMClientWithHTTPAndProvider(
			p.settings.LLMBaseURL,
			p.settings.LLMAPIKey,
			p.settings.LLMModel,
			p.settings.LLMProvider,
			generationMaxTokens,
			nil,
		)
		generator = delivery.NewGenerator(
			p.tools,
			client,
			p.settings.LLMProvider,
			p.settings.LLMModel,
			generationMaxTokens,
		)
	} else {
		log.Warnf("[feature-delivery] artifact generation disabled (LLM unavailable)")
	}

	service := delivery.NewService(
		deliveryStore,
		generator,
		time.Duration(p.settings.FeatureGenerationTimeout),
	)
	pipelineDefinition, err := pipeline.DefaultDefinition(pipeline.WorkflowVersion)
	if err != nil {
		return fmt.Errorf("prepare feature pipeline workflow: %w", err)
	}
	if err := p.flow.catalog.Publish([]workflow.WorkflowDefinition{pipelineDefinition}); err != nil {
		return fmt.Errorf("publish feature pipeline workflow: %w", err)
	}
	p.delivery.service = service
	p.delivery.api = featurehttp.New(service)
	p.delivery.api.SetPipelineStarter(
		pipeline.NewStarter(p.flow.service),
	)
	approvals := pipeline.NewApprovalCoordinator(service, p.flow.service)
	p.delivery.api.SetArtifactReviewer(approvals)
	if p.flow.api != nil {
		p.flow.api.SetApprovalDecider(approvals)
	}
	settings, runtime, definitions, err := p.currentFeatureReviewRuntime()
	if err != nil {
		return err
	}
	if err := p.configureFeatureReviewRuntime(
		settings,
		runtime,
		definitions,
	); err != nil {
		return err
	}
	p.configureFeatureImplementation(deliveryStore, service)
	p.flow.pipeline = pipeline.NewExecutor(service)
	if err := p.configureAgentWorkflowRuntime(runtime); err != nil {
		return err
	}
	log.Infof("[feature-delivery] persistence enabled")
	return nil
}

func (p *Platform) currentFeatureReviewRuntime() (
	*config.PlatformSettings,
	agentapi.Runtime,
	[]agentapi.Definition,
	error,
) {
	p.qa.mu.RLock()
	if p.settings == nil {
		p.qa.mu.RUnlock()
		return nil, nil, nil, nil
	}
	settings := *p.settings
	runtime := p.agents.runtime
	version := p.agents.version
	catalog := p.agents.catalog
	p.qa.mu.RUnlock()

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

func (p *Platform) configureFeatureReviewRuntime(
	settings *config.PlatformSettings,
	runtime agentapi.Runtime,
	definitions []agentapi.Definition,
) error {
	service := p.delivery.service
	if service == nil {
		return nil
	}
	if settings == nil || !settings.LLMEnabled() {
		service.SetReviewConfiguration(nil, nil)
		service.SetAdjudicationRunner(nil)
		p.flow.review = nil
		p.flow.coordinator = nil
		if p.delivery.api != nil {
			p.delivery.api.SetReviewCoordinator(nil)
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
	if p.flow.service == nil {
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
	if err := p.flow.service.PublishDefinitions(workflowDefinitions, true); err != nil {
		return fmt.Errorf("publish default review workflows: %w", err)
	}
	service.SetReviewConfiguration(delivery.NewRuntimeReviewRunner(runtime), defaults)
	service.SetAdjudicationRunner(delivery.NewRuntimeAdjudicationRunner(runtime))
	p.flow.review = reviewworkflow.NewExecutor(service)
	p.flow.coordinator = reviewworkflow.NewCoordinator(
		service,
		p.flow.service,
	)
	if p.delivery.api != nil {
		p.delivery.api.SetReviewCoordinator(p.flow.coordinator)
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

func (p *Platform) configureFeatureImplementation(deliveryStore delivery.Store, service *delivery.Service) {
	if len(p.settings.CodingEnabledProviders) == 0 {
		p.delivery.codingReason = "not_configured"
		log.Warnf("[feature-delivery] coding disabled (no provider configured)")
		return
	}
	if err := p.settings.ValidateCodingSettings(); err != nil {
		p.delivery.codingReason = "invalid_configuration"
		log.Warnf("[feature-delivery] coding disabled: %v", err)
		return
	}
	workspaces, err := delivery.NewWorkspaceManager(deliveryStore, p.cfg.CodingWorkRoot)
	if err != nil {
		p.delivery.codingReason = "workspace_unavailable"
		log.Warnf("[feature-delivery] coding disabled (workspace unavailable): %v", err)
		return
	}
	gitManager, err := delivery.NewGitManager(p.cfg.WorkspaceRoot, p.cfg.CodingWorkRoot, workspaces)
	if err != nil {
		p.delivery.codingReason = "git_unavailable"
		log.Warnf("[feature-delivery] coding disabled (Git unavailable): %v", err)
		return
	}
	runner := codingagent.New(codingagent.Config{
		CodexBin:         p.cfg.CodexBin,
		ClaudeBin:        p.cfg.ClaudeBin,
		EnabledProviders: p.settings.CodingEnabledProviders,
	})
	manager := delivery.NewImplementationManager(
		deliveryStore,
		workspaces,
		gitManager,
		runner,
		delivery.ImplementationConfig{
			Timeout:          time.Duration(p.settings.CodingTimeout),
			WorktreeTTL:      time.Duration(p.settings.CodingWorktreeTTL),
			MaxConcurrency:   p.settings.CodingMaxConcurrency,
			AllowNetwork:     p.settings.CodingAllowNetwork,
			EnabledProviders: p.settings.CodingEnabledProviders,
			DefaultModels: map[string]string{
				"codex":  p.settings.CodingCodexModel,
				"claude": p.settings.CodingClaudeModel,
			},
		},
	)
	service.SetImplementationManager(manager)
	p.delivery.implementations = manager
	p.delivery.codingReason = ""
	log.Infof(
		"[feature-delivery] coding configured deployment=single_instance isolation=local_process concurrency=%d network_allowed=%t providers=%v",
		p.settings.CodingMaxConcurrency,
		p.settings.CodingAllowNetwork,
		p.settings.CodingEnabledProviders,
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
