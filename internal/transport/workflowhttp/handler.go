// Package workflowhttp exposes the authenticated Workflow control API.
package workflowhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

type eventReader interface {
	List(context.Context, int64, int) ([]workflow.Event, error)
}

type service interface {
	PublishDefinitionsAs(context.Context, []workflow.WorkflowDefinition, int64, bool) error
	ListDefinitionRecords(context.Context, workflow.DefinitionCursor, int) ([]workflow.DefinitionRecord, error)
	SetDefinitionDefault(context.Context, string, int64, int64, bool) error
	SetDefinitionActive(context.Context, string, int64, bool, int64, bool) error
	ListDefinitionAudit(context.Context, string, int64, int, bool) ([]workflow.DefinitionAuditEvent, error)
	GetDefinitionRollout(string) (workflow.RolloutRule, bool, error)
	SetDefinitionRollout(context.Context, string, int64, int, string, bool, int64, bool) (workflow.RolloutRule, error)
	ListDefinitionRolloutAudit(context.Context, string, int64, int, bool) ([]workflow.RolloutAuditEvent, error)
	Start(context.Context, workflow.StartRequest) (*workflow.WorkflowRunRecord, error)
	GetRun(context.Context, string, int64, bool) (*workflow.WorkflowRunRecord, error)
	ListNodeRuns(context.Context, string, workflow.NodeRunCursor, int, int64, bool) ([]workflow.NodeRunRecord, error)
	ListEvents(context.Context, string, int64, int, int64, bool) ([]workflow.Event, error)
	OpenRunEvents(context.Context, string, int64, bool) (*workflow.WorkflowRunRecord, eventReader, error)
	SubscribeEvents(string) (<-chan workflow.Event, func(), error)
	ListHandoffs(context.Context, string, workflow.HandoffCursor, int, int64, bool) ([]workflow.Handoff, error)
	Cancel(context.Context, string, int64, bool) (workflow.CancelTransition, error)
	DecideHumanApproval(context.Context, workflow.ApprovalRequest) (workflow.ApprovalResult, error)
}

type serviceAdapter struct {
	service *workflow.Service
}

func (adapter serviceAdapter) PublishDefinitionsAs(
	ctx context.Context,
	definitions []workflow.WorkflowDefinition,
	actorUserID int64,
	admin bool,
) error {
	return adapter.service.PublishDefinitionsAs(ctx, definitions, actorUserID, admin)
}

func (adapter serviceAdapter) ListDefinitionRecords(
	ctx context.Context,
	cursor workflow.DefinitionCursor,
	limit int,
) ([]workflow.DefinitionRecord, error) {
	return adapter.service.ListDefinitionRecords(ctx, cursor, limit)
}

func (adapter serviceAdapter) SetDefinitionDefault(
	ctx context.Context,
	id string,
	version int64,
	actorUserID int64,
	admin bool,
) error {
	return adapter.service.SetDefinitionDefault(
		ctx, id, version, actorUserID, admin,
	)
}

func (adapter serviceAdapter) SetDefinitionActive(
	ctx context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
	admin bool,
) error {
	return adapter.service.SetDefinitionActive(
		ctx, id, version, active, actorUserID, admin,
	)
}

func (adapter serviceAdapter) ListDefinitionAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]workflow.DefinitionAuditEvent, error) {
	return adapter.service.ListDefinitionAudit(ctx, id, afterSeq, limit, admin)
}

func (adapter serviceAdapter) GetDefinitionRollout(
	id string,
) (workflow.RolloutRule, bool, error) {
	return adapter.service.GetDefinitionRollout(id)
}

func (adapter serviceAdapter) SetDefinitionRollout(
	ctx context.Context,
	id string,
	candidateVersion int64,
	percentageBPS int,
	salt string,
	active bool,
	actorUserID int64,
	admin bool,
) (workflow.RolloutRule, error) {
	return adapter.service.SetDefinitionRollout(
		ctx, id, candidateVersion, percentageBPS, salt, active, actorUserID, admin,
	)
}

