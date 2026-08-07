// Package evaluationhttp exposes authenticated trace and version evaluation APIs.
package evaluationhttp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/evaluation"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

type service interface {
	WorkflowTrace(context.Context, string, int64, bool) (*evaluation.WorkflowTrace, error)
	CompareAgentVersions(context.Context, string, int64, int64, time.Time, time.Time, bool) (evaluation.Comparison[evaluation.AgentVersionMetrics], error)
	CompareWorkflowVersions(context.Context, string, int64, int64, time.Time, time.Time, bool) (evaluation.Comparison[evaluation.WorkflowVersionMetrics], error)
	CompareReviewPolicyVersions(context.Context, string, int64, int64, time.Time, time.Time, bool) (evaluation.Comparison[evaluation.ReviewPolicyVersionMetrics], error)
	CreateReviewLabels(context.Context, string, []evaluation.ReviewLabelInput, int64, bool) ([]evaluation.ReviewLabel, error)
	ListReviewLabels(context.Context, string, int64, int, bool) ([]evaluation.ReviewLabel, error)
}

type Handler struct {
	service service
}

func New(evaluations *evaluation.Service) *Handler {
	if evaluations == nil {
		return &Handler{}
	}
	return &Handler{service: evaluations}
}

func (handler *Handler) RegisterRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/evaluations/workflow-runs/{run_id}/trace", handler.WorkflowTrace)
	api("GET /api/evaluations/agents/{agent_id}/versions/compare", handler.CompareAgentVersions)
	api("GET /api/evaluations/workflows/{workflow_id}/versions/compare", handler.CompareWorkflowVersions)
	api("GET /api/evaluations/review-policies/{policy_id}/versions/compare", handler.CompareReviewPolicyVersions)
	api("POST /api/evaluations/review-rounds/{round_id}/labels", handler.CreateReviewLabels)
	api("GET /api/evaluations/review-rounds/{round_id}/labels", handler.ListReviewLabels)
}

func (handler *Handler) WorkflowTrace(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	if handler.service == nil {
		writeDomainError(w, evaluation.ErrUnavailable)
		return
	}
	runID := strings.TrimSpace(r.PathValue("run_id"))
	trace, err := handler.service.WorkflowTrace(
		r.Context(), runID, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, trace)
}

func (handler *Handler) CompareAgentVersions(w http.ResponseWriter, r *http.Request) {
	user, request, ok := comparisonRequest(w, r)
	if !ok {
		return
	}
	if handler.service == nil {
		writeDomainError(w, evaluation.ErrUnavailable)
		return
	}
	result, err := handler.service.CompareAgentVersions(
		r.Context(),
		strings.TrimSpace(r.PathValue("agent_id")),
		request.baseVersion,
		request.candidateVersion,
		request.from,
		request.to,
		user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, result)
}

func (handler *Handler) CompareWorkflowVersions(w http.ResponseWriter, r *http.Request) {
	user, request, ok := comparisonRequest(w, r)
	if !ok {
		return
	}
	if handler.service == nil {
		writeDomainError(w, evaluation.ErrUnavailable)
		return
	}
	result, err := handler.service.CompareWorkflowVersions(
		r.Context(),
		strings.TrimSpace(r.PathValue("workflow_id")),
		request.baseVersion,
		request.candidateVersion,
		request.from,
		request.to,
		user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, result)
}

func (handler *Handler) CompareReviewPolicyVersions(w http.ResponseWriter, r *http.Request) {
	user, request, ok := comparisonRequest(w, r)
	if !ok {
		return
	}
	if handler.service == nil {
		writeDomainError(w, evaluation.ErrUnavailable)
		return
	}
	result, err := handler.service.CompareReviewPolicyVersions(
		r.Context(),
		strings.TrimSpace(r.PathValue("policy_id")),
		request.baseVersion,
		request.candidateVersion,
		request.from,
		request.to,
		user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, result)
}

