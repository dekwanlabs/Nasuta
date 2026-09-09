package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/agent/definition"
	"github.com/dekwanlabs/nasuta/internal/agent/delegation"
	agentqa "github.com/dekwanlabs/nasuta/internal/agent/qa"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/config"
	"github.com/dekwanlabs/nasuta/tool"
)

const dynamicDelegationToolOwner = "nasuta.dynamic_delegation"

type qaCatalogSnapshot struct {
	version      int64
	definitions  []agentapi.Definition
	capabilities []agentapi.Capability
}

// buildQARuntime assembles a QA runtime candidate and the catalog artifacts
// derived from the supplied settings, graph, and catalog version. It does not
// publish catalog entries or replace the currently active runtime.
func (p *Platform) buildQARuntime(
	settings *config.PlatformSettings,
	graph *codegraph.DB,
	version int64,
) (
	qaRuntimeBundle,
	[]agentapi.Definition,
	[]agentapi.Capability,
	agentapi.Runtime,
	error,
) {
	snapshot := *settings
	snapshot.VCSGroups = append([]string(nil), settings.VCSGroups...)
	snapshot.VCSExcludeProjects = append([]string(nil), settings.VCSExcludeProjects...)
	snapshot.CodingEnabledProviders = append([]string(nil), settings.CodingEnabledProviders...)
	snapshot.DelegationCapabilities = append([]string(nil), settings.DelegationCapabilities...)
	writeAvailable := p.incident.manager != nil
	if !snapshot.LLMEnabled() {
		hub := run.NewHub(p.qa.runs)
		runtime := qaRuntimeBundle{
			Hub: hub, RunStore: p.qa.runs,
			Sessions: p.qa.sessions,
			History:  p.history, Settings: &snapshot,
			WriteAvailable: writeAvailable,
		}
		return runtime, nil, nil, nil, nil
	}
	if err := snapshot.ValidateAgentSettings(); err != nil {
		return qaRuntimeBundle{}, nil, nil, nil, err
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
		return qaRuntimeBundle{}, nil, nil, nil, err
	}
	var extensionCapabilities []agentapi.Capability
	if p.agents.provider != nil {
		contribution, err := p.agents.provider.AgentCatalog(snapshot, version)
		if err != nil {
			return qaRuntimeBundle{}, nil, nil, nil, fmt.Errorf(
				"prepare application agent catalog: %w",
				err,
			)
		}
		if err := validateAgentCatalogContribution(contribution, version); err != nil {
			return qaRuntimeBundle{}, nil, nil, nil, err
		}
		definitions = append(definitions, contribution.Definitions...)
		extensionCapabilities = append(
			extensionCapabilities,
			contribution.Capabilities...,
		)
	}
	hub := run.NewHub(p.qa.runs)
	definitionRuntime, err := definition.NewRuntime(
		p.agents.catalog,
		p.agents.schemas,
		p.registry,
		&snapshot,
		p.qa.runs,
		hub,
	)
	if err != nil {
		return qaRuntimeBundle{}, nil, nil, nil, fmt.Errorf(
			"configure definition runtime: %w",
			err,
		)
	}
	models := agentqa.NewModels(&snapshot)
	p.tools.SetWebQueryRewriter(webQueryRewriter(models.Fast()))
	qa := agentqa.New(agentqa.Deps{
		Tools: p.tools, Cfg: p.cfg, Platform: &snapshot,
		CodeGraphDB: graph, History: p.history,
		Sessions: p.qa.sessions, Memory: p.qa.memory,
		Definitions:    p.agents.catalog,
		Agent:          agentapi.DefinitionRef{ID: definitions[0].ID},
		Starter:        definitionRuntime,
		ScenarioTools:  definitionRuntime,
		Models:         models,
		Events:         hub,
		WriteAvailable: writeAvailable,
	})
	runtime := qaRuntimeBundle{
		QA: qa, RunStore: p.qa.runs,
		Sessions: p.qa.sessions,
		History:  p.history, Settings: &snapshot,
		WriteAvailable: writeAvailable, Hub: hub,
		CompactionLLM: models.Primary(),
	}
	return runtime, definitions, extensionCapabilities, definitionRuntime, nil
}

