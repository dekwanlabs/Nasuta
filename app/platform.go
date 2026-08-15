package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/incident"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/feature/pipeline"
	"github.com/dekwanlabs/nasuta/internal/feature/reviewworkflow"
	"github.com/dekwanlabs/nasuta/internal/indexing"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/ontologystore"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/rbac"
	"github.com/dekwanlabs/nasuta/internal/sessionhistory"
	"github.com/dekwanlabs/nasuta/internal/transport/agenthttp"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
	"github.com/dekwanlabs/nasuta/internal/transport/incidenthttp"
	"github.com/dekwanlabs/nasuta/internal/transport/routes"
	"github.com/dekwanlabs/nasuta/internal/transport/workflowhttp"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

type authRuntime struct {
	db      *auth.DB
	service *auth.Service
	rbac    *rbac.Handler
	keyAuth routes.MCPKeyAuthenticator
	prompt  func(int64) string
}

type incidentRuntime struct {
	manager *incident.Manager
	api     *incidenthttp.Handler
}

type agentRuntime struct {
	version      int64
	schemas      *agentapi.SchemaRegistry
	catalog      *catalog.Catalog
	capabilities *agentapi.CapabilityRegistry
	provider     AgentCatalogProvider
	runtime      agentapi.Runtime
	api          *agenthttp.Handler
}

type qaState struct {
	reload   sync.RWMutex
	mu       sync.RWMutex
	sessions *memory.SessionStore
	memory   *memory.MemoryStore
	runs     *run.Store
	current  dashboard.QARuntime
}

type workflowRuntime struct {
	catalog     *workflow.Catalog
	pipeline    *pipeline.Executor
	review      *reviewworkflow.Executor
	coordinator *reviewworkflow.Coordinator
	service     *workflow.Service
	api         *workflowhttp.Handler
}

// Platform owns reusable runtime state and exposes only stable composition ports.
type Platform struct {
	cfg      config.Config
	settings *config.PlatformSettings
	db       *sql.DB
	index    *indexing.Service
	tools    *agent.Service
	registry *tool.Registry
	reads    *tool.ReadRegistry
	graph    *codegraph.DB
	calls    *callchain.Service
	ontology ontology.Backend
	history  *sessionhistory.Service
	auth     authRuntime
	incident incidentRuntime
	agents   agentRuntime
	qa       qaState
	flow     workflowRuntime
	delivery featureDeliveryRuntime
}

// New constructs the reusable platform without registering scenario routes.
func New() (_ *Platform, err error) {
	cfg := config.Load()
	InitLogging(cfg.Log)

	db, dbErr := openPlatformDB()
	docDB := store.NewDocStore(db)
	index, err := indexing.Build(cfg, docDB, dbErr)
	if err != nil {
		if db != nil {
			_ = db.Close()
		}
		return nil, fmt.Errorf("build platform index: %w", err)
	}
	p := &Platform{cfg: cfg, db: db, index: index}
	defer func() {
		if err != nil {
			err = errors.Join(err, p.Close())
		}
	}()

	ontologyBackend, err := ontologystore.New(cfg.Ontology, index.DB)
	if err != nil {
		return nil, fmt.Errorf("build ontology backend: %w", err)
	}
	p.ontology = ontologyBackend
	index.SetOntologyPublisher(ontologyBackend)
	p.graph, err = codegraph.Open(cfg.WorkspaceRoot)
	if err != nil {
		log.Warnf("[server] codegraph call-chain disabled: %v", err)
		p.graph = nil
	}
	p.calls = callchain.New(index.DB, p.graph)
	p.tools = agent.NewTools(agent.Deps{
		DB: index.DB, Semantic: index.Semantic,
		Embedder: index.Embedder, WorkspaceRoot: cfg.WorkspaceRoot, DocStore: index.DocDB(),
		CallChain: p.calls, Ontology: ontology.NewService(ontologyBackend),
	})
	index.SetTools(p.tools)
	p.tools.SetWebSearchEngine(cfg.WebSearchEngine)
	p.tools.SetWebSearchAPIKey(cfg.WebSearchAPIKey)

	p.auth.db, p.auth.service = buildAuth(cfg, db)
	p.settings = loadPlatformSettings(p.auth.db)
	p.index.SetPlatform(p.settings)
	p.qa.sessions = memory.NewSessionStore(db)
	p.history = buildSessionHistory(cfg, p.qa.sessions, index.Embedder)
	p.registry = agent.NewRegistry(p.tools, cfg, p.qa.sessions, p.history)
	p.reads = tool.NewReadRegistry(p.registry)

	if err := p.initCatalogs(); err != nil {
		return nil, err
	}
	if err := p.initAgentWorkflow(); err != nil {
		return nil, fmt.Errorf("configure agent workflow: %w", err)
	}
	if err := p.reloadQARuntime(p.graph); err != nil {
		return nil, fmt.Errorf("configure QA runtime: %w", err)
	}
	p.initRBAC()
	if err := p.initFeatureDelivery(); err != nil {
		return nil, fmt.Errorf("configure feature delivery: %w", err)
	}
	return p, nil
}

