package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/incident"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/approval"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/feature/pipeline"
	"github.com/dekwanlabs/nasuta/internal/feature/reviewworkflow"
	"github.com/dekwanlabs/nasuta/internal/indexing"
	"github.com/dekwanlabs/nasuta/internal/investigation"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/platform/ontologystore"
	"github.com/dekwanlabs/nasuta/internal/platform/semanticstore"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/rbac"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/internal/sessionhistory"
	"github.com/dekwanlabs/nasuta/internal/transport/agenthttp"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
	"github.com/dekwanlabs/nasuta/internal/transport/incidenthttp"
	"github.com/dekwanlabs/nasuta/internal/transport/investigationhttp"
	"github.com/dekwanlabs/nasuta/internal/transport/mcp"
	"github.com/dekwanlabs/nasuta/internal/transport/routes"
	"github.com/dekwanlabs/nasuta/internal/transport/webhook"
	"github.com/dekwanlabs/nasuta/internal/transport/workflowhttp"
	"github.com/dekwanlabs/nasuta/internal/writeaction"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

const workflowRecoveryPageSize = 100

// Platform owns reusable runtime state and exposes only stable composition ports.
type Platform struct {
	cfg                config.Config
	settings           *config.PlatformSettings
	platformDB         *sql.DB
	index              *indexing.Service
	knowledge          *agent.Service
	registry           *tool.Registry
	readTools          *tool.ReadRegistry
	writeReady         bool
	authDB             *auth.DB
	authService        *auth.Service
	rbacHandler        *rbac.Handler
	mcpKeyAuth         routes.MCPKeyAuthenticator
	rolePrompt         func(int64) string
	incidents          *incident.Manager
	actions            *approval.Service
	incidentAPI        *incidenthttp.Handler
	codegraph          *codegraph.DB
	callChain          *callchain.Service
	ontology           ontology.Backend
	history            *sessionhistory.Service
	featureDelivery    featureDeliveryRuntime
	qaReloadMu         sync.RWMutex
	qaMu               sync.RWMutex
	agentDefinitionVer int64
	schemaRegistry     *agentapi.SchemaRegistry
	agentCatalog       *catalog.Catalog
	workflowCatalog    *workflow.Catalog
	workflowStore      *workflow.Store
	workflowNodes      *workflow.AgentNodeExecutor
	workflowPipeline   *pipeline.Executor
	workflowReview     *reviewworkflow.Executor
	reviewCoordinator  *reviewworkflow.Coordinator
	workflowTransforms workflow.NodeExecutor
	workflowRunner     *workflow.Orchestrator
	workflowService    *workflow.Service
	workflowAPI        *workflowhttp.Handler
	agentAPI           *agenthttp.Handler
	investigationAPI   *investigationhttp.Handler
	qaSessions         *memory.SessionStore
	qaRunStore         *agent.RunStore
	qaRuntime          dashboard.QARuntime
	definitionRuntime  agentapi.Runtime
}

