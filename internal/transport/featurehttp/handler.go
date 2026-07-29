// Package featurehttp exposes the authenticated Feature Delivery API.
package featurehttp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

const maxPatchDownloadBytes = 20 << 20

type Handler struct {
	service *featuredelivery.Service
}

func New(service *featuredelivery.Service) *Handler {
	return &Handler{service: service}
}

func (handler *Handler) RegisterRoutes(api func(string, http.HandlerFunc)) {
	api("POST /api/features", handler.CreateFeature)
	api("GET /api/features", handler.ListFeatures)
	api("GET /api/features/{id}", handler.GetFeature)
	api("POST /api/features/{id}/requirements", handler.AddRequirement)
	api("POST /api/features/{id}/archive", handler.ArchiveFeature)

	api("GET /api/features/{id}/artifacts", handler.ListArtifacts)
	api("GET /api/features/{id}/artifacts/{artifact_id}", handler.GetArtifact)
	api("POST /api/features/{id}/artifacts/{kind}/generate", handler.GenerateArtifact)
	api("POST /api/features/{id}/artifacts/{kind}", handler.AddArtifact)
	api("POST /api/features/{id}/artifacts/{artifact_id}/review", handler.ReviewArtifact)

	api("POST /api/features/{id}/implementations", handler.CreateImplementation)
	api("GET /api/features/{id}/implementations", handler.ListImplementations)
	api("GET /api/feature-implementations/{run_id}", handler.GetImplementation)
	api("POST /api/feature-implementations/{run_id}/cancel", handler.CancelImplementation)
	api("GET /api/feature-implementations/{run_id}/events", handler.RunEvents)
	api("GET /api/feature-implementations/{run_id}/patch", handler.DownloadPatch)
	api("POST /api/feature-implementations/{run_id}/review", handler.ReviewChangeSet)
}

func (handler *Handler) CreateFeature(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	var request struct {
		Title       string                              `json:"title"`
		Requirement featuredelivery.RequirementDocument `json:"requirement"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	title, requirement, err := normalizeFeatureInput(request.Title, request.Requirement)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	feature, artifact, err := handler.service.CreateFeature(r.Context(), title, requirement, user.ID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"feature": feature, "requirement": artifact})
}

func (handler *Handler) ListFeatures(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeFeatureCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListFeatures(r.Context(), user.ID, user.IsAdmin, cursor, limit)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"items": items, "next_cursor": nextFeatureCursor(items, limit)})
}

func (handler *Handler) GetFeature(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	feature, artifacts, lineage, err := handler.service.GetFeature(r.Context(), r.PathValue("id"), user.ID, user.IsAdmin)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"feature": feature, "artifacts": artifacts, "lineage": lineage})
}

func (handler *Handler) AddRequirement(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	var requirement featuredelivery.RequirementDocument
	if err := httputil.DecodeStrictJSON(r, &requirement); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	requirement, err := normalizeRequirement(requirement)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	artifact, err := handler.service.AddRequirement(r.Context(), r.PathValue("id"), requirement, user.ID, user.IsAdmin)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, artifact)
}

func (handler *Handler) ArchiveFeature(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	if err := handler.service.ArchiveFeature(r.Context(), r.PathValue("id"), user.ID, user.IsAdmin); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": "archived"})
}

func (handler *Handler) ListArtifacts(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	_, artifacts, lineage, err := handler.service.GetFeature(r.Context(), r.PathValue("id"), user.ID, user.IsAdmin)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"items": artifacts, "lineage": lineage})
}

func (handler *Handler) GetArtifact(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	artifact, err := handler.service.GetArtifact(
		r.Context(), r.PathValue("id"), r.PathValue("artifact_id"), user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, artifact)
}

func (handler *Handler) GenerateArtifact(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	kind, err := featuredelivery.ParseArtifactKind(r.PathValue("kind"))
	if err != nil || kind == featuredelivery.KindRequirement {
		httputil.WriteBadRequest(w, "invalid generated artifact kind")
		return
	}
	artifact, run, err := handler.service.GenerateArtifact(r.Context(), r.PathValue("id"), kind, user.ID, user.IsAdmin)
	if err != nil {
		if featuredelivery.IsDomainError(err, featuredelivery.ErrUnavailable) {
			writeDomainError(w, err)
		} else {
			httputil.WriteErrStatus(w, http.StatusBadGateway, err)
		}
		return
	}
	httputil.WriteJSON(w, map[string]any{"artifact": artifact, "generation_run": run})
}

func (handler *Handler) AddArtifact(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	kind, err := featuredelivery.ParseArtifactKind(r.PathValue("kind"))
	if err != nil || kind == featuredelivery.KindRequirement {
		httputil.WriteBadRequest(w, "invalid artifact kind")
		return
	}
	var request struct {
		ParentArtifactID string          `json:"parent_artifact_id"`
		Document         json.RawMessage `json:"document"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	request.ParentArtifactID = strings.TrimSpace(request.ParentArtifactID)
	artifact, err := handler.service.AddArtifact(
		r.Context(), r.PathValue("id"), kind, request.ParentArtifactID, request.Document, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, artifact)
}

func (handler *Handler) ReviewArtifact(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	decision, comment, err := decodeReview(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	err = handler.service.ReviewArtifact(
		r.Context(), r.PathValue("id"), r.PathValue("artifact_id"), decision, comment, user.ID,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": string(decision)})
}

func (handler *Handler) CreateImplementation(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	var options featuredelivery.ImplementationOptions
	if err := httputil.DecodeStrictJSON(r, &options); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if err := normalizeImplementationOptions(&options); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	run, created, err := handler.service.CreateImplementation(r.Context(), r.PathValue("id"), options, user.ID, true)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"run": run, "created": created})
}

func (handler *Handler) ListImplementations(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeRunCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListImplementations(
		r.Context(), r.PathValue("id"), cursor, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"items": items, "next_cursor": nextRunCursor(items, limit)})
}

