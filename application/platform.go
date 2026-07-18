package application

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/astris/auth"
	"github.com/dekwanlabs/astris/config"
	"github.com/dekwanlabs/astris/internal/agent"
	"github.com/dekwanlabs/astris/internal/indexing"
	"github.com/dekwanlabs/astris/internal/rbac"
	"github.com/dekwanlabs/astris/internal/transport/dashboard"
	mcptransport "github.com/dekwanlabs/astris/internal/transport/mcp"
	platformroutes "github.com/dekwanlabs/astris/internal/transport/routes"
	"github.com/dekwanlabs/astris/internal/transport/webhook"
	"github.com/dekwanlabs/astris/knowledge"
	"github.com/dekwanlabs/astris/log"
	toolruntime "github.com/dekwanlabs/astris/tool"
	"github.com/dekwanlabs/astris/writeaction"
)

// Platform owns reusable runtime state and exposes only stable composition ports.
type Platform struct {
	cfg         config.Config
	settings    *config.PlatformSettings
	index       *indexing.Service
	knowledge   *agent.Service
	registry    *toolruntime.Registry
	readTools   *toolruntime.ReadRegistry
	writeReady  bool
	authDB      *auth.DB
	authService *auth.Service
	rbacHandler *rbac.Handler
	rolePrompt  func(int64) string
}

// New constructs the reusable platform without registering scenario routes.
func New() (*Platform, error) {
	cfg := config.Load()
	InitLogging(cfg.Log)

	index, err := indexing.Build(cfg)
	if err != nil {
		return nil, fmt.Errorf("build platform index: %w", err)
	}
	knowledgeService := agent.NewTools(agent.Deps{
		DB: index.DB, Graph: index.Graph, Semantic: index.Semantic,
		Embedder: index.Embedder, WorkspaceRoot: cfg.WorkspaceRoot, DocStore: index.DocDB(),
	})
	index.SetTools(knowledgeService)
	knowledgeService.SetWebSearchEngine(cfg.WebSearchEngine)
	knowledgeService.SetWebSearchAPIKey(cfg.WebSearchAPIKey)

	authDB, authService := buildAuth(cfg)
	settings := loadPlatformSettings(authDB)
	index.SetPlatform(settings)
	registry := agent.NewRegistry(knowledgeService, cfg)

	platform := &Platform{
		cfg: cfg, settings: settings, index: index, knowledge: knowledgeService,
		registry: registry, readTools: toolruntime.NewReadRegistry(registry),
		authDB: authDB, authService: authService,
	}
	platform.initRBAC()
	return platform, nil
}

func buildAuth(cfg config.Config) (*auth.DB, *auth.Service) {
	if config.LoadMySQLDSN() == "" {
		log.Warnf("[server] auth disabled (MYSQL_DSN not set)")
		return nil, nil
	}
	authDB, err := auth.NewDB(config.LoadMySQLDSN())
	if err != nil {
		log.Warnf("[server] MySQL auth DB unavailable: %v (auth disabled)", err)
		return nil, nil
	}
	oauth := auth.NewFeishuOAuth(cfg.FeishuAppID, cfg.FeishuAppSecret)
	log.Infof("[server] auth enabled (MySQL: ok, Feishu: %v)", cfg.FeishuConfigured())
	return authDB, auth.NewService(oauth, authDB, cfg.FeishuRedirectURI, cfg.WebBaseURL)
}

func loadPlatformSettings(authDB *auth.DB) *config.PlatformSettings {
	settings := &config.PlatformSettings{}
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
	if platform.authDB == nil {
		return
	}
	store, err := rbac.NewStore(platform.authDB.RawDB())
	if err != nil {
		log.Warnf("[server] RBAC store init failed: %v", err)
		return
	}
	platform.rbacHandler = rbac.NewHandler(store)
	platform.rolePrompt = store.RolePromptFor
	log.Infof("[server] RBAC enabled")
}