// New constructs the reusable platform without registering scenario routes.
func New() (*Platform, error) {
	cfg := config.Load()
	InitLogging(cfg.Log)

	platformDB, platformDBErr := openPlatformDB()
	docDB := store.NewDocStore(platformDB)
	index, err := indexing.Build(cfg, docDB, platformDBErr)
	if err != nil {
		if platformDB != nil {
			_ = platformDB.Close()
		}
		return nil, fmt.Errorf("build platform index: %w", err)
	}
	ontologyBackend, err := ontologystore.New(cfg.Ontology, index.DB)
	if err != nil {
		index.Close()
		if platformDB != nil {
			_ = platformDB.Close()
		}
		return nil, fmt.Errorf("build ontology backend: %w", err)
	}
	index.SetOntologyPublisher(ontologyBackend)
	codeGraph, err := codegraph.Open(cfg.WorkspaceRoot)
	if err != nil {
		log.Warnf("[server] codegraph call-chain disabled: %v", err)
		codeGraph = nil
	}
	callChainService := callchain.New(index.DB, codeGraph)
	knowledgeService := agent.NewTools(agent.Deps{
		DB: index.DB, Semantic: index.Semantic,
		Embedder: index.Embedder, WorkspaceRoot: cfg.WorkspaceRoot, DocStore: index.DocDB(),
		CallChain: callChainService, Ontology: ontology.NewService(ontologyBackend),
	})
	index.SetTools(knowledgeService)
	knowledgeService.SetWebSearchEngine(cfg.WebSearchEngine)
	knowledgeService.SetWebSearchAPIKey(cfg.WebSearchAPIKey)

	authDB, authService := buildAuth(cfg, platformDB)
	settings := loadPlatformSettings(authDB)
	index.SetPlatform(settings)
	sessions := memory.NewSessionStore(platformDB)
	history := buildSessionHistory(cfg, sessions, index.Embedder)
	registry := agent.NewRegistry(knowledgeService, cfg, sessions, history)
	schemaRegistry := agentapi.NewSchemaRegistry()
	if err := schemaRegistry.Publish(catalog.DefaultSchemas()); err != nil {
		index.Close()
		if platformDB != nil {
			_ = platformDB.Close()
		}
		return nil, fmt.Errorf("publish default agent schemas: %w", err)
	}
	if err := schemaRegistry.Publish(pipeline.Schemas()); err != nil {
		index.Close()
		if platformDB != nil {
			_ = platformDB.Close()
		}
		return nil, fmt.Errorf("publish feature pipeline schemas: %w", err)
	}
	if err := schemaRegistry.Publish(reviewworkflow.Schemas()); err != nil {
		index.Close()
		if platformDB != nil {
			_ = platformDB.Close()
		}
		return nil, fmt.Errorf("publish feature review schemas: %w", err)
	}

	agentCatalog := catalog.New(schemaRegistry)
	platform := &Platform{
		cfg: cfg, settings: settings, index: index, knowledge: knowledgeService,
		registry: registry, readTools: tool.NewReadRegistry(registry),
		platformDB: platformDB, authDB: authDB, authService: authService,
		codegraph: codeGraph, callChain: callChainService,
		ontology: ontologyBackend,
		history:  history, qaSessions: sessions,
		schemaRegistry: schemaRegistry, agentCatalog: agentCatalog,
		workflowCatalog: workflow.NewCatalog(schemaRegistry, agentCatalog),
	}
	platform.agentAPI = agenthttp.New(agentCatalog)
	platform.investigationAPI = investigationhttp.New(investigation.New(
		platformInvestigationExecutor{platform: platform},
	))
	if platformDB != nil {
		agentStore, agentStoreErr := catalog.NewStore(platformDB)
		if agentStoreErr != nil {
			_ = platform.Close()
			return nil, fmt.Errorf("configure agent catalog store: %w", agentStoreErr)
		}
		if attachErr := agentCatalog.AttachStore(context.Background(), agentStore); attachErr != nil {
			_ = platform.Close()
			return nil, fmt.Errorf("restore agent catalog: %w", attachErr)
		}
		platform.agentDefinitionVer = agentCatalog.MaxVersion()
		log.Infof(
			"[agent] catalog persistence enabled (restored_max_version=%d)",
			platform.agentDefinitionVer,
		)
		runStore, runStoreErr := agent.NewRunStore(platformDB)
		if runStoreErr != nil {
			log.Warnf("[qa] agent run store disabled: %v", runStoreErr)
		} else {
			platform.qaRunStore = runStore
			log.Infof("[qa] agent run store enabled (MySQL)")
		}
	}
	if err := platform.initAgentWorkflow(); err != nil {
		_ = platform.Close()
		return nil, fmt.Errorf("configure agent workflow: %w", err)
	}
	if err := platform.reloadQARuntime(codeGraph); err != nil {
		_ = platform.Close()
		return nil, fmt.Errorf("configure QA runtime: %w", err)
	}
	platform.initRBAC()
	if err := platform.initFeatureDelivery(); err != nil {
		_ = platform.Close()
		return nil, fmt.Errorf("configure feature delivery: %w", err)
	}
	return platform, nil
}

