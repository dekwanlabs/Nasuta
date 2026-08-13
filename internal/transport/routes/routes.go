package routes

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/rbac"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

type MCPKeyAuthenticator func(context.Context, string) (bool, error)

// Config contains only common platform transports.
type Config struct {
	Auth       *auth.Service
	Dashboard  *dashboard.Handler
	RBAC       *rbac.Handler
	MCP        http.Handler // Streamable HTTP MCP server
	MCPKeyAuth MCPKeyAuthenticator
	VCS        http.HandlerFunc // VCS webhook handler
	Cfg        config.Config
}

func TraceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := log.WithTraceID(r.Context(), log.GenerateTraceID())
		r = r.WithContext(ctx)
		response := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(response, r)

		if response.status < http.StatusBadRequest {
			return
		}
		message := "http request failed method=%s path=%s status=%d duration=%s"
		args := []any{r.Method, r.URL.Path, response.status, time.Since(started)}
		if response.err != nil {
			message += " error=%v"
			args = append(args, response.err)
		}
		if response.status >= http.StatusInternalServerError {
			log.ErrorfCtx(ctx, message, args...)
			return
		}
		log.WarnfCtx(ctx, message, args...)
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	err         error
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseRecorder) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *responseRecorder) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *responseRecorder) RecordHTTPError(err error) {
	if w.err == nil {
		w.err = err
	}
}

func bearerAuth(token string, keyAuth MCPKeyAuthenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" && keyAuth == nil {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			httputil.WriteUnauthorized(w, "invalid or missing bearer token")
			return
		}
		candidate := strings.TrimPrefix(authHeader, "Bearer ")
		if token != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		if keyAuth != nil {
			valid, err := keyAuth(r.Context(), candidate)
			if err != nil {
				log.ErrorfCtx(r.Context(), "authenticate service key: %v", err)
				httputil.WriteServiceUnavailable(w, "authentication unavailable")
				return
			}
			if valid {
				next.ServeHTTP(w, r)
				return
			}
		}
		httputil.WriteUnauthorized(w, "invalid or missing bearer token")
	})
}

func qaAskAuth(
	authService *auth.Service,
	token string,
	keyAuth MCPKeyAuthenticator,
	next http.HandlerFunc,
) http.Handler {
	serviceAuthConfigured := token != "" || keyAuth != nil
	if authService == nil {
		if !serviceAuthConfigured {
			return next
		}
		return bearerAuth(token, keyAuth, next)
	}
	if !serviceAuthConfigured {
		return authService.RequireAuth(next)
	}

	serviceAuth := bearerAuth(token, keyAuth, next)
	return authService.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.UserFromContext(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}
		serviceAuth.ServeHTTP(w, r)
	}))
}