func (adapter serviceAdapter) ListDefinitionRolloutAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]workflow.RolloutAuditEvent, error) {
	return adapter.service.ListDefinitionRolloutAudit(
		ctx, id, afterSeq, limit, admin,
	)
}

func (adapter serviceAdapter) Start(
	ctx context.Context,
	request workflow.StartRequest,
) (*workflow.WorkflowRunRecord, error) {
	return adapter.service.Start(ctx, request)
}

func (adapter serviceAdapter) GetRun(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*workflow.WorkflowRunRecord, error) {
	return adapter.service.GetRun(ctx, runID, userID, admin)
}

func (adapter serviceAdapter) ListNodeRuns(
	ctx context.Context,
	runID string,
	cursor workflow.NodeRunCursor,
	limit int,
	userID int64,
	admin bool,
) ([]workflow.NodeRunRecord, error) {
	return adapter.service.ListNodeRuns(ctx, runID, cursor, limit, userID, admin)
}

func (adapter serviceAdapter) ListEvents(
	ctx context.Context,
	runID string,
	afterSeq int64,
	limit int,
	userID int64,
	admin bool,
) ([]workflow.Event, error) {
	return adapter.service.ListEvents(ctx, runID, afterSeq, limit, userID, admin)
}

func (adapter serviceAdapter) OpenRunEvents(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*workflow.WorkflowRunRecord, eventReader, error) {
	run, reader, err := adapter.service.OpenRunEvents(ctx, runID, userID, admin)
	return run, reader, err
}

func (adapter serviceAdapter) SubscribeEvents(
	runID string,
) (<-chan workflow.Event, func(), error) {
	return adapter.service.SubscribeEvents(runID)
}

func (adapter serviceAdapter) ListHandoffs(
	ctx context.Context,
	runID string,
	cursor workflow.HandoffCursor,
	limit int,
	userID int64,
	admin bool,
) ([]workflow.Handoff, error) {
	return adapter.service.ListHandoffs(ctx, runID, cursor, limit, userID, admin)
}

func (adapter serviceAdapter) Cancel(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (workflow.CancelTransition, error) {
	return adapter.service.Cancel(ctx, runID, userID, admin)
}

func (adapter serviceAdapter) DecideHumanApproval(
	ctx context.Context,
	request workflow.ApprovalRequest,
) (workflow.ApprovalResult, error) {
	return adapter.service.DecideHumanApproval(ctx, request)
}

type Handler struct {
	service   service
	approvals approvalDecider
}

type approvalDecider interface {
	DecideHumanApproval(
		context.Context,
		workflow.ApprovalRequest,
	) (workflow.ApprovalResult, error)
}

func New(workflows *workflow.Service) *Handler {
	if workflows == nil {
		return &Handler{}
	}
	return &Handler{service: serviceAdapter{service: workflows}}
}

func (handler *Handler) SetApprovalDecider(decider approvalDecider) {
	handler.approvals = decider
}

func (handler *Handler) RegisterRoutes(api func(string, http.HandlerFunc)) {
	api("POST /api/workflows", handler.Publish)
	api("GET /api/workflows", handler.List)
	api("POST /api/workflows/{workflow_id}/versions/{version}/default", handler.SetDefault)
	api("POST /api/workflows/{workflow_id}/versions/{version}/status", handler.SetStatus)
	api("GET /api/workflows/{workflow_id}/audit", handler.ListAudit)
	api("GET /api/workflows/{workflow_id}/rollout", handler.GetRollout)
	api("POST /api/workflows/{workflow_id}/rollout", handler.SetRollout)
	api("GET /api/workflows/{workflow_id}/rollout/audit", handler.ListRolloutAudit)
	api("POST /api/workflows/{workflow_id}/runs", handler.Start)
	api("GET /api/workflow-runs/{run_id}", handler.GetRun)
	api("GET /api/workflow-runs/{run_id}/nodes", handler.ListNodes)
	api("GET /api/workflow-runs/{run_id}/events", handler.ListEvents)
	api("GET /api/workflow-runs/{run_id}/events/stream", handler.StreamEvents)
	api("GET /api/workflow-runs/{run_id}/handoffs", handler.ListHandoffs)
	api("POST /api/workflow-runs/{run_id}/cancel", handler.Cancel)
	api("POST /api/workflow-runs/{run_id}/nodes/{node_id}/approval", handler.Approve)
}

func (handler *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	var request struct {
		Definitions []workflow.WorkflowDefinition `json:"definitions"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	if err := handler.service.PublishDefinitionsAs(
		r.Context(), request.Definitions, user.ID, user.IsAdmin,
	); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]int{"published": len(request.Definitions)})
}

func (handler *Handler) List(w http.ResponseWriter, r *http.Request) {
	if _, ok := authenticatedUser(w, r); !ok {
		return
	}
	limit, err := requestLimit(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeDefinitionCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	items, err := handler.service.ListDefinitionRecords(
		r.Context(), cursor, limit,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	nextCursor := ""
	if len(items) == limit && len(items) > 0 {
		nextCursor = encodeDefinitionCursor(items[len(items)-1])
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_cursor": nextCursor,
	})
}

func (handler *Handler) SetDefault(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	version, err := pathVersion(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	if err := handler.service.SetDefinitionDefault(
		r.Context(), r.PathValue("workflow_id"), version, user.ID, user.IsAdmin,
	); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"id": r.PathValue("workflow_id"), "version": version, "default": true,
	})
}

func (handler *Handler) SetStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	version, err := pathVersion(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	var request struct {
		Active bool `json:"active"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	if err := handler.service.SetDefinitionActive(
		r.Context(), r.PathValue("workflow_id"), version, request.Active,
		user.ID, user.IsAdmin,
	); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"id": r.PathValue("workflow_id"), "version": version,
		"active": request.Active,
	})
}