func (platform *Platform) initAgentWorkflow() error {
	if platform.platformDB == nil {
		log.Warnf("[workflow] persistence and execution disabled (MySQL unavailable)")
		return nil
	}
	workflowStore, err := workflow.NewStore(platform.platformDB)
	if err != nil {
		return err
	}
	if err := platform.workflowCatalog.AttachStore(
		context.Background(),
		workflowStore,
	); err != nil {
		return fmt.Errorf("restore workflow catalog: %w", err)
	}
	platform.agentDefinitionVer = max(
		platform.agentDefinitionVer,
		platform.workflowCatalog.MaxVersion(),
	)
	service, err := workflow.NewService(platform.workflowCatalog, workflowStore, nil)
	if err != nil {
		return err
	}
	platform.workflowStore = workflowStore
	platform.workflowService = service
	platform.workflowAPI = workflowhttp.New(service)
	log.Infof("[workflow] persistence enabled (MySQL)")
	return nil
}

func buildSessionHistory(cfg config.Config, sessions *memory.SessionStore, emb embed.Embedder) *sessionhistory.Service {
	if sessions == nil {
		return nil
	}
	if emb == nil || !emb.Enabled() {
		log.Warnf("[qa] session history dense index disabled; lexical recall remains available")
		return sessionhistory.New(sessions, nil, emb)
	}
	historyConfig := cfg.Semantic
	historyConfig.Collection = "session_history"
	historySemantic, err := semanticstore.New(historyConfig)
	if err != nil {
		log.Errorf("[qa] session history semantic store unavailable; lexical recall remains available: %v", err)
		return sessionhistory.New(sessions, nil, emb)
	}
	if err := historySemantic.Ensure(context.Background(), semantic.Schema{Collection: "session_history", DenseDim: emb.Dim()}); err != nil {
		_ = historySemantic.Close()
		log.Errorf("[qa] session history collection unavailable; lexical recall remains available: %v", err)
		return sessionhistory.New(sessions, nil, emb)
	}
	history := sessionhistory.New(sessions, historySemantic, emb)
	vocabPath := filepath.Join(cfg.WorkspaceRoot, platform.WorkspaceMetadataDir, "history_bm25_vocab.json")
	if err := history.EnableBM25(vocabPath); err != nil {
		log.Errorf("[qa] session history BM25 disabled; dense and lexical recall remain available: %v", err)
		return history
	}
	log.Infof("[qa] session history hybrid index enabled (collection=session_history)")
	return history
}

func openPlatformDB() (*sql.DB, error) {
	dsn := config.LoadMySQLDSN()
	if dsn == "" {
		err := fmt.Errorf("MYSQL_DSN not set")
		log.Warnf("[server] MySQL-backed capabilities disabled (%v)", err)
		return nil, err
	}
	db, err := store.OpenMySQL(dsn)
	if err != nil {
		log.Warnf("[server] MySQL-backed capabilities disabled: %v", err)
		return nil, err
	}
	log.Infof("[server] MySQL platform store enabled")
	return db, nil
}

func buildAuth(cfg config.Config, db *sql.DB) (*auth.DB, *auth.Service) {
	if db == nil {
		log.Warnf("[server] auth disabled (MySQL unavailable)")
		return nil, nil
	}
	authDB := auth.NewDB(db)
	oauth := auth.NewFeishuOAuth(cfg.FeishuAppID, cfg.FeishuAppSecret)
	log.Infof("[server] auth enabled (MySQL: ok, Feishu: %v)", cfg.FeishuConfigured())
	return authDB, auth.NewService(oauth, authDB, cfg.FeishuRedirectURI, cfg.WebBaseURL)
}

func loadPlatformSettings(authDB *auth.DB) *config.PlatformSettings {
	settings := &config.PlatformSettings{}
	settings.Apply(nil)
	if authDB == nil {
		return settings
	}
	stored, err := authDB.GetSettings()
	if err != nil {
		log.Warnf("[server] platform settings unavailable: %v", err)
		return settings
	}
	settings.Apply(stored)
	return settings
}

func (platform *Platform) initRBAC() {
	if platform.platformDB == nil {
		return
	}
	store, err := rbac.NewStore(platform.platformDB)
	if err != nil {
		log.Warnf("[server] RBAC store init failed: %v", err)
		platform.mcpKeyAuth = func(context.Context, string) (bool, error) {
			return false, fmt.Errorf("RBAC store unavailable: %w", err)
		}
		return
	}
	platform.rbacHandler = rbac.NewHandler(store)
	platform.mcpKeyAuth = store.AuthenticateMCPKey
	platform.rolePrompt = store.RolePromptFor
	log.Infof("[server] RBAC enabled")
}

