package dashboard

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/platform/graph"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
)

// IndexingOps is the port to the indexing service for system operations.
// The concrete *indexing.Service implements this; the dashboard package
// depends on this interface rather than importing indexing directly.
type IndexingOps interface {
	CheckoutAll(ctx context.Context, vcsURL, vcsToken, vcsGroups, vcsConcurrency, vcsExcludeProjects string) ([]string, error)
	Bootstrap(ctx context.Context) error
	RebuildSQLIndex(ctx context.Context) error
	RebuildGraph(ctx context.Context) error
	ReindexRepo(ctx context.Context, repo string, commit string) error
	EmbedRepoCode(ctx context.Context, repo string) error
	GenerateDocsForRepo(ctx context.Context, repo string) error
	SyncOne(ctx context.Context, repo string) error
	EmbedDocs(ctx context.Context) error
	EmbedCodeChunks(ctx context.Context, dirs []string) error
	SetPlatform(ps *config.PlatformSettings)
	DiscoverScanDirs() []string
}

type Handler struct {
	db             *store.SQLite
	docDB          *store.DocStore
	authDB         *auth.DB
	semantic       semantic.Store
	embedder       embed.Embedder
	graph          *graph.Graph
	tools          *agent.Service
	qa             *agent.QA
	registry       *agent.Registry
	writeAvailable bool
	codegraphDB    *codegraph.DB
	callChain      *callchain.Service
	qaSessions     *memory.SessionStore
	cfg            config.Config
	platform       *config.PlatformSettings
	idx            IndexingOps
	rolePromptFn   func(userID int64) string
}

// SetRolePrompt wires a function that returns the combined RBAC-role
// prompt fragment for a user. Injected by the server after the RBAC store is
// built (which happens after the dashboard handler). Safe to leave unset.
func (handler *Handler) SetRolePrompt(fn func(userID int64) string) {
	handler.rolePromptFn = fn
}

// rolePromptFor returns the role prompt for a user, or "" when unresolved.
func (handler *Handler) rolePromptFor(userID int64) string {
	if handler.rolePromptFn == nil {
		return ""
	}
	return handler.rolePromptFn(userID)
}

// NewHandler builds the dashboard HTTP handler.
func NewHandler(db *store.SQLite, docDB *store.DocStore, authDB *auth.DB, sem semantic.Store, emb embed.Embedder, g *graph.Graph, t *agent.Service, cfg config.Config, ps *config.PlatformSettings, idx IndexingOps, registry *agent.Registry, writeAvailable bool, cgDB *codegraph.DB, chain *callchain.Service) *Handler {
	if ps == nil {
		ps = &config.PlatformSettings{}
	}
	h := &Handler{
		db:             db,
		docDB:          docDB,
		authDB:         authDB,
		semantic:       sem,
		embedder:       emb,
		graph:          g,
		tools:          t,
		qa:             agent.NewQA(agent.QADeps{Tools: t, Semantic: sem, Embedder: emb, WriteAvailable: writeAvailable, Cfg: cfg, Platform: ps, Registry: registry, CodeGraphDB: cgDB}),
		registry:       registry,
		writeAvailable: writeAvailable,
		cfg:            cfg,
		platform:       ps,
		idx:            idx,
		callChain:      chain,
	}
	h.codegraphDB = cgDB
	h.qaSessions = openQASessions(cfg)
	return h
}

func (handler *Handler) refreshCodeGraph() error {
	if handler.codegraphDB != nil {
		if err := handler.codegraphDB.Refresh(); err != nil {
			return fmt.Errorf("refresh codegraph connection: %w", err)
		}
		log.Infof("[dashboard] codegraph connection refreshed")
		return nil
	}

	cgDB, err := codegraph.Open(handler.cfg.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("open rebuilt codegraph: %w", err)
	}
	if cgDB == nil {
		return fmt.Errorf("open rebuilt codegraph: database unavailable")
	}
	handler.codegraphDB = cgDB
	if handler.callChain != nil {
		handler.callChain.SetGraph(cgDB)
	}
	handler.rebuildQA(handler.platform)
	log.Infof("[dashboard] codegraph connection enabled after rebuild")
	return nil
}

func openQASessions(cfg config.Config) *memory.SessionStore {
	if config.LoadMySQLDSN() == "" {
		log.Warnf("[dashboard] qa session store disabled (MYSQL_DSN not configured)")
		return nil
	}
	qaDB, err := memory.OpenSessionStore(config.LoadMySQLDSN())
	if err != nil {
		log.Warnf("[dashboard] qa session store disabled: %v", err)
		return nil
	}
	log.Infof("[dashboard] qa session store enabled (MySQL)")
	return qaDB
}
