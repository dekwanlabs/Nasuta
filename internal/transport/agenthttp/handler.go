// Package agenthttp exposes the authenticated Agent Definition control API.
package agenthttp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

const (
	defaultPageSize = 20
	maxPageSize     = 100
)

type definitionCatalog interface {
	PublishAs(context.Context, []agentapi.Definition, int64) error
	ListRecords(context.Context, catalog.DefinitionCursor, int) ([]catalog.DefinitionRecord, error)
	SetDefault(context.Context, string, int64, int64) error
	SetActive(context.Context, string, int64, bool, int64) error
	GetRollout(string) (catalog.RolloutRule, bool)
	SetRollout(context.Context, string, int64, int, string, bool, int64) (catalog.RolloutRule, error)
	ListAudit(context.Context, string, int64, int) ([]catalog.AuditEvent, error)
	ListRolloutAudit(context.Context, string, int64, int) ([]catalog.RolloutAuditEvent, error)
}

type Handler struct {
	catalog definitionCatalog
}

func New(definitions *catalog.Catalog) *Handler {
	if definitions == nil {
		return &Handler{}
	}
	return &Handler{catalog: definitions}
}

func (handler *Handler) RegisterRoutes(api func(string, http.HandlerFunc)) {
	api("POST /api/agents", handler.Publish)
	api("GET /api/agents", handler.List)
	api("POST /api/agents/{agent_id}/versions/{version}/default", handler.SetDefault)
	api("POST /api/agents/{agent_id}/versions/{version}/status", handler.SetStatus)
	api("GET /api/agents/{agent_id}/audit", handler.ListAudit)
	api("GET /api/agents/{agent_id}/rollout", handler.GetRollout)
	api("POST /api/agents/{agent_id}/rollout", handler.SetRollout)
	api("GET /api/agents/{agent_id}/rollout/audit", handler.ListRolloutAudit)
}

func (handler *Handler) Publish(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	var request struct {
		Definitions []agentapi.Definition `json:"definitions"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.catalog == nil {
		writeDomainError(w, catalog.ErrUnavailable)
		return
	}
	if err := handler.catalog.PublishAs(r.Context(), request.Definitions, user.ID); err != nil {
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
	if handler.catalog == nil {
		writeDomainError(w, catalog.ErrUnavailable)
		return
	}
	items, err := handler.catalog.ListRecords(r.Context(), cursor, limit)
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
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	version, err := pathVersion(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.catalog == nil {
		writeDomainError(w, catalog.ErrUnavailable)
		return
	}
	id := r.PathValue("agent_id")
	if err := handler.catalog.SetDefault(r.Context(), id, version, user.ID); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"id": id, "version": version, "default": true,
	})
}

func (handler *Handler) SetStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
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
	if handler.catalog == nil {
		writeDomainError(w, catalog.ErrUnavailable)
		return
	}
	id := r.PathValue("agent_id")
	if err := handler.catalog.SetActive(
		r.Context(), id, version, request.Active, user.ID,
	); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"id": id, "version": version, "active": request.Active,
	})
}

func (handler *Handler) ListAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminUser(w, r); !ok {
		return
	}
	limit, err := requestLimit(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	afterSeq, err := afterSequence(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.catalog == nil {
		writeDomainError(w, catalog.ErrUnavailable)
		return
	}
	items, err := handler.catalog.ListAudit(
		r.Context(), r.PathValue("agent_id"), afterSeq, limit,
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
	if handler.catalog == nil {
		writeDomainError(w, catalog.ErrUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("agent_id"))
	rule, ok := handler.catalog.GetRollout(id)
	if !ok {
		writeDomainError(w, fmt.Errorf(
			"agent rollout %q not found: %w",
			id, catalog.ErrNotFound,
		))
		return
	}
	httputil.WriteJSON(w, rule)
}

func (handler *Handler) SetRollout(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
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
	if handler.catalog == nil {
		writeDomainError(w, catalog.ErrUnavailable)
		return
	}
	rule, err := handler.catalog.SetRollout(
		r.Context(),
		strings.TrimSpace(r.PathValue("agent_id")),
		request.CandidateVersion,
		request.PercentageBPS,
		request.Salt,
		request.Active,
		user.ID,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, rule)
}

func (handler *Handler) ListRolloutAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminUser(w, r); !ok {
		return
	}
	limit, err := requestLimit(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	afterSeq, err := afterSequence(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.catalog == nil {
		writeDomainError(w, catalog.ErrUnavailable)
		return
	}
	items, err := handler.catalog.ListRolloutAudit(
		r.Context(),
		strings.TrimSpace(r.PathValue("agent_id")),
		afterSeq,
		limit,
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

func authenticatedUser(w http.ResponseWriter, r *http.Request) (*auth.User, bool) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		httputil.WriteUnauthorized(w, "authentication required")
		return nil, false
	}
	return user, true
}

func adminUser(w http.ResponseWriter, r *http.Request) (*auth.User, bool) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return nil, false
	}
	if !user.IsAdmin {
		httputil.WriteErrStatus(w, http.StatusForbidden, errors.New("administrator permission required"))
		return nil, false
	}
	return user, true
}

func requestLimit(r *http.Request) (int, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return defaultPageSize, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxPageSize {
		return 0, errors.New("limit must be between 1 and 100")
	}
	return limit, nil
}

func afterSequence(r *http.Request) (int64, error) {
	value := strings.TrimSpace(r.URL.Query().Get("after_seq"))
	if value == "" {
		return 0, nil
	}
	sequence, err := strconv.ParseInt(value, 10, 64)
	if err != nil || sequence < 0 {
		return 0, errors.New("after_seq must be a non-negative integer")
	}
	return sequence, nil
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
	case errors.Is(err, catalog.ErrInvalid):
		httputil.WriteBadRequest(w, err.Error())
	case errors.Is(err, catalog.ErrNotFound):
		httputil.WriteErrStatus(w, http.StatusNotFound, err)
	case errors.Is(err, catalog.ErrConflict):
		httputil.WriteErrStatus(w, http.StatusConflict, err)
	case errors.Is(err, catalog.ErrUnavailable):
		httputil.WriteErrStatus(w, http.StatusServiceUnavailable, err)
	default:
		httputil.WriteErr(w, err)
	}
}