// defaultAgentDefinitions builds the built-in QA, reviewer, and investigator
// definitions for one catalog version.
func defaultAgentDefinitions(
	settings *config.PlatformSettings,
	version int64,
) ([]agentapi.Definition, error) {
	qa, err := catalog.DefaultQAVersion(settings, version)
	if err != nil {
		return nil, fmt.Errorf("prepare QA definition: %w", err)
	}
	reviewers, err := catalog.DefaultReviewers(settings, version)
	if err != nil {
		return nil, fmt.Errorf("prepare reviewer definitions: %w", err)
	}
	investigators, err := catalog.DefaultInvestigators(settings, version)
	if err != nil {
		return nil, fmt.Errorf("prepare delegation investigator definitions: %w", err)
	}
	definitions := make([]agentapi.Definition, 0, 1+len(reviewers)+len(investigators))
	definitions = append(definitions, qa)
	definitions = append(definitions, reviewers...)
	definitions = append(definitions, investigators...)
	return definitions, nil
}

// currentQARuntime returns the currently published QA runtime snapshot.
func (p *Platform) currentQARuntime() qaRuntimeBundle {
	p.qa.mu.RLock()
	defer p.qa.mu.RUnlock()
	return p.qa.current
}

// currentQAPorts adapts the app-owned QA runtime into the narrow transport
// boundary. The dashboard handler never sees the QARuntime aggregate.
func (p *Platform) currentQAPorts() dashboard.QAApplicationPorts {
	rt := p.currentQARuntime()
	ports := dashboard.QAApplicationPorts{
		SessionStore:   rt.Sessions,
		RunStore:       rt.RunStore,
		MemoryStore:    p.qa.memory,
		Settings:       rt.Settings,
		WriteAvailable: rt.WriteAvailable,
	}
	if rt.Hub != nil {
		hub := rt.Hub
		ports.RuntimeStatus = hub
		if rt.QA != nil {
			ports.Application = &qaApplication{service: rt.QA, hub: hub}
		}
	}
	return ports
}

// initializeQARuntime performs the first QA runtime initialization during
// platform startup.
func (p *Platform) initializeQARuntime(graph *codegraph.DB) error {
	p.qa.reload.Lock()
	defer p.qa.reload.Unlock()

	p.qa.mu.RLock()
	settings := p.settings
	p.qa.mu.RUnlock()
	if settings == nil {
		settings = loadPlatformSettings(p.auth.db)
	}
	return p.rebuildQARuntimeLocked(settings, graph)
}

// applyStoredPlatformSettings loads persisted settings and rebuilds QA only
// when the changed keys can affect QA behavior. Independent settings are
// synchronized without recreating the runtime.
func (p *Platform) applyStoredPlatformSettings(changedKeys []string) error {
	p.qa.reload.Lock()
	defer p.qa.reload.Unlock()

	settings := loadPlatformSettings(p.auth.db)
	p.qa.mu.RLock()
	initialized := p.qa.current.Settings != nil
	graph := p.graph
	p.qa.mu.RUnlock()
	if !initialized || settingsAffectQARuntime(changedKeys) {
		return p.rebuildQARuntimeLocked(settings, graph)
	}

	p.applyPlatformSettingsLocked(settings)
	log.Infof(
		"[settings] platform settings synchronized without rebuilding QA runtime (keys=%v)",
		changedKeys,
	)
	return nil
}

// replaceQACodeGraph updates the graph used by QA. A graph replacement
// rebuilds an initialized runtime because its retrievers capture the graph.
func (p *Platform) replaceQACodeGraph(graph *codegraph.DB) error {
	p.qa.reload.Lock()
	defer p.qa.reload.Unlock()

	p.qa.mu.RLock()
	settings := p.settings
	initialized := p.qa.current.Settings != nil
	p.qa.mu.RUnlock()
	if !initialized {
		p.qa.mu.Lock()
		p.graph = graph
		p.qa.mu.Unlock()
		return nil
	}
	return p.rebuildQARuntimeLocked(settings, graph)
}

