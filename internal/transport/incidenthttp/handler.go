package incidenthttp

import (
	"context"
	"fmt"
	"net/http"

	"github.com/dekwanlabs/nasuta/incident"
	"github.com/dekwanlabs/nasuta/internal/approval"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

// Handler exposes the platform-owned Incident and approval workflows.
type Handler struct {
	incidents   *incident.Manager
	actions     *approval.Service
	alertSecret string
}

func New(incidents *incident.Manager, actions *approval.Service, alertSecret string) *Handler {
	if incidents == nil || actions == nil {
		return nil
	}
	return &Handler{incidents: incidents, actions: actions, alertSecret: alertSecret}
}

func (handler *Handler) RegisterRoutes(api func(string, http.HandlerFunc)) {
	if handler == nil {
		return
	}
	api("POST /api/alert/webhook", handler.AlertWebhook)
	api("POST /api/alert/manual", handler.AlertManual)
	api("GET /api/incidents", handler.ListIncidents)
	api("GET /api/incidents/{id}", handler.GetIncident)
	api("DELETE /api/incidents/{id}", handler.DeleteIncident)
	api("POST /api/incidents/{id}/fix", handler.FixIncident)
	api("POST /api/incidents/{id}/confirm", handler.ConfirmIncident)
	api("GET /api/qa/actions", handler.ListActions)
	api("GET /api/qa/actions/{id}", handler.GetAction)
	api("POST /api/qa/actions/{id}", handler.DecideAction)
}

func (handler *Handler) AlertWebhook(w http.ResponseWriter, r *http.Request) {
	if handler.alertSecret != "" && r.Header.Get("X-Alert-Secret") != handler.alertSecret {
		httputil.WriteBadRequest(w, "unauthorized")
		return
	}
	var raw map[string]any
	if err := httputil.DecodeJSON(r, &raw); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	value, err := handler.incidents.CreateFromAlert(r.Context(), "alert_webhook", incident.ParseAlertPayload(raw))
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	handler.analyzeAsync(r.Context(), value.ID)
	httputil.WriteJSON(w, value)
}

func (handler *Handler) AlertManual(w http.ResponseWriter, r *http.Request) {
	var request incident.ManualAlertRequest
	if err := httputil.DecodeJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	value, err := handler.incidents.CreateFromAlert(r.Context(), "manual", incident.ParseManualAlert(request))
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	if len(request.ErrorLogs) > 0 {
		value.ErrorLogs = request.ErrorLogs
		if err := handler.incidents.SaveErrorLogs(r.Context(), value); err != nil {
			httputil.WriteErr(w, err)
			return
		}
	}
	handler.analyzeAsync(r.Context(), value.ID)
	httputil.WriteJSON(w, value)
}

func (handler *Handler) analyzeAsync(ctx context.Context, id string) {
	go func() {
		if err := handler.incidents.Analyze(context.WithoutCancel(ctx), id); err != nil {
			log.Infof("[incident] analyze %s: %v", id, err)
		}
	}()
}

func (handler *Handler) ListIncidents(w http.ResponseWriter, r *http.Request) {
	list, err := handler.incidents.List(r.Context())
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, list)
}

func (handler *Handler) GetIncident(w http.ResponseWriter, r *http.Request) {
	value, err := handler.incidents.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, value)
}

func (handler *Handler) DeleteIncident(w http.ResponseWriter, r *http.Request) {
	if err := handler.incidents.Delete(r.Context(), r.PathValue("id")); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": "deleted"})
}

func (handler *Handler) FixIncident(w http.ResponseWriter, r *http.Request) {
	var request incident.FixRequest
	if err := httputil.DecodeJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	value, err := handler.incidents.StartFix(r.Context(), r.PathValue("id"), request)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, value)
}

func (handler *Handler) ConfirmIncident(w http.ResponseWriter, r *http.Request) {
	var request incident.ConfirmRequest
	if err := httputil.DecodeJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	value, err := handler.incidents.CommitFix(r.Context(), r.PathValue("id"), request)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, value)
}

func (handler *Handler) ListActions(w http.ResponseWriter, r *http.Request) {
	query := httputil.Query(r)
	page, pageSize := query.Page(20, 200)
	if query.Err() != nil {
		httputil.WriteBadRequest(w, query.Err().Error())
		return
	}
	list, err := handler.actions.ListPage(approval.Status(query.Str("status")), page, pageSize)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, list)
}

func (handler *Handler) GetAction(w http.ResponseWriter, r *http.Request) {
	action, err := handler.actions.Get(r.PathValue("id"))
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, action)
}

func (handler *Handler) DecideAction(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := httputil.DecodeJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	user := auth.UserFromContext(r.Context())
	if user == nil {
		httputil.WriteUnauthorized(w, "approval requires authentication")
		return
	}
	if !user.IsAdmin {
		httputil.WriteErrStatus(w, http.StatusForbidden, fmt.Errorf("approval requires administrator permission"))
		return
	}
	switch request.Decision {
	case "approve":
		action, err := handler.actions.Approve(r.Context(), r.PathValue("id"), user.ID)
		if err != nil {
			httputil.WriteErr(w, err)
			return
		}
		httputil.WriteJSON(w, action)
	case "reject":
		if err := handler.actions.Reject(r.PathValue("id"), user.ID, request.Reason); err != nil {
			httputil.WriteErr(w, err)
			return
		}
		httputil.WriteJSON(w, map[string]string{"status": "rejected"})
	default:
		httputil.WriteBadRequest(w, "unknown decision: "+request.Decision)
	}
}