// Knowledge returns the stable read-only API available to scenario tools.
func (platform *Platform) Knowledge() knowledge.API { return platform.knowledge }

// ReadTools returns the restricted publisher available to scenario code.
func (platform *Platform) ReadTools() *toolruntime.ReadRegistry { return platform.readTools }

// ConfigureWriteActions connects persistence to the closed platform catalog.
func (platform *Platform) ConfigureWriteActions(proposer writeaction.Proposer) error {
	if proposer == nil {
		return nil
	}
	if platform.writeReady {
		return fmt.Errorf("write actions are already configured")
	}
	if err := writeaction.RegisterBuiltins(platform.registry, proposer); err != nil {
		return fmt.Errorf("configure write actions: %w", err)
	}
	platform.writeReady = true
	return nil
}

// WorkspaceRoot returns the canonical workspace path established by core config.
func (platform *Platform) WorkspaceRoot() string { return platform.cfg.WorkspaceRoot }

// Settings returns a detached copy so scenario composition cannot mutate platform state.
func (platform *Platform) Settings() config.PlatformSettings {
	settings := *platform.settings
	settings.VCSGroups = append([]string(nil), platform.settings.VCSGroups...)
	settings.VCSExcludeProjects = append([]string(nil), platform.settings.VCSExcludeProjects...)
	return settings
}

// RegisterCommonRoutes attaches only reusable platform routes to mux.
func (platform *Platform) RegisterCommonRoutes(mux *http.ServeMux) {
	dashboardHandler := dashboard.NewHandler(
		platform.index.DB, platform.index.DocDB(), platform.authDB,
		platform.index.Semantic, platform.index.Embedder, platform.index.Graph,
		platform.knowledge, platform.cfg, platform.settings, platform.index,
		platform.registry, platform.writeReady,
	)
	if platform.rolePrompt != nil {
		dashboardHandler.SetRolePrompt(platform.rolePrompt)
	}
	platformroutes.Setup(mux, platformroutes.Config{
		Auth: platform.authService, Dashboard: dashboardHandler, RBAC: platform.rbacHandler,
		MCP: mcptransport.NewDynamicHandler(platform.knowledge, platform.registry),
		VCS: webhook.VCSHandler(platform.index, platform.settings.VCSWebhookSecret),
		Cfg: platform.cfg,
	})
}

// AuthenticatedAPI gives the root composition the platform auth boundary without exposing its store.
func (platform *Platform) AuthenticatedAPI(mux *http.ServeMux) platformroutes.APIRegistrar {
	return platformroutes.AuthenticatedAPI(mux, platform.authService)
}

// Serve runs background platform work and serves the already-composed root mux.
func (platform *Platform) Serve(ctx context.Context, mux *http.ServeMux) error {
	go platform.startDailySyncTicker(ctx)
	log.Infof("[server] listening on %s (MCP: /mcp, webhook: /internal/vcs-hook, api: /api)", platform.cfg.HTTPAddr)
	if platform.cfg.AuthToken == "" {
		log.Warnf("[server] WARNING: no CODELOOM_AUTH_TOKEN set, /mcp is unauthenticated")
	}
	if err := http.ListenAndServe(platform.cfg.HTTPAddr, platformroutes.TraceMiddleware(mux)); err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// Close releases reusable platform resources.
func (platform *Platform) Close() error {
	if platform == nil || platform.index == nil {
		return nil
	}
	platform.index.Close()
	return nil
}

func (platform *Platform) startDailySyncTicker(ctx context.Context) {
	atTime := strings.TrimSpace(os.Getenv("CODELOOM_DAILY_SYNC_TIME"))
	if atTime == "" {
		atTime = "02:07"
	}
	for {
		next, err := nextDailySyncAt(atTime)
		if err != nil {
			log.WarnfCtx(ctx, "[daily-sync] bad CODELOOM_DAILY_SYNC_TIME %q (%v), defaulting to 02:07", atTime, err)
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