func (p *Platform) initCatalogs() error {
	schemas, err := newSchemaRegistry()
	if err != nil {
		return err
	}
	p.agents.schemas = schemas
	p.agents.catalog = catalog.New(schemas)
	p.agents.capabilities = agentapi.NewCapabilityRegistry(
		schemas,
		p.agents.catalog,
	)
	p.agents.api = agenthttp.New(p.agents.catalog)
	p.flow.catalog = workflow.NewCatalog(schemas, p.agents.catalog)
	if p.db == nil {
		return nil
	}
	agentStore, err := catalog.NewStore(p.db)
	if err != nil {
		return fmt.Errorf("configure agent catalog store: %w", err)
	}
	if err := p.agents.catalog.AttachStore(context.Background(), agentStore); err != nil {
		return fmt.Errorf("restore agent catalog: %w", err)
	}
	p.agents.version = p.agents.catalog.MaxVersion()
	log.Infof(
		"[agent] catalog persistence enabled (restored_max_version=%d)",
		p.agents.version,
	)
	runStore, err := run.NewStore(p.db)
	if err != nil {
		log.Warnf("[qa] agent run store disabled: %v", err)
		return nil
	}
	p.qa.runs = runStore
	log.Infof("[qa] agent run store enabled (MySQL)")
	return nil
}

func newSchemaRegistry() (*agentapi.SchemaRegistry, error) {
	schemas := agentapi.NewSchemaRegistry()
	groups := []struct {
		name        string
		definitions []agentapi.SchemaDefinition
	}{
		{name: "default agent", definitions: catalog.DefaultSchemas()},
		{name: "feature pipeline", definitions: pipeline.Schemas()},
		{name: "feature review", definitions: reviewworkflow.Schemas()},
	}
	for _, group := range groups {
		if err := schemas.Publish(group.definitions); err != nil {
			return nil, fmt.Errorf("publish %s schemas: %w", group.name, err)
		}
	}
	return schemas, nil
}

func (p *Platform) initAgentWorkflow() error {
	if p.db == nil {
		log.Warnf("[workflow] persistence and execution disabled (MySQL unavailable)")
		return nil
	}
	workflowStore, err := workflow.NewStore(p.db)
	if err != nil {
		return err
	}
	if err := p.flow.catalog.AttachStore(
		context.Background(),
		workflowStore,
	); err != nil {
		return fmt.Errorf("restore workflow catalog: %w", err)
	}
	p.agents.version = max(
		p.agents.version,
		p.flow.catalog.MaxVersion(),
	)
	service, err := workflow.NewService(p.flow.catalog, workflowStore, nil)
	if err != nil {
		return err
	}
	p.flow.service = service
	p.flow.api = workflowhttp.New(service)
	log.Infof("[workflow] persistence enabled (MySQL)")
	return nil
}

// Knowledge returns the stable read-only API available to scenario tools.
func (p *Platform) Knowledge() knowledge.API { return p.tools }

// ReadTools returns the restricted publisher available to scenario code.
func (p *Platform) ReadTools() *tool.ReadRegistry { return p.reads }

// WorkspaceRoot returns the canonical workspace path established by core config.
func (p *Platform) WorkspaceRoot() string { return p.cfg.WorkspaceRoot }

// Settings returns a detached copy so scenario composition cannot mutate platform state.
func (p *Platform) Settings() config.PlatformSettings {
	p.qa.mu.RLock()
	defer p.qa.mu.RUnlock()
	settings := *p.settings
	settings.VCSGroups = append([]string(nil), p.settings.VCSGroups...)
	settings.VCSExcludeProjects = append([]string(nil), p.settings.VCSExcludeProjects...)
	settings.CodingEnabledProviders = append([]string(nil), p.settings.CodingEnabledProviders...)
	return settings
}

// Close releases reusable platform resources.
func (p *Platform) Close() error {
	if p == nil {
		return nil
	}
	if p.flow.service != nil {
		p.flow.service.Close()
	}
	if p.incident.manager != nil {
		_ = p.incident.manager.Close()
	}
	if p.calls != nil {
		_ = p.calls.Close()
	}
	if p.ontology != nil {
		_ = p.ontology.Close()
	}
	if p.history != nil {
		_ = p.history.Close()
	}
	if p.qa.memory != nil {
		_ = p.qa.memory.Close()
	}
	if p.index != nil {
		p.index.Close()
	}
	if p.db != nil {
		return p.db.Close()
	}
	return nil
}
