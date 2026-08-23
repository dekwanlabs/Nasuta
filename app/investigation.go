package app

import (
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	"github.com/dekwanlabs/nasuta/platform/config"
	"github.com/dekwanlabs/nasuta/tool"
)

// buildInvestigationCoordinator assembles the isolated  investigation
// coordinator from the live schema, catalog, tool, and agent runtime objects.
// It returns a coordinator without mutating platform state so callers can wire
// it into the QA entrypoint once P1-05 switches the transport.
func (p *Platform) buildInvestigationCoordinator(
	settings *config.PlatformSettings,
	sharedStore ...investigation.RunStore,
) (*investigation.Coordinator, error) {
	if settings == nil {
		return nil, fmt.Errorf("investigation: platform settings are required")
	}
	if p.agents.schemas == nil {
		return nil, fmt.Errorf("investigation: schema registry is required")
	}
	if p.agents.catalog == nil {
		return nil, fmt.Errorf("investigation: agent definition catalog is required")
	}
	if p.agents.runtime == nil {
		return nil, fmt.Errorf("investigation: agent runtime is required")
	}
	if p.registry == nil {
		return nil, fmt.Errorf("investigation: tool registry is required")
	}

	catalog := investigation.NewTaskTemplateCatalog()
	if err := investigation.RegisterDefaultInvestigationTemplates(catalog); err != nil {
		return nil, fmt.Errorf("investigation: register task templates: %w", err)
	}
	if p.investigationTemplateProvider != nil {
		applicationTemplates, err := p.investigationTemplateProvider.InvestigationTemplates()
		if err != nil {
			return nil, fmt.Errorf("investigation: application templates: %w", err)
		}
		for _, template := range applicationTemplates {
			if err := catalog.Register(internalInvestigationTemplate(template)); err != nil {
				return nil, fmt.Errorf(
					"investigation: register application template %q: %w",
					template.ID,
					err,
				)
			}
		}
	}

	store := investigation.RunStore(investigation.NewMemoryRunStore())
	var leaseStore investigation.LeaseStore
	if len(sharedStore) > 0 && sharedStore[0] != nil {
		store = sharedStore[0]
	} else if p.db != nil {
		mysqlStore, err := investigation.NewMySQLRunStore(p.db)
		if err != nil {
			return nil, fmt.Errorf("investigation: configure mysql run store: %w", err)
		}
		store = mysqlStore
		leaseStore, err = investigation.NewMySQLLeaseStore(p.db)
		if err != nil {
			return nil, fmt.Errorf("investigation: configure mysql lease store: %w", err)
		}
	}

	snapshot := p.registry.Snapshot(tool.ReadPolicy())
	toolExecutor := tool.NewExecutor(time.Duration(settings.AgentTimeout))
	agentExecutor := investigation.AgentRuntimeTaskExecutor{
		Runtime:     p.agents.runtime,
		Definitions: p.agents.catalog,
	}
	executors := investigation.NewExecutorRegistry(map[investigation.ExecutorType]investigation.TaskExecutor{
		investigation.ExecutorDirectTool:   investigation.DirectToolExecutor{Executor: toolExecutor, Snapshot: snapshot},
		investigation.ExecutorToolPipeline: investigation.ToolPipelineExecutor{Executor: toolExecutor, Snapshot: snapshot},
		investigation.ExecutorInvestigator: agentExecutor,
		investigation.ExecutorVerifier:     agentExecutor,
		investigation.ExecutorComposer:     agentExecutor,
	})

	composer := investigation.AgentComposer{
		Runtime:     p.agents.runtime,
		Definitions: p.agents.catalog,
	}

	policy, err := investigation.BudgetPolicyFromPlatformSettings(*settings)
	if err != nil {
		return nil, fmt.Errorf("investigation: %w", err)
	}

	coordinator := investigation.NewCoordinator(investigation.CoordinatorOptions{
		Catalog:     catalog,
		Schemas:     p.agents.schemas,
		Tools:       snapshot,
		Store:       store,
		Executors:   executors,
		Composer:    composer,
		Delivery:    investigation.DeliveryGate{},
		Lease:       leaseStore,
		BudgetLimit: policy.Limit,
		CompositionBudget: investigation.BudgetVector{
			OutputTokens: int64(settings.LLMAnswerMaxTokens),
		},
		BudgetProfile:       policy.Profile,
		PolicyVersion:       investigation.DefaultBudgetPolicyVersion,
		MaxRounds:           policy.MaxRounds,
		MaxTasks:            settings.InvestigationMaxTasks,
		MaxParallelism:      settings.InvestigationMaxParallelism,
		MaxAgentParallelism: settings.InvestigationMaxParallelism,
		MaxToolParallelism:  settings.InvestigationMaxParallelism,
	})
	return coordinator, nil
}

func internalInvestigationTemplate(template InvestigationTemplate) investigation.TaskTemplate {
	toolCalls := make([]investigation.ToolCallSpec, len(template.ToolCalls))
	for index, call := range template.ToolCalls {
		toolCalls[index] = investigation.ToolCallSpec{
			ToolID: call.ToolID,
			Args:   call.Args,
		}
	}
	return investigation.TaskTemplate{
		ID:             template.ID,
		Version:        template.Version,
		GoalKinds:      append([]string(nil), template.GoalKinds...),
		RequiredInputs: append([]string(nil), template.RequiredInputs...),
		Provides:       append([]string(nil), template.Provides...),
		ToolGrant:      append([]tool.ToolID(nil), template.ToolGrant...),
		InputSchema:    template.InputSchema,
		OutputSchema:   template.OutputSchema,
		Executor:       investigation.ExecutorType(template.Executor),
		ToolCalls:      toolCalls,
		CostProfile: investigation.BudgetVector{
			InputTokens:  template.CostProfile.InputTokens,
			OutputTokens: template.CostProfile.OutputTokens,
			ToolCalls:    template.CostProfile.ToolCalls,
			Duration:     template.CostProfile.Duration,
			CostMicros:   template.CostProfile.CostMicros,
		},
		MaxAttempts: template.MaxAttempts,
		Enabled:     template.Enabled,
	}
}
