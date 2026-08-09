package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
	"github.com/dekwanlabs/nasuta/internal/transport/mcp"
	"github.com/dekwanlabs/nasuta/internal/transport/routes"
	"github.com/dekwanlabs/nasuta/internal/transport/webhook"
	"github.com/dekwanlabs/nasuta/log"
)

const workflowRecoveryPageSize = 100

// RegisterCommonRoutes attaches only reusable platform routes to mux.
func (p *Platform) RegisterCommonRoutes(mux *http.ServeMux) {
	dashboardHandler := dashboard.NewHandler(
		p.index.DB, p.index.DocDB(), p.auth.db,
		p.index.Semantic, p.index.Embedder,
		p.tools, p.cfg, p.index,
		p.graph, p.calls,
		p.currentQARuntime, p.reloadQARuntime,
	)
	if p.auth.prompt != nil {
		dashboardHandler.SetRolePrompt(p.auth.prompt)
	}
	dashboardHandler.SetFeatureDeliveryStatus(p.delivery.status)
	routes.Setup(mux, routes.Config{
		Auth: p.auth.service, Dashboard: dashboardHandler, RBAC: p.auth.rbac,
		MCP:        mcp.NewDynamicHandler(p.registry),
		MCPKeyAuth: p.auth.keyAuth,
		VCS:        webhook.VCSHandler(p.index, p.settings.VCSWebhookSecret),
		Cfg:        p.cfg,
	})
	if p.incident.api != nil {
		p.incident.api.RegisterRoutes(p.AuthenticatedAPI(mux))
	}
	if p.delivery.api != nil {
		p.delivery.api.RegisterRoutes(p.AuthenticatedAPI(mux))
	}
	if p.agents.api != nil {
		p.agents.api.RegisterRoutes(p.AuthenticatedAPI(mux))
	}
	if p.flow.api != nil {
		p.flow.api.RegisterRoutes(p.AuthenticatedAPI(mux))
	}
}

// AuthenticatedAPI gives the root composition the platform auth boundary without exposing its store.
func (p *Platform) AuthenticatedAPI(mux *http.ServeMux) APIRegistrar {
	return routes.AuthenticatedAPI(mux, p.auth.service)
}

// Serve runs background platform work and serves the already-composed root mux.
func (p *Platform) Serve(ctx context.Context, mux *http.ServeMux) error {
	workflowRecoveryCutoff := time.Now().UTC()
	go p.startDailySyncTicker(ctx)
	p.delivery.start(ctx)
	if p.history != nil {
		go p.history.Run(ctx)
	}
	if p.flow.service.ExecutionAvailable() {
		go p.recoverActiveWorkflows(ctx, workflowRecoveryCutoff)
	}
	log.Infof("[server] listening on %s (MCP: /mcp, webhook: /internal/vcs-hook, api: /api)", p.cfg.HTTPAddr)
	if p.cfg.AuthToken == "" && p.auth.keyAuth == nil {
		log.Warnf("[server] WARNING: no MCP authentication configured, /mcp is unauthenticated")
	}
	if err := http.ListenAndServe(p.cfg.HTTPAddr, routes.TraceMiddleware(mux)); err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func (p *Platform) recoverActiveWorkflows(ctx context.Context, startedBefore time.Time) {
	report, err := p.flow.service.RecoverActiveWithObserver(
		ctx,
		startedBefore,
		workflowRecoveryPageSize,
		func(
			ctx context.Context,
			runID string,
			result workflow.ResumeResult,
			resumeErr error,
		) error {
			if p.flow.coordinator == nil {
				return nil
			}
			_, err := p.flow.coordinator.ReconcileRecoveredRun(
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

func (p *Platform) startDailySyncTicker(ctx context.Context) {
	atTime := p.cfg.DailySyncTime
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
		if p.auth.db == nil {
			log.WarnfCtx(ctx, "[daily-sync] auth store unavailable - skipping")
			continue
		}
		settings, err := p.auth.db.GetSettings()
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
		if err := p.index.DailySync(ctx,
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