func (handler *Handler) GetImplementation(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	run, err := handler.service.GetImplementation(r.Context(), r.PathValue("run_id"), user.ID, user.IsAdmin)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, run)
}

func (handler *Handler) CancelImplementation(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminUser(w, r); !ok {
		return
	}
	if err := handler.service.CancelImplementation(r.Context(), r.PathValue("run_id"), true); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": "cancellation_requested"})
}

func (handler *Handler) ReviewChangeSet(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	decision, comment, err := decodeReview(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if err := handler.service.ReviewChangeSet(r.Context(), r.PathValue("run_id"), decision, comment, user.ID); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]string{"status": string(decision)})
}

func (handler *Handler) DownloadPatch(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	runID := r.PathValue("run_id")
	path, change, err := handler.service.PatchPath(r.Context(), runID, user.ID, user.IsAdmin)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeDomainError(w, featuredelivery.ErrNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != change.PatchBytes ||
		info.Size() < 0 || info.Size() > maxPatchDownloadBytes {
		httputil.WriteErr(w, fmt.Errorf("patch metadata verification failed"))
		return
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil ||
		!strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), change.PatchSHA256) {
		httputil.WriteErr(w, fmt.Errorf("patch integrity verification failed"))
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	log.InfofCtx(r.Context(), "[feature-delivery] user %d downloaded patch for run %s", user.ID, runID)
	w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+runID+`.patch"`)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, file); err != nil {
		log.WarnfCtx(r.Context(), "[feature-delivery] stream patch %s: %v", runID, err)
	}
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
		httputil.WriteErrStatus(w, http.StatusForbidden, featuredelivery.ErrForbidden)
		return nil, false
	}
	return user, true
}

func decodeReview(r *http.Request) (featuredelivery.ReviewDecision, string, error) {
	var request struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		return "", "", err
	}
	decision := featuredelivery.ReviewDecision(strings.ToLower(strings.TrimSpace(request.Decision)))
	if decision != featuredelivery.DecisionApproved && decision != featuredelivery.DecisionRejected {
		return "", "", fmt.Errorf("decision must be approved or rejected")
	}
	return decision, strings.TrimSpace(request.Comment), nil
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, featuredelivery.ErrForbidden):
		httputil.WriteErrStatus(w, http.StatusForbidden, err)
	case errors.Is(err, featuredelivery.ErrInvalid):
		httputil.WriteBadRequest(w, err.Error())
	case errors.Is(err, featuredelivery.ErrNotFound):
		httputil.WriteErrStatus(w, http.StatusNotFound, err)
	case errors.Is(err, featuredelivery.ErrConflict):
		httputil.WriteErrStatus(w, http.StatusConflict, err)
	case errors.Is(err, featuredelivery.ErrUnavailable):
		httputil.WriteErrStatus(w, http.StatusServiceUnavailable, err)
	default:
		httputil.WriteErr(w, err)
	}
}