// prepareQACatalogSnapshot validates all catalog artifacts in isolated
// registries and returns exact identities ready for live publication.
func (p *Platform) prepareQACatalogSnapshot(
	settings *config.PlatformSettings,
	version int64,
	definitions []agentapi.Definition,
	extensionCapabilities []agentapi.Capability,
) (qaCatalogSnapshot, error) {
	capabilities, err := catalog.DefaultCapabilities(definitions, version)
	if err != nil {
		return qaCatalogSnapshot{}, fmt.Errorf(
			"prepare delegation capabilities: %w",
			err,
		)
	}
	capabilities = append(capabilities, extensionCapabilities...)
	_, preparedDefinitions, preparedCapabilities, err :=
		stageAgentCatalogSnapshot(
			p.agents.schemas,
			definitions,
			capabilities,
		)
	if err != nil {
		return qaCatalogSnapshot{}, err
	}
	return qaCatalogSnapshot{
		version:      version,
		definitions:  preparedDefinitions,
		capabilities: preparedCapabilities,
	}, nil
}

// reusableQACatalogVersion finds one coherent published default version whose
// Agent contents are identical to the generated candidate.
func (p *Platform) reusableQACatalogVersion(
	definitions []agentapi.Definition,
) (int64, bool) {
	if p.agents.catalog == nil || len(definitions) == 0 {
		return 0, false
	}
	var version int64
	for _, candidate := range definitions {
		published, err := p.agents.catalog.Resolve(agentapi.DefinitionRef{
			ID: candidate.ID,
		})
		if err != nil {
			return 0, false
		}
		if version == 0 {
			version = published.Version
		} else if published.Version != version {
			return 0, false
		}
		candidate.Version = published.Version
		candidate.ContentHash = ""
		prepared, err := agentapi.Prepare(candidate)
		if err != nil || prepared.ContentHash != published.ContentHash {
			return 0, false
		}
	}
	return version, version > 0
}

// publishedQACatalogMatches verifies that an exact candidate version already
// exists. During reload, it also checks the in-memory Capability identities.
func (p *Platform) publishedQACatalogMatches(
	snapshot qaCatalogSnapshot,
	requireCapabilities bool,
) bool {
	for _, definition := range snapshot.definitions {
		published, err := p.agents.catalog.Resolve(agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		})
		if err != nil || published.ContentHash != definition.ContentHash {
			return false
		}
	}
	if requireCapabilities {
		for _, capability := range snapshot.capabilities {
			published, err := p.agents.capabilities.Resolve(
				agentapi.CapabilityRef{
					ID: capability.ID, Version: capability.Version,
				},
			)
			if err != nil || published.ContentHash != capability.ContentHash {
				return false
			}
		}
	}
	return true
}

// nextQACatalogVersion allocates above both the active version and restored
// database watermarks, including versions not present in the startup cache.
func (p *Platform) nextQACatalogVersion() int64 {
	return max(
		p.agents.version,
		p.agents.maxVersion,
		p.agents.catalog.MaxVersion(),
	) + 1
}

// qaReloadCandidate is the fully staged, not-yet-active reload result. It
// keeps the expensive build/publish work separate from the atomic activation
// and recovery-worker reconciliation steps.
type qaReloadCandidate struct {
	settings          *config.PlatformSettings
	runtime           qaRuntimeBundle
	definitions       []agentapi.Definition
	definitionRuntime agentapi.Runtime
	catalogVersion    int64
	reusedCatalog     bool
}