func Setup(mux *http.ServeMux, rc Config) {
	api := routeWithAuth(mux, rc)
	pub := routePublic(mux)
	dash := rc.Dashboard

	if rc.Auth != nil {
		pub("/auth/login", rc.Auth.Login)
		pub("/auth/callback", rc.Auth.Callback)
		pub("/auth/logout", rc.Auth.Logout)
		pub("POST /auth/register", rc.Auth.Register)
		pub("POST /auth/login/password", rc.Auth.LoginWithPassword)
		mux.Handle("/auth/me", rc.Auth.Middleware(http.HandlerFunc(rc.Auth.Me)))
	} else {
		pub("/auth/login", authDisabled("auth not configured (set MYSQL_DSN + FEISHU_APP_ID)"))
		pub("/auth/me", authDisabled("auth not configured"))
	}

	mux.Handle("/mcp", bearerAuth(rc.Cfg.AuthToken, rc.MCPKeyAuth, rc.MCP))
	pub("/internal/vcs-hook", rc.VCS)
	pub("/healthz", healthz)

	api("GET /api/summary", dash.APISummary)
	api("GET /api/services", dash.APIServices)
	api("GET /api/endpoints", dash.APIEndpoints)
	api("GET /api/edges", dash.APIEdges)
	api("GET /api/semantic/status", dash.APISemanticStatus)

	mux.Handle("POST /api/qa/ask", qaAskAuth(rc.Auth, rc.Cfg.AuthToken, rc.MCPKeyAuth, dash.APIQAAsk))
	api("GET /api/qa/runtime", dash.APIQARuntimeStatus)
	api("GET /api/qa/memories", dash.APIQAMemories)
	api("DELETE /api/qa/memories", dash.APIQAMemoriesClear)
	api("DELETE /api/qa/memories/{id}", dash.APIQAMemoryDelete)
	api("GET /api/qa/sessions", dash.APIQASessions)
	api("POST /api/qa/sessions", dash.APIQASessionSave)
	api("GET /api/qa/sessions/{id}/messages", dash.APIQASessionMessages)
	api("PUT /api/qa/sessions/{id}/message-feedback", dash.APIQAMessageFeedback)
	api("DELETE /api/qa/sessions/{id}", dash.APIQASessionDelete)
	api("GET /api/qa/runs", dash.APIQARuns)
	api("GET /api/qa/runs/{id}", dash.APIQARunGet)
	api("GET /api/qa/runs/{id}/events", dash.APIQARunEvents)
	api("GET /api/qa/tool-result-artifacts/{id}", dash.APIQAToolResultArtifact)
	api("POST /api/qa/runs/{id}", dash.APIQARunControl)
	api("GET /api/settings", dash.APISettingsGet)
	api("PUT /api/settings", dash.APISettingsPut)
	api("GET /api/projects", dash.APIProjects)
	api("GET /api/system/status", dash.APISystemStatus)

	api("POST /api/system/gitlab-sync", dash.APIGitlabSync)
	api("POST /api/system/bootstrap", dash.APIBootstrap)
	api("POST /api/system/rebuild-sql-index", dash.APIRebuildSQLIndex)
	api("POST /api/system/rebuild-codegraph", dash.APIRebuildCodeGraph)
	api("POST /api/system/embed-docs", dash.APIEmbedDocs)
	api("POST /api/system/embed-code", dash.APIEmbedCode)

	api("POST /api/system/reindex-repo", dash.APIReindexRepo)
	api("POST /api/system/embed-repo", dash.APIEmbedRepo)
	api("POST /api/system/gendocs-repo", dash.APIGendocsRepo)
	api("POST /api/system/sync-project", dash.APISyncProject)

	api("GET /api/tool/get_service", dash.ServiceLookup)
	api("GET /api/tool/search_code", dash.CodeSearch)
	api("GET /api/tool/search_runbooks", dash.RunbookSearch)
	api("GET /api/tool/trace_deps", dash.TraceDeps)
	api("GET /api/tool/list_apis", dash.ListApis)
	api("GET /api/tool/check_docs", dash.DocGapCheck)
	api("GET /api/tool/index_stats", dash.IndexSummary)
	api("GET /api/tool/get_symbol", dash.GetSymbol)
	api("GET /api/tool/trace_calls", dash.TraceCalls)

	api("GET /api/codegraph/endpoint", dash.APICodeGraphEndpoint)
	api("GET /api/codegraph/source", dash.APICodeGraphSource)

	api("GET /api/docs", dash.APIDocs)
	api("POST /api/docs", dash.APIDocUpload)
	api("GET /api/docs/template", dash.APIDocTemplate)
	api("GET /api/docs/{id}/chunks", dash.APIDocChunks)
	api("POST /api/docs/reindex", dash.APIDocsBatchReindex)
	api("GET /api/docs/{id}", dash.APIDocGet)
	api("DELETE /api/docs/{id}", dash.APIDocDelete)
	api("POST /api/docs/{id}/reindex", dash.APIDocReindex)
	api("GET /api/docs/search", dash.DocSearchTest)

	api("GET /api/knowledge", dash.APIKnowledgeList)
	api("POST /api/knowledge", dash.APIKnowledgeCreate)
	api("GET /api/knowledge/{id}", dash.APIKnowledgeGet)
	api("DELETE /api/knowledge/{id}", dash.APIKnowledgeDelete)
	api("POST /api/knowledge/{id}/reindex", dash.APIKnowledgeReindex)

	if rc.RBAC != nil {
		rb := rc.RBAC
		api("GET /api/rbac/users", rb.ListUsers)
		api("GET /api/rbac/roles", rb.ListRoles)
		api("POST /api/rbac/roles", rb.CreateRole)
		api("PUT /api/rbac/roles/{id}", rb.UpdateRole)
		api("DELETE /api/rbac/roles/{id}", rb.DeleteRole)
		api("POST /api/rbac/roles/{id}/assign", rb.AssignRole)
		api("POST /api/rbac/roles/{id}/revoke", rb.RevokeRole)
		api("GET /api/rbac/users/{id}/roles", rb.GetUserRoles)
		api("GET /api/rbac/menus/my", rb.ListMyMenus)
		api("GET /api/rbac/menus", rb.ListMenus)
		api("POST /api/rbac/menus", rb.CreateMenu)
		api("PUT /api/rbac/menus/{id}", rb.UpdateMenu)
		api("DELETE /api/rbac/menus/{id}", rb.DeleteMenu)
		api("POST /api/rbac/menus/{id}/grant", rb.GrantMenu)
		api("GET /api/rbac/menus/{id}/roles", rb.GetRoleMenus)
		api("GET /api/rbac/users/{id}/keys", rb.ListMCPKeys)
		api("POST /api/rbac/users/{id}/keys", rb.CreateMCPKey)
		api("DELETE /api/rbac/users/{id}/keys/{keyID}", rb.RevokeMCPKey)
	}
}

func routeWithAuth(mux *http.ServeMux, rc Config) func(string, http.HandlerFunc) {
	return AuthenticatedAPI(mux, rc.Auth)
}

// AuthenticatedAPI returns the common dashboard authentication boundary.
// The returned registrar matches app.APIRegistrar so the public contract is the
// single source of truth for the extension surface.
func AuthenticatedAPI(mux *http.ServeMux, authService *auth.Service) func(string, http.HandlerFunc) {
	return func(pattern string, fn http.HandlerFunc) {
		if authService != nil {
			mux.Handle(pattern, authService.RequireAuth(fn))
			return
		}
		mux.HandleFunc(pattern, fn)
	}
}

func routePublic(mux *http.ServeMux) func(string, http.HandlerFunc) {
	return func(pattern string, fn http.HandlerFunc) {
		mux.HandleFunc(pattern, fn)
	}
}

func authDisabled(msg string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		httputil.WriteServiceUnavailable(w, msg)
	}
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	httputil.WriteJSON(w, map[string]string{"status": "ok"})
}
