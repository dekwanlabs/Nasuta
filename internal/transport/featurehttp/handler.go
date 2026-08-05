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

const (
	maxPatchDownloadBytes      = 20 << 20
	maxValidationDownloadBytes = 2 << 20
)

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
	api("GET /api/features/{id}/generations", handler.ListGenerationRuns)
	api("GET /api/feature-generations/{run_id}", handler.GetGenerationRun)

	api("POST /api/features/{id}/implementations", handler.CreateImplementation)
	api("GET /api/features/{id}/implementations", handler.ListImplementations)
	api("GET /api/feature-implementations/{run_id}", handler.GetImplementation)
	api("POST /api/feature-implementations/{run_id}/cancel", handler.CancelImplementation)
	api("GET /api/feature-implementations/{run_id}/events", handler.RunEvents)
	api("GET /api/feature-implementations/{run_id}/patch", handler.DownloadPatch)
	api("GET /api/feature-implementations/{run_id}/validations/{sequence}/output", handler.DownloadValidationOutput)
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
	log.InfofCtx(r.Context(), "[feature-delivery] user %d created feature %s artifact %s", user.ID, feature.ID, artifact.ID)
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
	feature, lineage, err := handler.service.GetFeature(r.Context(), r.PathValue("id"), user.ID, user.IsAdmin)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"feature": feature, "lineage": lineage})
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
	log.InfofCtx(r.Context(), "[feature-delivery] user %d created requirement artifact %s for feature %s", user.ID, artifact.ID, r.PathValue("id"))
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
	log.InfofCtx(r.Context(), "[feature-delivery] user %d archived feature %s", user.ID, r.PathValue("id"))
	httputil.WriteJSON(w, map[string]string{"status": "archived"})
}

func (handler *Handler) ListArtifacts(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	kind, err := featuredelivery.ParseArtifactKind(r.URL.Query().Get("kind"))
	if err != nil {
		httputil.WriteBadRequest(w, "kind is required and must be a supported artifact kind")
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeArtifactCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if cursor.Kind != "" && cursor.Kind != kind {
		httputil.WriteBadRequest(w, "artifact cursor does not match kind")
		return
	}
	artifacts, lineage, err := handler.service.ListArtifacts(
		r.Context(), r.PathValue("id"), kind, cursor, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"items": artifacts, "lineage": lineage, "next_cursor": nextArtifactCursor(artifacts, limit),
	})
}

func (handler *Handler) ListGenerationRuns(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeGenerationCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListGenerationRuns(
		r.Context(), r.PathValue("id"), cursor, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"items": items, "next_cursor": nextGenerationCursor(items, limit)})
}

func (handler *Handler) GetGenerationRun(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	run, err := handler.service.GetGenerationRun(r.Context(), r.PathValue("run_id"), user.ID, user.IsAdmin)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, run)
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
		if run != nil {
			log.WarnfCtx(r.Context(), "[feature-delivery] user %d generated artifact for feature %s run=%s kind=%s status=%s error=%v", user.ID, r.PathValue("id"), run.ID, kind, run.Status, err)
		}
		if featuredelivery.IsDomainError(err, featuredelivery.ErrUnavailable) {
			writeDomainError(w, err)
		} else {
			httputil.WriteErrStatus(w, http.StatusBadGateway, err)
		}
		return
	}
	log.InfofCtx(r.Context(), "[feature-delivery] user %d generated artifact %s for feature %s run=%s kind=%s", user.ID, artifact.ID, r.PathValue("id"), run.ID, kind)
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
		BaseArtifactID   string          `json:"base_artifact_id"`
		Document         json.RawMessage `json:"document"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	request.ParentArtifactID = strings.TrimSpace(request.ParentArtifactID)
	request.BaseArtifactID = strings.TrimSpace(request.BaseArtifactID)
	artifact, err := handler.service.AddArtifact(
		r.Context(), r.PathValue("id"), kind, request.ParentArtifactID, request.BaseArtifactID, request.Document, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(r.Context(), "[feature-delivery] user %d created artifact %s for feature %s kind=%s", user.ID, artifact.ID, r.PathValue("id"), kind)
	httputil.WriteJSON(w, artifact)
}

func (handler *Handler) ReviewArtifact(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	decision, comment, binding, err := decodeReview(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	err = handler.service.ReviewArtifact(
		r.Context(), r.PathValue("id"), r.PathValue("artifact_id"), decision, comment, binding, user.ID,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(r.Context(), "[feature-delivery] user %d reviewed artifact %s for feature %s decision=%s", user.ID, r.PathValue("artifact_id"), r.PathValue("id"), decision)
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
	log.InfofCtx(r.Context(), "[feature-delivery] user %d requested implementation %s for feature %s created=%t provider=%s repo=%s", user.ID, run.ID, r.PathValue("id"), created, run.Provider, run.Repo)
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
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	if err := handler.service.CancelImplementation(r.Context(), r.PathValue("run_id"), true); err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(r.Context(), "[feature-delivery] user %d cancelled implementation %s", user.ID, r.PathValue("run_id"))
	httputil.WriteJSON(w, map[string]string{"status": "cancellation_requested"})
}

func (handler *Handler) ReviewChangeSet(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	decision, comment, binding, err := decodeReview(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if err := handler.service.ReviewChangeSet(r.Context(), r.PathValue("run_id"), decision, comment, binding, user.ID); err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(r.Context(), "[feature-delivery] user %d reviewed implementation %s decision=%s", user.ID, r.PathValue("run_id"), decision)
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
	file, size, err := openVerifiedArtifact(path, change.PatchBytes, change.PatchSHA256, maxPatchDownloadBytes)
	if err != nil {
		writeArtifactError(w, "patch", err)
		return
	}
	defer file.Close()
	log.InfofCtx(r.Context(), "[feature-delivery] user %d downloaded patch for run %s", user.ID, runID)
	w.Header().Set("Content-Type", "text/x-diff; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+runID+`.patch"`)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, file); err != nil {
		log.WarnfCtx(r.Context(), "[feature-delivery] stream patch %s: %v", runID, err)
	}
}