// rebuildQARuntimeLocked builds, publishes, configures, and atomically
// activates a new QA runtime while the caller holds the reload lock. It is
// intentionally a thin orchestration around named phase helpers so the failure
// boundary of each lifecycle step is visible.
func (p *Platform) rebuildQARuntimeLocked(
	settings *config.PlatformSettings,
	graph *codegraph.DB,
) error {
	candidate, err := p.stageQARuntimeCandidate(settings, graph)
	if err != nil {
		return err
	}
	if err := p.configureQARuntimeExtensions(candidate); err != nil {
		return err
	}
	p.activateQARuntimeAtomically(candidate, graph)
	p.reconcileQARecoveryWorker(candidate.definitionRuntime)
	p.publishQARuntimeIndexSettings(candidate.settings, candidate.catalogVersion)
	return nil
}

// stageQARuntimeCandidate builds the runtime bundle and stages/publishes the
// catalog without touching the currently active runtime.
func (p *Platform) stageQARuntimeCandidate(
	settings *config.PlatformSettings,
	graph *codegraph.DB,
) (qaReloadCandidate, error) {
	version := p.nextQACatalogVersion()
	candidate, definitions, extensionCapabilities, definitionRuntime, err := p.buildQARuntime(
		settings,
		graph,
		version,
	)
	if err != nil {
		return qaReloadCandidate{}, err
	}
	reusedCatalog := false
	if len(definitions) > 0 {
		definitions, version, reusedCatalog, err = p.publishQACatalog(
			candidate.Settings, version, definitions, extensionCapabilities,
		)
		if err != nil {
			return qaReloadCandidate{}, err
		}
	}
	return qaReloadCandidate{
		settings:          candidate.Settings,
		runtime:           candidate,
		definitions:       definitions,
		definitionRuntime: definitionRuntime,
		catalogVersion:    version,
		reusedCatalog:     reusedCatalog,
	}, nil
}

// publishQACatalog stages and publishes (or reuses) the definition and
// capability catalogs for one reload candidate.
func (p *Platform) publishQACatalog(
	settings *config.PlatformSettings,
	version int64,
	definitions []agentapi.Definition,
	extensionCapabilities []agentapi.Capability,
) ([]agentapi.Definition, int64, bool, error) {
	reusedCatalog := false
	snapshot, err := p.prepareQACatalogSnapshot(
		settings, version, definitions, extensionCapabilities,
	)
	if err != nil {
		return nil, version, false, err
	}
	if reusableVersion, reusable := p.reusableQACatalogVersion(definitions); reusable {
		rewrittenDefinitions, err := rewriteCatalogDefinitions(definitions, reusableVersion)
		if err != nil {
			return nil, version, false, err
		}
		rewrittenCapabilities := rewriteCatalogCapabilities(extensionCapabilities, reusableVersion)
		reusedSnapshot, err := p.prepareQACatalogSnapshot(
			settings, reusableVersion, rewrittenDefinitions, rewrittenCapabilities,
		)
		if err != nil {
			return nil, version, false, err
		}
		p.qa.mu.RLock()
		initialized := p.qa.current.Settings != nil
		p.qa.mu.RUnlock()
		if p.publishedQACatalogMatches(reusedSnapshot, initialized) {
			version = reusableVersion
			snapshot = reusedSnapshot
			reusedCatalog = true
		}
	}
	definitions = snapshot.definitions
	if !reusedCatalog {
		if err := p.agents.catalog.Publish(snapshot.definitions); err != nil {
			return nil, version, false, fmt.Errorf("publish agent definitions: %w", err)
		}
	}
	if err := p.agents.capabilities.Publish(snapshot.capabilities); err != nil {
		return nil, version, false, fmt.Errorf("publish delegation capabilities: %w", err)
	}
	if reusedCatalog {
		log.Infof("[settings] reused published QA catalog version %d", version)
	}
	return definitions, version, reusedCatalog, nil
}

// configureQARuntimeExtensions wires dynamic delegation and feature review
// against the staged runtime before it becomes active.
func (p *Platform) configureQARuntimeExtensions(candidate qaReloadCandidate) error {
	if err := p.configureDynamicDelegation(candidate.settings, candidate.definitionRuntime); err != nil {
		return err
	}
	if err := p.configureFeatureReviewRuntime(
		candidate.settings, candidate.definitionRuntime, candidate.definitions,
	); err != nil {
		return err
	}
	return nil
}

