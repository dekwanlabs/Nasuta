package app

import (
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/config"
)

func (p *Platform) buildQARuntime(
	settings *config.PlatformSettings,
	graph *codegraph.DB,
	version int64,
) (dashboard.QARuntime, []agentapi.Definition, agentapi.Runtime, error) {
	snapshot := *settings
	snapshot.VCSGroups = append([]string(nil), settings.VCSGroups...)
	snapshot.VCSExcludeProjects = append([]string(nil), settings.VCSExcludeProjects...)
	snapshot.CodingEnabledProviders = append([]string(nil), settings.CodingEnabledProviders...)
	writeAvailable := p.incident.manager != nil
	investigationRunner := platformQAInvestigationRunner{platform: p}
	if !snapshot.LLMEnabled() {
		scenarios := agent.NewScenarioRuntime(p.qa.runs)
		runtime := dashboard.QARuntime{
			Hub: scenarios.Hub(), RunStore: p.qa.runs,
			Sessions: p.qa.sessions,
			History:  p.history, Settings: &snapshot,
			WriteAvailable: writeAvailable,
		}
		if p.qa.runs != nil && p.flow.service != nil {
			coordinator := agent.NewInvestigationCoordinator(
				investigationRunner,
				scenarios,
				p.qa.runs,
				p.qa.sessions,
			)
			runtime.InvestigationCanceller = coordinator
			runtime.InvestigationReconciler = coordinator
		}
		return runtime, nil, nil, nil
	}
	if err := snapshot.ValidateAgentSettings(); err != nil {
		return dashboard.QARuntime{}, nil, nil, err
	}
	if p.qa.memory == nil {
		p.qa.memory = buildLongTermMemory(
			p.cfg,
			p.db,
			p.index.Embedder,
		)
	}
	definitions, err := defaultAgentDefinitions(&snapshot, version)
	if err != nil {
		return dashboard.QARuntime{}, nil, nil, err
	}
	definitionRuntime, err := agent.NewDefinitionRuntime(
		p.agents.catalog,
		p.agents.schemas,
		p.registry,
		&snapshot,
		p.qa.runs,
	)
	if err != nil {
		return dashboard.QARuntime{}, nil, nil, fmt.Errorf("configure definition runtime: %w", err)
	}
	models := agent.NewQAModels(&snapshot)
	investigationRunner.events = definitionRuntime
	var coordinator *agent.InvestigationCoordinator
	if p.qa.runs != nil && p.flow.service != nil {
		coordinator = agent.NewInvestigationCoordinator(
			investigationRunner,
			definitionRuntime,
			p.qa.runs,
			p.qa.sessions,
		)
	}
	qa := agent.NewQA(agent.QADeps{
		Tools: p.tools, Cfg: p.cfg, Platform: &snapshot,
		CodeGraphDB: graph, History: p.history,
		Sessions: p.qa.sessions, Memory: p.qa.memory,
		Definitions:       p.agents.catalog,
		Agent:             agentapi.DefinitionRef{ID: definitions[0].ID},
		Runtime:           definitionRuntime,
		RuntimeTools:      definitionRuntime,
		Models:            models,
		PhaseEmitter:      definitionRuntime,
		Investigation:     investigationRunner,
		ScenarioLifecycle: definitionRuntime,
		Coordinator:       coordinator,
		ExecutionEvents:   definitionRuntime,
		WriteAvailable:    writeAvailable,
	})
	runtime := dashboard.QARuntime{
		QA: qa, RunStore: p.qa.runs,
		Sessions: p.qa.sessions,
		History:  p.history, Settings: &snapshot,
		WriteAvailable: writeAvailable, Hub: definitionRuntime.Hub(),
		CompactionLLM: models.Primary(),
	}
	if coordinator != nil {
		runtime.InvestigationCanceller = coordinator
		runtime.InvestigationReconciler = coordinator
	}
	return runtime, definitions, definitionRuntime, nil
}

func defaultAgentDefinitions(
	settings *config.PlatformSettings,
	version int64,
) ([]agentapi.Definition, error) {
	qa, err := catalog.DefaultQAVersion(settings, version)
	if err != nil {
		return nil, fmt.Errorf("prepare QA definition: %w", err)
	}
	reviewers, err := catalog.DefaultReviewersVersion(settings, version)
	if err != nil {
		return nil, fmt.Errorf("prepare reviewer definitions: %w", err)
	}
	investigators, err := catalog.DefaultInvestigatorsVersion(settings, version)
	if err != nil {
		return nil, fmt.Errorf("prepare investigation definitions: %w", err)
	}
	definitions := make([]agentapi.Definition, 0, 1+len(reviewers)+len(investigators))
	definitions = append(definitions, qa)
	definitions = append(definitions, reviewers...)
	definitions = append(definitions, investigators...)
	return definitions, nil
}

func (p *Platform) currentQARuntime() dashboard.QARuntime {
	p.qa.mu.RLock()
	defer p.qa.mu.RUnlock()
	return p.qa.current
}

