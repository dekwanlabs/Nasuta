package dashboard

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	"github.com/dekwanlabs/nasuta/internal/llm"
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
	DiscoverScanDirs() ([]string, error)
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
	persistentRunStore *run.Store
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
	featureStatusFn    func(context.Context) delivery.FeatureDeliveryStatus
	qaRuntimeFn        func() QARuntime
	reloadQAFn         func(*codegraph.DB) error
}

type QAInvestigationCanceller interface {
	Cancel(context.Context, string, int64) error
}

type QAInvestigationReconciler interface {
	Reconcile(context.Context, string) error
}

type QARuntime struct {
	QA                      *agent.QA
	Hub                     *run.Hub
	CompactionLLM           *llm.LLMClient
	RunStore                *run.Store
	InvestigationCanceller  QAInvestigationCanceller
	InvestigationReconciler QAInvestigationReconciler
	Sessions                *memory.SessionStore
	History                 agent.SessionHistory
	Settings                *config.PlatformSettings
	WriteAvailable          bool
}

// SetRolePrompt wires a function that returns the combined RBAC-role
// prompt fragment for a user. Injected by the server after the RBAC store is
// built (which happens after the dashboard handler). Safe to leave unset.
func (handler *Handler) SetRolePrompt(fn func(userID int64) string) {
	handler.rolePromptFn = fn
}

func (handler *Handler) SetFeatureDeliveryStatus(fn func(context.Context) delivery.FeatureDeliveryStatus) {
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
func NewHandler(db *store.SQLite, docDB *store.DocStore, authDB *auth.DB, sem semantic.Store, emb embed.Embedder, t *agent.Service, cfg config.Config, idx IndexingOps, cgDB *codegraph.DB, chain *callchain.Service, qaRuntime func() QARuntime, reloadQA func(*codegraph.DB) error) *Handler {
	h := &Handler{
		db:          db,
		docDB:       docDB,
		authDB:      authDB,
		semantic:    sem,
		embedder:    emb,
		tools:       t,
		cfg:         cfg,
		idx:         idx,
		callChain:   chain,
		qaRuntimeFn: qaRuntime,
		reloadQAFn:  reloadQA,
	}
	h.codegraphDB = cgDB
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
	if err := handler.reloadQA(cgDB); err != nil {
		return fmt.Errorf("reload QA after codegraph rebuild: %w", err)
	}
	log.Infof("[dashboard] codegraph connection enabled after rebuild")
	return nil
}