func (handler *Handler) DownloadValidationOutput(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	sequence, err := strconv.Atoi(r.PathValue("sequence"))
	if err != nil || sequence <= 0 {
		httputil.WriteBadRequest(w, "validation sequence must be a positive integer")
		return
	}
	runID := r.PathValue("run_id")
	path, validation, err := handler.service.ValidationOutputPath(r.Context(), runID, sequence, user.ID, user.IsAdmin)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	file, size, err := openVerifiedArtifact(path, validation.OutputBytes, validation.OutputSHA256, maxValidationDownloadBytes)
	if err != nil {
		writeArtifactError(w, "validation output", err)
		return
	}
	defer file.Close()
	log.InfofCtx(r.Context(), "[feature-delivery] user %d downloaded validation %d for run %s", user.ID, sequence, runID)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s-validation-%02d.log"`, runID, sequence))
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := io.Copy(w, file); err != nil {
		log.WarnfCtx(r.Context(), "[feature-delivery] stream validation %d for run %s: %v", sequence, runID, err)
	}
}

func openVerifiedArtifact(path string, expectedBytes int64, expectedHash string, maxBytes int64) (*os.File, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 ||
		info.Size() != expectedBytes || info.Size() > maxBytes {
		file.Close()
		return nil, 0, fmt.Errorf("metadata verification failed")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		file.Close()
		return nil, 0, fmt.Errorf("read artifact: %w", err)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedHash) {
		file.Close()
		return nil, 0, fmt.Errorf("integrity verification failed")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, 0, fmt.Errorf("rewind artifact: %w", err)
	}
	return file, info.Size(), nil
}

func writeArtifactError(w http.ResponseWriter, name string, err error) {
	if errors.Is(err, os.ErrNotExist) {
		writeDomainError(w, featuredelivery.ErrNotFound)
		return
	}
	httputil.WriteErr(w, fmt.Errorf("%s %w", name, err))
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

func decodeReview(r *http.Request) (featuredelivery.ReviewDecision, string, featuredelivery.ReviewApprovalBinding, error) {
	var request struct {
		Decision      string `json:"decision"`
		Comment       string `json:"comment"`
		SubjectHash   string `json:"subject_hash"`
		ReviewRoundID string `json:"review_round_id"`
		GateResultID  string `json:"gate_result_id"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		return "", "", featuredelivery.ReviewApprovalBinding{}, err
	}
	decision := featuredelivery.ReviewDecision(strings.ToLower(strings.TrimSpace(request.Decision)))
	if decision != featuredelivery.DecisionApproved && decision != featuredelivery.DecisionRejected {
		return "", "", featuredelivery.ReviewApprovalBinding{}, fmt.Errorf("decision must be approved or rejected")
	}
	return decision, strings.TrimSpace(request.Comment), featuredelivery.ReviewApprovalBinding{
		SubjectHash:   strings.TrimSpace(request.SubjectHash),
		ReviewRoundID: strings.TrimSpace(request.ReviewRoundID),
		GateResultID:  strings.TrimSpace(request.GateResultID),
	}, nil
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