func (p *Platform) reloadQARuntime(graph *codegraph.DB) error {
	p.qa.reload.Lock()
	defer p.qa.reload.Unlock()

	settings := loadPlatformSettings(p.auth.db)
	version := p.agents.version + 1
	candidate, definitions, definitionRuntime, err := p.buildQARuntime(settings, graph, version)
	if err != nil {
		return err
	}
	if len(definitions) > 0 {
		if err := p.agents.catalog.Publish(definitions); err != nil {
			return fmt.Errorf("publish agent definitions: %w", err)
		}
		capabilities, err := catalog.DefaultInvestigationCapabilities(
			definitions,
			version,
		)
		if err != nil {
			return fmt.Errorf("prepare investigation capabilities: %w", err)
		}
		if err := p.agents.capabilities.Publish(capabilities); err != nil {
			return fmt.Errorf("publish investigation capabilities: %w", err)
		}
		investigationBudgets, err := delegatedInvestigationBudgetPolicy(definitions)
		if err != nil {
			return fmt.Errorf("prepare delegated investigation budgets: %w", err)
		}
		workflowDefinition, err := p.delegatedInvestigationWorkflow(
			version,
			time.Duration(candidate.Settings.AgentTimeout),
			investigationBudgets,
		)
		if err != nil {
			return fmt.Errorf("prepare delegated investigation workflow: %w", err)
		}
		if err := p.flow.catalog.Publish([]workflow.WorkflowDefinition{workflowDefinition}); err != nil {
			return fmt.Errorf("publish delegated investigation workflow: %w", err)
		}
	}
	if err := p.configureFeatureReviewRuntime(candidate.Settings, definitionRuntime, definitions); err != nil {
		return err
	}
	if err := p.configureAgentWorkflowRuntime(definitionRuntime); err != nil {
		return err
	}

	p.qa.mu.Lock()
	p.settings = candidate.Settings
	p.qa.current = candidate
	p.agents.runtime = definitionRuntime
	if len(definitions) > 0 {
		p.agents.version = version
	}
	p.graph = graph
	p.qa.mu.Unlock()
	p.index.SetPlatform(candidate.Settings)
	log.Infof("[settings] agent runtimes reloaded (definition_version=%d, model=%s, timeout=%s, max_steps=%d)",
		p.agents.version, candidate.Settings.LLMModel,
		time.Duration(candidate.Settings.AgentTimeout), candidate.Settings.AgentMaxSteps)
	return nil
}

func (p *Platform) delegatedInvestigationWorkflow(
	version int64,
	nodeTimeout time.Duration,
	budgets workflow.DelegatedInvestigationBudgetPolicy,
) (workflow.WorkflowDefinition, error) {
	fallback, err := workflow.DefaultDelegatedInvestigation(
		version,
		nodeTimeout,
		budgets,
	)
	if err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	policy, err := workflow.DelegatedInvestigationCompilationPolicy(
		version,
		nodeTimeout,
		budgets,
	)
	if err != nil {
		log.Warnf(
			"[workflow] delegated investigation proposal policy unavailable; using fixed fallback: %v",
			err,
		)
		return fallback, nil
	}
	compiler, err := workflow.NewProposalCompiler(
		p.agents.schemas,
		p.agents.capabilities,
	)
	if err != nil {
		log.Warnf(
			"[workflow] delegated investigation proposal compiler unavailable; using fixed fallback: %v",
			err,
		)
		return fallback, nil
	}
	compiled, err := compiler.Compile(
		workflow.DefaultDelegatedInvestigationProposal(),
		policy,
	)
	if err != nil {
		log.Warnf(
			"[workflow] delegated investigation proposal rejected; using fixed fallback: %v",
			err,
		)
		return fallback, nil
	}
	return compiled, nil
}

func (p *Platform) configureAgentWorkflowRuntime(runtime agentapi.Runtime) error {
	if p.flow.service == nil {
		return nil
	}
	transforms := newWorkflowTransformDispatcher(
		p.flow.pipeline,
		p.flow.review,
	)
	var agentNodes *workflow.AgentNodeExecutor
	if runtime != nil {
		nodes, err := workflow.NewAgentNodeExecutor(
			p.agents.schemas,
			p.agents.catalog,
			runtime,
		)
		if err != nil {
			return fmt.Errorf("configure workflow agent nodes: %w", err)
		}
		agentNodes = nodes
	}
	if agentNodes == nil && transforms == nil {
		p.flow.service.SetOrchestrator(nil)
		log.Warnf("[workflow] execution disabled (agent and transform executors unavailable)")
		return nil
	}
	nodes := workflow.NewNodeDispatcher(agentNodes, transforms)
	runner := workflow.NewOrchestrator(p.agents.schemas, nodes, nil)
	p.flow.service.SetOrchestrator(runner)
	switch {
	case agentNodes != nil && transforms != nil:
		log.Infof("[workflow] agent and feature transform execution enabled")
	case agentNodes != nil:
		log.Infof("[workflow] agent execution enabled")
	default:
		log.Infof("[workflow] feature transform execution enabled (LLM unavailable)")
	}
	return nil
}
