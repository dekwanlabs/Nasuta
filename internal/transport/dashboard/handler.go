package dashboard

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
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
	db                 *store.SQLite
	docDB              *store.DocStore
	authDB             *auth.DB
	platformDB         *sql.DB
	semantic           semantic.Store
	embedder           embed.Embedder
	tools              *agent.Service
	qa                 *agent.QA
	persistentRunStore *agent.RunStore
	registry           *agent.Registry
	writeAvailable     bool
	codegraphDB        *codegraph.DB
	callChain          *callchain.Service
	qaSessions         *memory.SessionStore
	history            agent.SessionHistory
	cfg                config.Config
	platform           *config.PlatformSettings
	idx                IndexingOps
	rolePromptFn       func(userID int64) string
	featureStatusFn    func(context.Context) featuredelivery.FeatureDeliveryStatus
}

// SetRolePrompt wires a function that returns the combined RBAC-role
// prompt fragment for a user. Injected by the server after the RBAC store is
// built (which happens after the dashboard handler). Safe to leave unset.
func (handler *Handler) SetRolePrompt(fn func(userID int64) string) {
	handler.rolePromptFn = fn
}

func (handler *Handler) SetFeatureDeliveryStatus(fn func(context.Context) featuredelivery.FeatureDeliveryStatus) {
	handler.featureStatusFn = fn
}

// rolePromptFor returns the role prompt for a user, or "" when unresolved.
func (handler *Handler) rolePromptFor(userID int64) string {
	if handler.rolePromptFn == nil {
		return ""
	}
	return handler.rolePromptFn(userID)
}

// NewHandler builds the dashboard HTTP handler.
func NewHandler(db *store.SQLite, docDB *store.DocStore, authDB *auth.DB, platformDB *sql.DB, sem semantic.Store, emb embed.Embedder, t *agent.Service, cfg config.Config, ps *config.PlatformSettings, idx IndexingOps, registry *agent.Registry, writeAvailable bool, cgDB *codegraph.DB, chain *callchain.Service, histories ...agent.SessionHistory) *Handler {
	if ps == nil {
		ps = &config.PlatformSettings{}
	}
	runStore := openRunStore(platformDB)
	var history agent.SessionHistory
	if len(histories) > 0 {
		history = histories[0]
	}
	h := &Handler{
		db:                 db,
		docDB:              docDB,
		authDB:             authDB,
		platformDB:         platformDB,
		semantic:           sem,
		embedder:           emb,
		tools:              t,
		qa:                 agent.NewQA(agent.QADeps{Tools: t, Semantic: sem, Embedder: emb, WriteAvailable: writeAvailable, Cfg: cfg, Platform: ps, Registry: registry, CodeGraphDB: cgDB, DB: platformDB, RunStore: runStore, History: history}),
		persistentRunStore: runStore,
		registry:           registry,
		writeAvailable:     writeAvailable,
		cfg:                cfg,
		platform:           ps,
		idx:                idx,
		callChain:          chain,
		history:            history,
	}
	h.codegraphDB = cgDB
	h.qaSessions = openQASessions(platformDB)
	return h
}

func openRunStore(db *sql.DB) *agent.RunStore {
	if db == nil {
		return nil
	}
	runStore, err := agent.NewRunStore(db)
	if err != nil {
		log.Warnf("[dashboard] agent run store disabled: %v", err)
		return nil
	}
	log.Infof("[dashboard] agent run store enabled (MySQL)")
	return runStore
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

func openQASessions(db *sql.DB) *memory.SessionStore {
	if db == nil {
		return nil
	}
	qaDB := memory.NewSessionStore(db)
	log.Infof("[dashboard] qa session store enabled (MySQL)")
	return qaDB
}