// Knowledge returns the stable read-only API available to scenario tools.
func (platform *Platform) Knowledge() knowledge.API { return platform.knowledge }

// ReadTools returns the restricted publisher available to scenario code.
func (platform *Platform) ReadTools() *tool.ReadRegistry { return platform.readTools }

func (platform *Platform) configureIncidents(evidence incident.EvidenceProvider) error {
	if platform.platformDB == nil {
		log.Warnf("[server] incident and approval disabled (MySQL unavailable)")
		return nil
	}
	return platform.configureIncidentsWithDB(platform.platformDB, evidence)
}

func (platform *Platform) configureIncidentsWithDB(db *sql.DB, evidence incident.EvidenceProvider) error {
	if platform.writeReady {
		return fmt.Errorf("incident workflows are already configured")
	}
	cfg := incident.Config{
		WebBaseURL:          platform.cfg.WebBaseURL,
		NotifyFeishuWebhook: platform.cfg.NotifyFeishuWebhook,
		NotifyWecomWebhook:  platform.cfg.NotifyWecomWebhook,
		NotifyHTTPWebhook:   platform.cfg.NotifyHTTPWebhook,
		FixDefaultAssignee:  platform.cfg.FixDefaultAssignee,
		FixBranchPrefix:     platform.cfg.FixBranchPrefix,
		LLMBaseURL:          platform.settings.LLMBaseURL,
		LLMAPIKey:           platform.settings.LLMAPIKey,
		LLMModel:            platform.settings.LLMModel,
		LLMProvider:         platform.settings.LLMProvider,
		LLMMaxTokens:        platform.settings.LLMMaxTokens,
		VCSURL:              platform.settings.VCSURL,
		VCSToken:            platform.settings.VCSToken,
	}
	manager, err := incident.NewManager(
		cfg, db, platform.cfg.WorkspaceRoot, evidence, platform.knowledge,
	)
	if err != nil {
		return fmt.Errorf("configure incident manager: %w", err)
	}
	actions, err := approval.NewService(db, manager)
	if err != nil {
		return fmt.Errorf("configure approval service: %w", err)
	}
	if err := writeaction.RegisterBuiltins(platform.registry, actions); err != nil {
		return fmt.Errorf("configure write actions: %w", err)
	}
	platform.incidents = manager
	platform.actions = actions
	platform.incidentAPI = incidenthttp.New(manager, actions, platform.cfg.AlertWebhookSecret)
	platform.writeReady = true
	if platform.agentCatalog != nil {
		if err := platform.reloadQARuntime(platform.codegraph); err != nil {
			return fmt.Errorf("reload QA runtime with write actions: %w", err)
		}
	}
	log.Infof("[server] incident and approval workflows enabled")
	return nil
}

// WorkspaceRoot returns the canonical workspace path established by core config.
func (platform *Platform) WorkspaceRoot() string { return platform.cfg.WorkspaceRoot }

// Settings returns a detached copy so scenario composition cannot mutate platform state.
func (platform *Platform) Settings() config.PlatformSettings {
	platform.qaMu.RLock()
	defer platform.qaMu.RUnlock()
	settings := *platform.settings
	settings.VCSGroups = append([]string(nil), platform.settings.VCSGroups...)
	settings.VCSExcludeProjects = append([]string(nil), platform.settings.VCSExcludeProjects...)
	settings.CodingEnabledProviders = append([]string(nil), platform.settings.CodingEnabledProviders...)
	return settings
}

