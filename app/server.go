package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/feature/reviewworkflow"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
	"github.com/dekwanlabs/nasuta/internal/transport/mcp"
	"github.com/dekwanlabs/nasuta/internal/transport/routes"
	"github.com/dekwanlabs/nasuta/internal/transport/webhook"
	"github.com/dekwanlabs/nasuta/log"
)

const workflowRecoveryPageSize = 100

type recoveredWorkflowReader interface {
	GetRun(context.Context, string, int64, bool) (*workflow.RunRecord, error)
}

type recoveredReviewReconciler interface {
	ReconcileRecoveredRun(context.Context, string, workflow.RunStatus, error) error
}

// RegisterCommonRoutes attaches only reusable platform routes to mux.
func (p *Platform) RegisterCommonRoutes(mux *http.ServeMux) {
	dashboardHandler := dashboard.NewHandler(
		p.index.DB, p.index.DocDB(), p.auth.db,
		p.index.Semantic, p.index.Embedder,
		p.tools, p.cfg, p.index,
		p.graph, p.calls,
		p.currentQARuntime,
		p.applyStoredPlatformSettings,
		p.replaceQACodeGraph,
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
	p.startAgentWorkers(ctx)
	go p.startDailySyncTicker(ctx)
	p.delivery.start(ctx)
	if p.history != nil {
		go p.history.Run(ctx)
	}
	go p.recoverStartupRuns(ctx, workflowRecoveryCutoff)
	// Bind the socket before reporting that the server is listening. This makes
	// address conflicts deterministic and prevents a misleading startup log.
	listener, err := net.Listen("tcp", p.cfg.HTTPAddr)
	if err != nil {
		// Workers are started before the listener so recovery can be ready as
		// soon as Serve returns successfully. If binding fails, however, this
		// invocation must not leave child/recovery workers consuming queue work.
		p.stopAgentWorkers()
		return fmt.Errorf("http server: listen tcp %s: %w", p.cfg.HTTPAddr, err)
	}

	server := &http.Server{
		Addr:    p.cfg.HTTPAddr,
		Handler: routes.TraceMiddleware(mux),
	}
	log.Infof("[server] listening on %s (MCP: /mcp, webhook: /internal/vcs-hook, api: /api)", p.cfg.HTTPAddr)
	if p.cfg.AuthToken == "" && p.auth.keyAuth == nil {
		log.Warnf("[server] WARNING: no MCP authentication configured, /mcp is unauthenticated")
	}

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http server shutdown: %w", err)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	}
}

// recoverStartupRuns resumes work left active by an earlier process.
// Durable Workflow state is recovered before domain projections are reconciled.
// This ordering prevents owners from observing a stale terminal outcome.
func (p *Platform) recoverStartupRuns(ctx context.Context, startedBefore time.Time) {
	// Only incident and product-development workflows use the durable workflow
	// runtime. QA requests are ordinary agent runs; dynamic investigation is
	// handled by delegate_investigation inside that run and has no workflow
	// startup recovery or parent reconciliation phase.
	reviews := p.flow.coordinator
	if p.flow.service != nil && p.flow.service.Available() {
		p.recoverActiveWorkflows(ctx, startedBefore, reviews)
	} else if p.flow.service != nil {
		log.WarnfCtx(ctx, "[workflow] startup execution recovery skipped (execution unavailable)")
	}
}

func (p *Platform) recoverActiveWorkflows(
	ctx context.Context,
	startedBefore time.Time,
	reviews recoveredReviewReconciler,
) {
	report, err := p.flow.service.RecoverWithObserver(
		ctx,
		startedBefore,
		workflowRecoveryPageSize,
		func(
			ctx context.Context,
			runID string,
			result workflow.ResumeResult,
			resumeErr error,
		) error {
			return reconcileRecoveredWorkflow(
				ctx,
				p.flow.service,
				reviews,
				runID,
				resumeErr,
			)
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

func reconcileRecoveredWorkflow(
	ctx context.Context,
	workflows recoveredWorkflowReader,
	reviews recoveredReviewReconciler,
	runID string,
	resumeErr error,
) error {
	if workflows == nil {
		return fmt.Errorf("reconcile recovered workflow %q: workflow reader is unavailable", runID)
	}
	record, err := workflows.GetRun(ctx, runID, 0, true)
	if err != nil {
		return fmt.Errorf("load recovered workflow %q metadata: %w", runID, err)
	}
	switch record.Scenario {
	case reviewworkflow.ScenarioID:
		if reviews == nil {
			return fmt.Errorf("reconcile recovered review workflow %q: coordinator is unavailable", runID)
		}
		return reviews.ReconcileRecoveredRun(ctx, runID, record.Status, resumeErr)
	default:
		return fmt.Errorf(
			"recovered workflow %q has unsupported scenario %q",
			runID,
			record.Scenario,
		)
	}
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