// activateQARuntimeAtomically replaces the active settings/runtime/graph in a
// single lock section. It never leaves a partially updated active snapshot.
func (p *Platform) activateQARuntimeAtomically(candidate qaReloadCandidate, graph *codegraph.DB) {
	p.qa.mu.Lock()
	p.settings = candidate.settings
	p.qa.current = candidate.runtime
	p.agents.runtime = candidate.definitionRuntime
	if len(candidate.definitions) > 0 {
		p.agents.version = candidate.catalogVersion
		p.agents.maxVersion = max(p.agents.maxVersion, candidate.catalogVersion)
	}
	p.graph = graph
	p.qa.mu.Unlock()
}

// reconcileQARecoveryWorker stops the previous durable recovery worker (if it
// differs from the new runtime) and starts the new one under the current
// worker context.
func (p *Platform) reconcileQARecoveryWorker(next agentapi.Runtime) {
	p.qa.mu.RLock()
	oldRuntime := p.agents.runtime
	p.qa.mu.RUnlock()
	if oldRecovery, ok := oldRuntime.(durableRecoveryWorker); ok && oldRuntime != next {
		oldRecovery.StopDurableRecoveryWorker()
	}
	p.workerMu.Lock()
	workerCtx := p.workerCtx
	p.workerMu.Unlock()
	if workerCtx == nil {
		return
	}
	if recovery, ok := next.(durableRecoveryWorker); ok {
		recovery.StartDurableRecoveryWorker(workerCtx, time.Second)
	}
}

// publishQARuntimeIndexSettings pushes the newly active settings into the
// indexing service and emits a concise reload summary.
func (p *Platform) publishQARuntimeIndexSettings(settings *config.PlatformSettings, version int64) {
	p.index.SetPlatform(settings)
	log.Infof("[settings] agent runtimes reloaded (definition_version=%d, model=%s, timeout=%s, max_steps=%d)",
		version, settings.LLMModel,
		time.Duration(settings.AgentTimeout), settings.AgentMaxSteps)
}

// applyPlatformSettingsLocked updates platform and index settings that do not
// require rebuilding the QA runtime.
func (p *Platform) applyPlatformSettingsLocked(
	settings *config.PlatformSettings,
) {
	p.qa.mu.Lock()
	p.settings = settings
	p.qa.current.Settings = settings
	p.qa.mu.Unlock()
	p.index.SetPlatform(settings)
}

// settingsIndependentOfQARuntime lists persisted settings whose changes can
// be applied without rebuilding QA.
var settingsIndependentOfQARuntime = map[string]struct{}{
	"vcs_url":                    {},
	"vcs_token":                  {},
	"vcs_groups":                 {},
	"vcs_webhook_secret":         {},
	"vcs_clone_concurrency":      {},
	"vcs_exclude_projects":       {},
	"coding_enabled_providers":   {},
	"coding_default_provider":    {},
	"coding_codex_model":         {},
	"coding_claude_model":        {},
	"feature_generation_timeout": {},
	"coding_timeout":             {},
	"coding_max_concurrency":     {},
	"coding_allow_network":       {},
	"coding_worktree_ttl":        {},
}

// settingsAffectQARuntime reports whether any changed setting requires a new
// QA runtime. Unknown keys conservatively require a rebuild.
func settingsAffectQARuntime(changedKeys []string) bool {
	for _, key := range changedKeys {
		if _, independent := settingsIndependentOfQARuntime[key]; !independent {
			return true
		}
	}
	return false
}

// setQARuntimeWriteAvailable changes write-action availability in the active
// QA service without rebuilding the runtime.
func (p *Platform) setQARuntimeWriteAvailable(available bool) {
	p.qa.reload.RLock()
	defer p.qa.reload.RUnlock()
	p.qa.mu.Lock()
	defer p.qa.mu.Unlock()
	p.qa.current.WriteAvailable = available
	if p.qa.current.QA != nil {
		p.qa.current.QA.SetWriteAvailable(available)
	}
}