// RegisterCommonRoutes attaches only reusable platform routes to mux.
func (platform *Platform) RegisterCommonRoutes(mux *http.ServeMux) {
	dashboardHandler := dashboard.NewHandler(
		platform.index.DB, platform.index.DocDB(), platform.authDB,
		platform.index.Semantic, platform.index.Embedder,
		platform.knowledge, platform.cfg, platform.index,
		platform.codegraph, platform.callChain,
		platform.currentQARuntime, platform.reloadQARuntime,
	)
	if platform.rolePrompt != nil {
		dashboardHandler.SetRolePrompt(platform.rolePrompt)
	}
	dashboardHandler.SetFeatureDeliveryStatus(platform.featureDelivery.status)
	routes.Setup(mux, routes.Config{
		Auth: platform.authService, Dashboard: dashboardHandler, RBAC: platform.rbacHandler,
		MCP:        mcp.NewDynamicHandler(platform.registry),
		MCPKeyAuth: platform.mcpKeyAuth,
		VCS:        webhook.VCSHandler(platform.index, platform.settings.VCSWebhookSecret),
		Cfg:        platform.cfg,
	})
	if platform.incidentAPI != nil {
		platform.incidentAPI.RegisterRoutes(platform.AuthenticatedAPI(mux))
	}
	if platform.featureDelivery.api != nil {
		platform.featureDelivery.api.RegisterRoutes(platform.AuthenticatedAPI(mux))
	}
	if platform.investigationAPI != nil {
		platform.investigationAPI.RegisterRoutes(platform.AuthenticatedAPI(mux))
	}
	if platform.agentAPI != nil {
		platform.agentAPI.RegisterRoutes(platform.AuthenticatedAPI(mux))
	}
	if platform.workflowAPI != nil {
		platform.workflowAPI.RegisterRoutes(platform.AuthenticatedAPI(mux))
	}
}