func (handler *Handler) ListAudit(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	afterSeq, err := eventCursor(r, false)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	items, err := handler.service.ListDefinitionAudit(
		r.Context(), r.PathValue("workflow_id"), afterSeq, limit, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	nextAfterSeq := afterSeq
	if len(items) > 0 {
		nextAfterSeq = items[len(items)-1].Seq
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_after_seq": nextAfterSeq,
	})
}

func (handler *Handler) GetRollout(w http.ResponseWriter, r *http.Request) {
	if _, ok := authenticatedUser(w, r); !ok {
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("workflow_id"))
	rule, ok, err := handler.service.GetDefinitionRollout(id)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if !ok {
		writeDomainError(w, fmt.Errorf(
			"workflow rollout %q not found: %w",
			id, workflow.ErrNotFound,
		))
		return
	}
	httputil.WriteJSON(w, rule)
}

func (handler *Handler) SetRollout(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	var request struct {
		CandidateVersion int64  `json:"candidate_version"`
		PercentageBPS    int    `json:"percentage_bps"`
		Salt             string `json:"salt"`
		Active           bool   `json:"active"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	rule, err := handler.service.SetDefinitionRollout(
		r.Context(),
		strings.TrimSpace(r.PathValue("workflow_id")),
		request.CandidateVersion,
		request.PercentageBPS,
		strings.TrimSpace(request.Salt),
		request.Active,
		user.ID,
		user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, rule)
}

func (handler *Handler) ListRolloutAudit(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	afterSeq, err := eventCursor(r, false)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	items, err := handler.service.ListDefinitionRolloutAudit(
		r.Context(),
		strings.TrimSpace(r.PathValue("workflow_id")),
		afterSeq,
		limit,
		user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	nextAfterSeq := afterSeq
	if len(items) > 0 {
		nextAfterSeq = items[len(items)-1].Seq
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_after_seq": nextAfterSeq,
	})
}

func (handler *Handler) Start(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	var request struct {
		Version int64           `json:"version"`
		Input   json.RawMessage `json:"input"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if len(request.Input) == 0 {
		httputil.WriteBadRequest(w, "input is required")
		return
	}
	if request.Version < 0 {
		httputil.WriteBadRequest(w, "version must be non-negative")
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	run, err := handler.service.Start(r.Context(), workflow.StartRequest{
		Workflow: workflow.DefinitionRef{
			ID: r.PathValue("workflow_id"), Version: request.Version,
		},
		Input: request.Input,
		Actor: agentapi.Actor{UserID: user.ID},
		Admin: user.IsAdmin,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, run)
}

func (handler *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	run, err := handler.service.GetRun(
		r.Context(), r.PathValue("run_id"), user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, run)
}

func (handler *Handler) ListNodes(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeNodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	items, err := handler.service.ListNodeRuns(
		r.Context(), r.PathValue("run_id"), cursor, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	nextCursor := ""
	if len(items) == limit && len(items) > 0 {
		nextCursor = encodeNodeCursor(items[len(items)-1])
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_cursor": nextCursor,
	})
}

func (handler *Handler) ListEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	afterSeq, err := eventCursor(r, false)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	items, err := handler.service.ListEvents(
		r.Context(), r.PathValue("run_id"), afterSeq, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	nextAfterSeq := afterSeq
	if len(items) > 0 {
		nextAfterSeq = items[len(items)-1].Seq
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_after_seq": nextAfterSeq,
	})
}

func (handler *Handler) ListHandoffs(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeHandoffCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	items, err := handler.service.ListHandoffs(
		r.Context(), r.PathValue("run_id"), cursor, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	nextCursor := ""
	if len(items) == limit && len(items) > 0 {
		nextCursor = encodeHandoffCursor(items[len(items)-1])
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_cursor": nextCursor,
	})
}

func (handler *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	transition, err := handler.service.Cancel(
		r.Context(), r.PathValue("run_id"), user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, transition)
}

func (handler *Handler) Approve(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	var request struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	decision := workflow.ApprovalDecision(
		strings.ToLower(strings.TrimSpace(request.Decision)),
	)
	if decision != workflow.ApprovalApproved &&
		decision != workflow.ApprovalRejected {
		httputil.WriteBadRequest(w, "decision must be approved or rejected")
		return
	}
	if handler.service == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	approvals := handler.approvals
	if approvals == nil {
		approvals = handler.service
	}
	if approvals == nil {
		writeDomainError(w, workflow.ErrUnavailable)
		return
	}
	result, err := approvals.DecideHumanApproval(
		r.Context(),
		workflow.ApprovalRequest{
			WorkflowRunID: r.PathValue("run_id"),
			NodeID:        r.PathValue("node_id"),
			Decision:      decision,
			Approver:      agentapi.Actor{UserID: user.ID},
			Admin:         user.IsAdmin,
			Comment:       strings.TrimSpace(request.Comment),
		},
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, result)
}

func authenticatedUser(w http.ResponseWriter, r *http.Request) (*auth.User, bool) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		httputil.WriteUnauthorized(w, "authentication required")
		return nil, false
	}
	return user, true
}

func pathVersion(r *http.Request) (int64, error) {
	version, err := strconv.ParseInt(
		strings.TrimSpace(r.PathValue("version")), 10, 64,
	)
	if err != nil || version <= 0 {
		return 0, errors.New("version must be a positive integer")
	}
	return version, nil
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, workflow.ErrInvalid):
		httputil.WriteBadRequest(w, err.Error())
	case errors.Is(err, workflow.ErrForbidden):
		httputil.WriteErrStatus(w, http.StatusForbidden, err)
	case errors.Is(err, workflow.ErrNotFound):
		httputil.WriteErrStatus(w, http.StatusNotFound, err)
	case errors.Is(err, workflow.ErrConflict):
		httputil.WriteErrStatus(w, http.StatusConflict, err)
	case errors.Is(err, workflow.ErrUnavailable):
		httputil.WriteErrStatus(w, http.StatusServiceUnavailable, err)
	default:
		httputil.WriteErr(w, err)
	}
}