// configureDynamicDelegation reconciles the optional delegation read tool
// with the current settings and runtime.
func (p *Platform) configureDynamicDelegation(
	settings *config.PlatformSettings,
	runtime agentapi.Runtime,
) error {
	set := tool.ReadToolSet{Owner: dynamicDelegationToolOwner}
	if settings == nil || !settings.DelegationEnabled {
		p.configureDelegationWorker(nil)
		if err := p.reads.Reconcile(set); err != nil {
			return fmt.Errorf("disable dynamic delegation tool: %w", err)
		}
		return nil
	}
	if runtime == nil {
		return fmt.Errorf("configure dynamic delegation: agent runtime is unavailable")
	}
	if p.qa.runs == nil {
		return fmt.Errorf("configure dynamic delegation: durable agent run store is required")
	}
	executor, err := delegation.NewExecutor(delegation.ExecutorConfig{
		Capabilities: p.agents.capabilities,
		Definitions:  p.agents.catalog,
		Runtime:      runtime,
		Persistence:  p.qa.runs,
		Policy: agentapi.DelegationPolicy{
			MaxDepth:              1,
			MaxChildren:           settings.DelegationMaxChildren,
			MaxConcurrent:         settings.DelegationMaxConcurrent,
			MaxChildTurns:         settings.DelegationMaxChildTurns,
			MaxChildToolCalls:     settings.DelegationMaxChildToolCalls,
			MaxChildContextTokens: settings.DelegationMaxChildContextTokens,
			MaxChildOutputTokens:  settings.DelegationMaxChildOutputTokens,
			MaxReportTokens:       settings.DelegationMaxReportTokens,
			MaxTotalTokens:        settings.DelegationMaxTotalTokens,
			MaxTotalCostMicros:    settings.DelegationMaxTotalCostMicros,
			ParentAnswerReserve:   settings.DelegationParentAnswerReserve,
			BatchTimeout:          time.Duration(settings.DelegationBatchTimeout),
			ChildTimeout:          time.Duration(settings.DelegationChildTimeout),
			MaxGapChaseRounds:     settings.DelegationGapChaseRounds,
		},
		Allowlist:          settings.DelegationCapabilities,
		VerifierCapability: delegation.SemanticVerifierCapabilityID,
		Events:             runtimeEventEmitter(runtime),
		Queue:              p.qa.runs,
		WorkerOwner:        p.qa.runs.LeaseOwner(),
		WorkerLeaseTTL:     30 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("configure dynamic delegation executor: %w", err)
	}
	available := make(map[string]struct{})
	for _, capability := range executor.Capabilities() {
		available[capability.ID] = struct{}{}
	}
	for _, capability := range settings.DelegationCapabilities {
		if _, ok := available[capability]; !ok {
			return fmt.Errorf(
				"configure dynamic delegation: capability %q is not an enabled read-only investigator",
				capability,
			)
		}
	}
	if len(available) == 0 {
		return fmt.Errorf("configure dynamic delegation: no enabled read-only investigator capabilities")
	}
	set.Tools = []tool.ReadTool{executor.Tool(), executor.StatusTool()}
	if err := p.reads.Reconcile(set); err != nil {
		return fmt.Errorf("publish dynamic delegation tool: %w", err)
	}
	if definitionRuntime, ok := runtime.(*definition.Runtime); ok {
		definitionRuntime.SetDelegationAwaiter(executor)
	}
	p.configureDelegationWorker(executor)
	return nil
}

// configureDelegationWorker publishes one executor to the platform worker
// lifecycle. It is deliberately separate from the read-tool registry so a
// runtime reload cannot leave an old child dispatcher consuming new work.
func (p *Platform) configureDelegationWorker(worker durableChildWorker) {
	if p == nil {
		return
	}
	p.workerMu.Lock()
	oldCancel := p.delegationWorkerCancel
	p.delegationWorkerCancel = nil
	p.delegationWorker = worker
	workerCtx := p.workerCtx
	var childCtx context.Context
	if worker != nil && workerCtx != nil {
		childCtx, p.delegationWorkerCancel = context.WithCancel(workerCtx)
	}
	p.workerMu.Unlock()
	if oldCancel != nil {
		oldCancel()
	}
	if worker != nil && childCtx != nil {
		worker.StartWorker(childCtx, time.Second)
	}
}

func (p *Platform) startAgentWorkers(ctx context.Context) {
	if p == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	p.workerMu.Lock()
	if p.workerCtx == nil {
		p.workerCtx = ctx
	}
	workerCtx := p.workerCtx
	worker := p.delegationWorker
	var childCtx context.Context
	if worker != nil && p.delegationWorkerCancel == nil {
		childCtx, p.delegationWorkerCancel = context.WithCancel(workerCtx)
	}
	p.workerMu.Unlock()
	if worker != nil && childCtx != nil {
		worker.StartWorker(childCtx, time.Second)
	}
	p.qa.mu.RLock()
	runtime := p.agents.runtime
	p.qa.mu.RUnlock()
	if recovery, ok := runtime.(durableRecoveryWorker); ok {
		recovery.StartDurableRecoveryWorker(workerCtx, time.Second)
	}
}

func (p *Platform) stopAgentWorkers() {
	if p == nil {
		return
	}
	p.workerMu.Lock()
	cancel := p.delegationWorkerCancel
	p.delegationWorkerCancel = nil
	p.delegationWorker = nil
	p.workerCtx = nil
	p.workerMu.Unlock()
	if cancel != nil {
		cancel()
	}
	p.qa.mu.RLock()
	runtime := p.agents.runtime
	p.qa.mu.RUnlock()
	if recovery, ok := runtime.(durableRecoveryWorker); ok {
		recovery.StopDurableRecoveryWorker()
	}
}

// runtimeEventEmitter returns the runtime's optional delegation event
// emitter, if it implements that interface.
func runtimeEventEmitter(runtime agentapi.Runtime) delegation.EventEmitter {
	emitter, ok := runtime.(delegation.EventEmitter)
	if !ok {
		log.Warnf("[qa] delegation event emitter unavailable; delegation progress will not be broadcast (runtime_type=%T)", runtime)
	}
	return emitter
}

// validateAgentCatalogContribution checks extension-owned catalog entries
// before they are merged into the next catalog version.
func validateAgentCatalogContribution(
	contribution AgentCatalogContribution,
	version int64,
) error {
	for _, definition := range contribution.Definitions {
		if definition.Version != version {
			return fmt.Errorf(
				"application agent definition %q version %d does not match catalog version %d",
				definition.ID,
				definition.Version,
				version,
			)
		}
	}
	for _, capability := range contribution.Capabilities {
		if capability.Version != version {
			return fmt.Errorf(
				"application capability %q version %d does not match catalog version %d",
				capability.ID,
				capability.Version,
				version,
			)
		}
	}
	return nil
}

// stageAgentCatalogSnapshot validates an unpublished Agent and Capability
// snapshot and returns the prepared capability identities used by Workflow.
func stageAgentCatalogSnapshot(
	schemas *agentapi.SchemaRegistry,
	definitions []agentapi.Definition,
	capabilities []agentapi.Capability,
) (
	*agentapi.CapabilityRegistry,
	[]agentapi.Definition,
	[]agentapi.Capability,
	error,
) {
	stagedDefinitions := catalog.New(schemas)
	if err := stagedDefinitions.Publish(definitions); err != nil {
		return nil, nil, nil, fmt.Errorf("validate agent definitions: %w", err)
	}
	stagedCapabilities := agentapi.NewCapabilityRegistry(
		schemas,
		stagedDefinitions,
	)
	if err := stagedCapabilities.Publish(capabilities); err != nil {
		return nil, nil, nil, fmt.Errorf("validate agent capabilities: %w", err)
	}
	preparedDefinitions := make([]agentapi.Definition, 0, len(definitions))
	for _, definition := range definitions {
		resolved, err := stagedDefinitions.Resolve(agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf(
				"resolve staged agent definition %q version %d: %w",
				definition.ID,
				definition.Version,
				err,
			)
		}
		preparedDefinitions = append(preparedDefinitions, resolved)
	}
	prepared := make([]agentapi.Capability, 0, len(capabilities))
	for _, capability := range capabilities {
		resolved, err := stagedCapabilities.Resolve(agentapi.CapabilityRef{
			ID: capability.ID, Version: capability.Version,
		})
		if err != nil {
			return nil, nil, nil, fmt.Errorf(
				"resolve staged capability %q version %d: %w",
				capability.ID,
				capability.Version,
				err,
			)
		}
		prepared = append(prepared, resolved)
	}
	return stagedCapabilities, preparedDefinitions, prepared, nil
}

func (p *Platform) configureAgentWorkflowRuntime(runtime agentapi.Runtime) error {
	if p.flow.service == nil {
		return nil
	}
	transforms := newWorkflowTransformDispatcher(
		p.flow.pipeline,
		p.flow.review,
	)
	var agentNodes *workflow.AgentExecutor
	if runtime != nil {
		nodes, err := workflow.NewAgentExecutor(
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
	runner := workflow.NewOrchestrator(
		p.agents.schemas,
		nodes,
		map[string]workflow.GateEvaluator{},
	)
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

// webQueryRewriteSystem instructs the fast model to turn a natural-language
// question into a compact keyword query. Output is a single search string,
// never prose.
const webQueryRewriteSystem = "Rewrite the user's question into a short keyword search query for a web search engine. Keep technical terms, entities, and the specific thing being asked; drop question words and filler. Return only the search query, with no explanation."

// webQueryRewriter wraps the fast model as a web search query rewriter. It
// returns nil when no fast model is available, leaving the deterministic
// question-word strip in place as the fallback.
func webQueryRewriter(client *llm.LLMClient) tools.WebQueryRewriter {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, query string) (string, error) {
		rewritten, err := client.ChatText(ctx, webQueryRewriteSystem, query, llm.CallOptions{MaxTokens: 128})
		return strings.TrimSpace(rewritten), err
	}
}

// rewriteCatalogDefinitions clones the prepared candidate definitions and
// rebinds them to a reused catalog version. It preserves the exact published
// content for each ID while only changing the version identity, so a reused
// version can be verified against the live catalog without rebuilding the
// runtime or generating a second definition batch.
func rewriteCatalogDefinitions(
	definitions []agentapi.Definition,
	version int64,
) ([]agentapi.Definition, error) {
	rewritten := make([]agentapi.Definition, 0, len(definitions))
	for _, definition := range definitions {
		definition.Version = version
		definition.ContentHash = ""
		prepared, err := agentapi.Prepare(definition)
		if err != nil {
			return nil, fmt.Errorf(
				"rewrite agent definition %q to reused version %d: %w",
				definition.ID,
				version,
				err,
			)
		}
		rewritten = append(rewritten, prepared)
	}
	return rewritten, nil
}

// rewriteCatalogCapabilities rebinds extension-owned capabilities to a reused
// catalog version. Both the capability identity and its pinned agent
// definition reference must move together, otherwise the reused snapshot fails
// validation because the capability points at a version the definitions no
// longer carry. The content hash is cleared so the capability registry
// recomputes it for the rebased identity.
func rewriteCatalogCapabilities(
	capabilities []agentapi.Capability,
	version int64,
) []agentapi.Capability {
	rewritten := make([]agentapi.Capability, 0, len(capabilities))
	for _, capability := range capabilities {
		capability.Version = version
		capability.Agent.Version = version
		capability.ContentHash = ""
		rewritten = append(rewritten, capability)
	}
	return rewritten
}