func (platform *Platform) buildQARuntime(
	settings *config.PlatformSettings,
	graph *codegraph.DB,
	version int64,
) (dashboard.QARuntime, []agentapi.Definition, agentapi.Runtime, error) {
	snapshot := *settings
	snapshot.VCSGroups = append([]string(nil), settings.VCSGroups...)
	snapshot.VCSExcludeProjects = append([]string(nil), settings.VCSExcludeProjects...)
	snapshot.CodingEnabledProviders = append([]string(nil), settings.CodingEnabledProviders...)
	if !snapshot.LLMEnabled() {
		return dashboard.QARuntime{
			RunStore: platform.qaRunStore, Sessions: platform.qaSessions,
			History: platform.history, Settings: &snapshot,
			WriteAvailable: platform.writeReady,
		}, nil, nil, nil
	}
	if err := snapshot.ValidateAgentSettings(); err != nil {
		return dashboard.QARuntime{}, nil, nil, err
	}
	definitions, err := defaultAgentDefinitions(&snapshot, version)
	if err != nil {
		return dashboard.QARuntime{}, nil, nil, err
	}
	definitionRuntime, err := agent.NewDefinitionRuntime(
		platform.agentCatalog,
		platform.schemaRegistry,
		platform.registry,
		&snapshot,
		platform.qaRunStore,
	)
	if err != nil {
		return dashboard.QARuntime{}, nil, nil, fmt.Errorf("configure definition runtime: %w", err)
	}
	models := agent.NewQAModels(&snapshot)
	investigationRunner := platformQAInvestigationRunner{
		platform: platform,
		events:   definitionRuntime,
	}
	qa := agent.NewQA(agent.QADeps{
		Tools: platform.knowledge, Semantic: platform.index.Semantic,
		Embedder: platform.index.Embedder, Cfg: platform.cfg, Platform: &snapshot,
		CodeGraphDB: graph, DB: platform.platformDB,
		History: platform.history, Sessions: platform.qaSessions,
		Definitions:       platform.agentCatalog,
		Agent:             agentapi.DefinitionRef{ID: definitions[0].ID},
		Runtime:           definitionRuntime,
		RuntimeTools:      definitionRuntime,
		Models:            models,
		PhaseEmitter:      definitionRuntime,
		Investigation:     investigationRunner,
		ScenarioLifecycle: definitionRuntime,
		ExecutionEvents:   definitionRuntime,
		WriteAvailable:    platform.writeReady,
	})
	return dashboard.QARuntime{
		QA: qa, RunStore: platform.qaRunStore, Sessions: platform.qaSessions,
		History: platform.history, Settings: &snapshot,
		WriteAvailable: platform.writeReady, Hub: definitionRuntime.Hub(),
		CompactionLLM: models.Primary(),
	}, definitions, definitionRuntime, nil
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

func (platform *Platform) currentQARuntime() dashboard.QARuntime {
	platform.qaMu.RLock()
	defer platform.qaMu.RUnlock()
	return platform.qaRuntime
}

func (platform *Platform) reloadQARuntime(graph *codegraph.DB) error {
	platform.qaReloadMu.Lock()
	defer platform.qaReloadMu.Unlock()

	settings := loadPlatformSettings(platform.authDB)
	version := platform.agentDefinitionVer + 1
	candidate, definitions, definitionRuntime, err := platform.buildQARuntime(settings, graph, version)
	if err != nil {
		return err
	}
	if len(definitions) > 0 {
		if err := platform.agentCatalog.Publish(definitions); err != nil {
			return fmt.Errorf("publish agent definitions: %w", err)
		}
		workflowDefinition, err := workflow.DefaultDelegatedInvestigation(
			version,
			time.Duration(candidate.Settings.AgentTimeout),
		)
		if err != nil {
			return fmt.Errorf("prepare delegated investigation workflow: %w", err)
		}
		if err := platform.workflowCatalog.Publish([]workflow.WorkflowDefinition{workflowDefinition}); err != nil {
			return fmt.Errorf("publish delegated investigation workflow: %w", err)
		}
	}
	if err := platform.configureFeatureReviewRuntime(candidate.Settings, definitionRuntime, definitions); err != nil {
		return err
	}
	if err := platform.configureAgentWorkflowRuntime(definitionRuntime); err != nil {
		return err
	}

	platform.qaMu.Lock()
	platform.settings = candidate.Settings
	platform.qaRuntime = candidate
	platform.definitionRuntime = definitionRuntime
	if len(definitions) > 0 {
		platform.agentDefinitionVer = version
	}
	platform.codegraph = graph
	platform.qaMu.Unlock()
	platform.index.SetPlatform(candidate.Settings)
	log.Infof("[settings] agent runtimes reloaded (definition_version=%d, model=%s, timeout=%s, max_steps=%d)",
		platform.agentDefinitionVer, candidate.Settings.LLMModel,
		time.Duration(candidate.Settings.AgentTimeout), candidate.Settings.AgentMaxSteps)
	return nil
}

func (platform *Platform) configureAgentWorkflowRuntime(runtime agentapi.Runtime) error {
	if platform.workflowService == nil {
		return nil
	}
	platform.workflowTransforms = newWorkflowTransformDispatcher(
		platform.workflowPipeline,
		platform.workflowReview,
	)
	var agentNodes *workflow.AgentNodeExecutor
	if runtime != nil {
		nodes, err := workflow.NewAgentNodeExecutor(
			platform.schemaRegistry,
			platform.agentCatalog,
			runtime,
		)
		if err != nil {
			return fmt.Errorf("configure workflow agent nodes: %w", err)
		}
		agentNodes = nodes
	}
	if agentNodes == nil && platform.workflowTransforms == nil {
		platform.workflowService.SetOrchestrator(nil)
		platform.workflowNodes = nil
		platform.workflowRunner = nil
		log.Warnf("[workflow] execution disabled (agent and transform executors unavailable)")
		return nil
	}
	nodes := workflow.NewNodeDispatcher(agentNodes, platform.workflowTransforms)
	runner := workflow.NewOrchestrator(platform.schemaRegistry, nodes, nil)
	platform.workflowService.SetOrchestrator(runner)
	platform.workflowNodes = agentNodes
	platform.workflowRunner = runner
	switch {
	case agentNodes != nil && platform.workflowTransforms != nil:
		log.Infof("[workflow] agent and feature transform execution enabled")
	case agentNodes != nil:
		log.Infof("[workflow] agent execution enabled")
	default:
		log.Infof("[workflow] feature transform execution enabled (LLM unavailable)")
	}
	return nil
}

// AuthenticatedAPI gives the root composition the platform auth boundary without exposing its store.
func (platform *Platform) AuthenticatedAPI(mux *http.ServeMux) APIRegistrar {
	return routes.AuthenticatedAPI(mux, platform.authService)
}

// Serve runs background platform work and serves the already-composed root mux.
func (platform *Platform) Serve(ctx context.Context, mux *http.ServeMux) error {
	workflowRecoveryCutoff := time.Now().UTC()
	go platform.startDailySyncTicker(ctx)
	platform.featureDelivery.start(ctx)
	if platform.history != nil {
		go platform.history.Run(ctx)
	}
	if platform.workflowService != nil && platform.workflowRunner != nil {
		go platform.recoverActiveWorkflows(ctx, workflowRecoveryCutoff)
	}
	log.Infof("[server] listening on %s (MCP: /mcp, webhook: /internal/vcs-hook, api: /api)", platform.cfg.HTTPAddr)
	if platform.cfg.AuthToken == "" && platform.mcpKeyAuth == nil {
		log.Warnf("[server] WARNING: no MCP authentication configured, /mcp is unauthenticated")
	}
	if err := http.ListenAndServe(platform.cfg.HTTPAddr, routes.TraceMiddleware(mux)); err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func (platform *Platform) recoverActiveWorkflows(ctx context.Context, startedBefore time.Time) {
	report, err := platform.workflowService.RecoverActiveWithObserver(
		ctx,
		startedBefore,
		workflowRecoveryPageSize,
		func(
			ctx context.Context,
			runID string,
			result workflow.ResumeResult,
			resumeErr error,
		) error {
			if platform.reviewCoordinator == nil {
				return nil
			}
			_, err := platform.reviewCoordinator.ReconcileRecoveredRun(
				ctx,
				runID,
				result.Status,
				resumeErr,
			)
			return err
		},
	)
	if err != nil {
		log.ErrorfCtx(
			ctx,
			"[workflow] startup recovery incomplete scanned=%d resumed=%d succeeded=%d waiting_human=%d failed=%d cancelled=%d timed_out=%d errors=%d: %v",
			report.Scanned,
			report.Resumed,
			report.Succeeded,
			report.WaitingHuman,
			report.Failed,
			report.Cancelled,
			report.TimedOut,
			report.Errors,
			err,
		)
		return
	}
	log.InfofCtx(
		ctx,
		"[workflow] startup recovery complete scanned=%d resumed=%d succeeded=%d waiting_human=%d failed=%d cancelled=%d timed_out=%d",
		report.Scanned,
		report.Resumed,
		report.Succeeded,
		report.WaitingHuman,
		report.Failed,
		report.Cancelled,
		report.TimedOut,
	)
}

// Close releases reusable platform resources.
func (platform *Platform) Close() error {
	if platform.workflowService != nil {
		platform.workflowService.Close()
	}
	if platform.incidents != nil {
		_ = platform.incidents.Close()
	}
	_ = platform.callChain.Close()
	if platform.ontology != nil {
		_ = platform.ontology.Close()
	}
	if platform.history != nil {
		_ = platform.history.Close()
	}
	platform.index.Close()
	if platform.platformDB != nil {
		return platform.platformDB.Close()
	}
	return nil
}

func (platform *Platform) startDailySyncTicker(ctx context.Context) {
	atTime := platform.cfg.DailySyncTime
	for {
		next, err := nextDailySyncAt(atTime)
		if err != nil {
			log.WarnfCtx(ctx, "[daily-sync] bad NASUTA_DAILY_SYNC_TIME %q (%v), defaulting to 02:07", atTime, err)
			next, _ = nextDailySyncAt("02:07")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(time.Until(next), time.Second)):
		}
		if platform.authDB == nil {
			log.WarnfCtx(ctx, "[daily-sync] auth store unavailable - skipping")
			continue
		}
		settings, err := platform.authDB.GetSettings()
		if err != nil {
			log.ErrorfCtx(ctx, "[daily-sync] load settings: %v", err)
			continue
		}
		vcsURL := settings["vcs_url"]
		vcsToken := settings["vcs_token"]
		vcsGroups := settings["vcs_groups"]
		if vcsURL == "" || vcsToken == "" || vcsGroups == "" {
			log.WarnfCtx(ctx, "[daily-sync] VCS not configured - skipping")
			continue
		}
		if err := platform.index.DailySync(ctx,
			vcsURL, vcsToken, vcsGroups,
			settings["vcs_clone_concurrency"], settings["vcs_exclude_projects"],
		); err != nil {
			log.ErrorfCtx(ctx, "[daily-sync] run failed: %v", err)
		}
	}
}

func nextDailySyncAt(value string) (time.Time, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("expected HH:MM")
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return time.Time{}, fmt.Errorf("bad hour %q", parts[0])
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return time.Time{}, fmt.Errorf("bad minute %q", parts[1])
	}
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next, nil
}