func (handler *Handler) CreateReviewLabels(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	var request struct {
		Labels []evaluation.ReviewLabelInput `json:"labels"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	for index := range request.Labels {
		request.Labels[index].Label = evaluation.ReviewLabelKind(strings.ToLower(
			strings.TrimSpace(string(request.Labels[index].Label)),
		))
		request.Labels[index].FindingID = strings.TrimSpace(
			request.Labels[index].FindingID,
		)
		request.Labels[index].TargetHash = strings.ToLower(strings.TrimSpace(
			request.Labels[index].TargetHash,
		))
		request.Labels[index].Category = strings.ToLower(strings.TrimSpace(
			request.Labels[index].Category,
		))
	}
	if handler.service == nil {
		writeDomainError(w, evaluation.ErrUnavailable)
		return
	}
	labels, err := handler.service.CreateReviewLabels(
		r.Context(),
		strings.TrimSpace(r.PathValue("round_id")),
		request.Labels,
		user.ID,
		user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"items": labels})
}

func (handler *Handler) ListReviewLabels(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	afterSeq, err := parseNonNegativeInt64(r.URL.Query().Get("after_seq"), 0)
	if err != nil {
		httputil.WriteBadRequest(w, "after_seq must be a non-negative integer")
		return
	}
	limit, err := parsePositiveInt(r.URL.Query().Get("limit"), 20)
	if err != nil || limit > 100 {
		httputil.WriteBadRequest(w, "limit must be between 1 and 100")
		return
	}
	if handler.service == nil {
		writeDomainError(w, evaluation.ErrUnavailable)
		return
	}
	labels, err := handler.service.ListReviewLabels(
		r.Context(),
		strings.TrimSpace(r.PathValue("round_id")),
		afterSeq,
		limit,
		user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	nextAfterSeq := afterSeq
	if len(labels) > 0 {
		nextAfterSeq = labels[len(labels)-1].Seq
	}
	httputil.WriteJSON(w, map[string]any{
		"items": labels, "next_after_seq": nextAfterSeq,
	})
}

type versionComparisonRequest struct {
	baseVersion      int64
	candidateVersion int64
	from             time.Time
	to               time.Time
}

func comparisonRequest(
	w http.ResponseWriter,
	r *http.Request,
) (*auth.User, versionComparisonRequest, bool) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return nil, versionComparisonRequest{}, false
	}
	if !user.IsAdmin {
		httputil.WriteErrStatus(
			w,
			http.StatusForbidden,
			errors.New("administrator permission required"),
		)
		return nil, versionComparisonRequest{}, false
	}
	baseVersion, err := parsePositiveInt64(r.URL.Query().Get("base_version"))
	if err != nil {
		httputil.WriteBadRequest(w, "base_version must be a positive integer")
		return nil, versionComparisonRequest{}, false
	}
	candidateVersion, err := parsePositiveInt64(
		r.URL.Query().Get("candidate_version"),
	)
	if err != nil {
		httputil.WriteBadRequest(w, "candidate_version must be a positive integer")
		return nil, versionComparisonRequest{}, false
	}
	from, err := parseOptionalTime(r.URL.Query().Get("from"))
	if err != nil {
		httputil.WriteBadRequest(w, "from must be RFC3339")
		return nil, versionComparisonRequest{}, false
	}
	to, err := parseOptionalTime(r.URL.Query().Get("to"))
	if err != nil {
		httputil.WriteBadRequest(w, "to must be RFC3339")
		return nil, versionComparisonRequest{}, false
	}
	return user, versionComparisonRequest{
		baseVersion: baseVersion, candidateVersion: candidateVersion,
		from: from, to: to,
	}, true
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
		httputil.WriteErrStatus(
			w,
			http.StatusForbidden,
			errors.New("administrator permission required"),
		)
		return nil, false
	}
	return user, true
}

func parsePositiveInt64(value string) (int64, error) {
	number, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || number <= 0 {
		return 0, errors.New("positive integer required")
	}
	return number, nil
}

func parseNonNegativeInt64(value string, fallback int64) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number < 0 {
		return 0, errors.New("non-negative integer required")
	}
	return number, nil
}

func parsePositiveInt(value string, fallback int) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 {
		return 0, errors.New("positive integer required")
	}
	return number, nil
}

func parseOptionalTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, evaluation.ErrInvalid):
		httputil.WriteBadRequest(w, err.Error())
	case errors.Is(err, evaluation.ErrNotFound):
		httputil.WriteErrStatus(w, http.StatusNotFound, err)
	case errors.Is(err, evaluation.ErrForbidden):
		httputil.WriteErrStatus(w, http.StatusForbidden, err)
	case errors.Is(err, evaluation.ErrConflict):
		httputil.WriteErrStatus(w, http.StatusConflict, err)
	case errors.Is(err, evaluation.ErrUnavailable):
		httputil.WriteErrStatus(w, http.StatusServiceUnavailable, err)
	default:
		httputil.WriteErr(w, err)
	}
}
