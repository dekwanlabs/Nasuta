// Package featurehttp exposes the authenticated Feature Delivery API.
package featurehttp

import (
	"context"
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
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	"github.com/dekwanlabs/nasuta/internal/feature/pipeline"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

const (
	maxPatchDownloadBytes      = 20 << 20
	maxValidationDownloadBytes = 2 << 20
)

type Handler struct {
	service           *delivery.Service
	pipelineStarter   pipelineStarter
	artifactReviewer  artifactReviewer
	reviewCoordinator reviewCoordinator
}

func New(service *delivery.Service) *Handler {
	return &Handler{service: service}
}

type pipelineStarter interface {
	Start(context.Context, pipeline.Request, agentapi.Actor) (*workflow.RunRecord, error)
}

type artifactReviewer interface {
	ReviewArtifact(
		context.Context,
		string,
		string,
		delivery.ReviewDecision,
		string,
		delivery.ReviewApprovalBinding,
		int64,
	) error
}

type reviewCoordinator interface {
	Execute(
		context.Context,
		string,
		agentapi.Actor,
		bool,
	) (*delivery.ReviewGateResult, error)
	Cancel(context.Context, string, agentapi.Actor, bool) error
}

func (handler *Handler) SetPipelineStarter(starter pipelineStarter) {
	handler.pipelineStarter = starter
}

func (handler *Handler) SetArtifactReviewer(reviewer artifactReviewer) {
	handler.artifactReviewer = reviewer
}

func (handler *Handler) SetReviewCoordinator(coordinator reviewCoordinator) {
	handler.reviewCoordinator = coordinator
}

func (handler *Handler) RegisterRoutes(api func(string, http.HandlerFunc)) {
	api("POST /api/features", handler.CreateFeature)
	api("GET /api/features", handler.ListFeatures)
	api("GET /api/features/{id}", handler.GetFeature)
	api("POST /api/features/{id}/requirements", handler.AddRequirement)
	api("POST /api/features/{id}/archive", handler.ArchiveFeature)
	api("POST /api/features/{id}/pipeline", handler.StartPipeline)

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

	api("POST /api/feature-delivery/review-policies", handler.PublishReviewPolicy)
	api("GET /api/feature-delivery/review-policies", handler.ListReviewPolicies)
	api("GET /api/feature-delivery/review-policies/{policy_id}/versions/{version}", handler.GetReviewPolicy)
	api("POST /api/feature-delivery/review-policies/{policy_id}/versions/{version}/default", handler.SetReviewPolicyDefault)
	api("POST /api/feature-delivery/review-policies/{policy_id}/versions/{version}/status", handler.SetReviewPolicyStatus)
	api("GET /api/feature-delivery/review-policies/{policy_id}/audit", handler.ListReviewPolicyAudit)
	api("GET /api/feature-delivery/review-policy-rollouts/{subject_kind}", handler.GetReviewPolicyRollout)
	api("POST /api/feature-delivery/review-policy-rollouts/{subject_kind}", handler.SetReviewPolicyRollout)
	api("GET /api/feature-delivery/review-policy-rollouts/{subject_kind}/audit", handler.ListReviewPolicyRolloutAudit)
	api("POST /api/feature-delivery/subjects/{type}/{subject_id}/review-rounds", handler.CreateReviewRound)
	api("GET /api/feature-delivery/review-rounds", handler.ListReviewRounds)
	api("GET /api/feature-delivery/review-rounds/{round_id}", handler.GetReviewRound)
	api("POST /api/feature-delivery/review-rounds/{round_id}/execute", handler.ExecuteReviewRound)
	api("GET /api/feature-delivery/review-rounds/{round_id}/assignments", handler.ListReviewAssignments)
	api("GET /api/feature-delivery/review-rounds/{round_id}/assignments/{assignment_id}/report", handler.GetReviewReport)
	api("GET /api/feature-delivery/review-rounds/{round_id}/findings", handler.ListReviewFindings)
	api("GET /api/feature-delivery/review-rounds/{round_id}/adjudications", handler.ListReviewAdjudications)
	api("GET /api/feature-delivery/review-rounds/{round_id}/gate", handler.GetReviewGateResult)
	api("GET /api/feature-delivery/review-rounds/{round_id}/events", handler.ListReviewEvents)
	api("GET /api/feature-delivery/review-rounds/{round_id}/events/stream", handler.StreamReviewEvents)
	api("POST /api/feature-delivery/review-rounds/{round_id}/cancel", handler.CancelReviewRound)
	api("GET /api/feature-delivery/findings/{finding_id}", handler.GetReviewFinding)
	api("GET /api/feature-delivery/findings/{finding_id}/resolutions", handler.ListFindingResolutions)
	api("POST /api/feature-delivery/findings/{finding_id}/resolutions", handler.CreateFindingResolution)
	api("POST /api/feature-delivery/findings/{finding_id}/waivers", handler.CreateFindingWaiver)
}

func (handler *Handler) StartPipeline(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	var input pipelineInput
	if err := httputil.DecodeStrictJSON(r, &input); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	request, err := normalizePipelineInput(r.PathValue("id"), input)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if handler.pipelineStarter == nil {
		writeDomainError(w, delivery.ErrUnavailable)
		return
	}
	run, err := handler.pipelineStarter.Start(
		r.Context(),
		request,
		agentapi.Actor{UserID: user.ID},
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(
		r.Context(),
		"[feature-delivery] user %d started pipeline %s for feature %s provider=%s repo=%s",
		user.ID,
		run.ID,
		request.FeatureID,
		request.Provider,
		request.Repository,
	)
	httputil.WriteJSON(w, run)
}

func (handler *Handler) CreateFeature(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	var request struct {
		Title       string                       `json:"title"`
		Requirement delivery.RequirementDocument `json:"requirement"`
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
	var requirement delivery.RequirementDocument
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
	kind, err := delivery.ParseArtifactKind(r.URL.Query().Get("kind"))
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
	kind, err := delivery.ParseArtifactKind(r.PathValue("kind"))
	if err != nil || kind == delivery.KindRequirement {
		httputil.WriteBadRequest(w, "invalid generated artifact kind")
		return
	}
	artifact, run, err := handler.service.GenerateArtifact(r.Context(), r.PathValue("id"), kind, user.ID, user.IsAdmin)
	if err != nil {
		if run != nil {
			log.WarnfCtx(r.Context(), "[feature-delivery] user %d generated artifact for feature %s run=%s kind=%s status=%s error=%v", user.ID, r.PathValue("id"), run.ID, kind, run.Status, err)
		}
		if delivery.IsDomainError(err, delivery.ErrUnavailable) {
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
	kind, err := delivery.ParseArtifactKind(r.PathValue("kind"))
	if err != nil || kind == delivery.KindRequirement {
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
	reviewer := handler.artifactReviewer
	if reviewer == nil {
		reviewer = handler.service
	}
	if reviewer == nil {
		writeDomainError(w, delivery.ErrUnavailable)
		return
	}
	err = reviewer.ReviewArtifact(
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
	var options delivery.ImplementationOptions
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

// PublishReviewPolicy creates one immutable administrator-controlled policy version.
func (handler *Handler) PublishReviewPolicy(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	var request reviewPolicyInput
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	policy, err := normalizeReviewPolicyInput(request)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	published, err := handler.service.PublishReviewPolicyAs(
		r.Context(), policy, user.ID, true,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(
		r.Context(), "[feature-delivery] user %d published review policy %s@%d",
		user.ID, published.ID, published.Version,
	)
	httputil.WriteJSON(w, published)
}

// GetReviewPolicy serves one immutable policy version to administrators.
func (handler *Handler) GetReviewPolicy(w http.ResponseWriter, r *http.Request) {
	_, ok := adminUser(w, r)
	if !ok {
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PathValue("policy_id")))
	version, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("version")), 10, 64)
	if id == "" || err != nil || version <= 0 {
		httputil.WriteBadRequest(w, "policy_id and positive version are required")
		return
	}
	policy, err := handler.service.GetReviewPolicy(r.Context(), id, version, true)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, policy)
}

func (handler *Handler) ListReviewPolicies(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminUser(w, r); !ok {
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeReviewPolicyCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListReviewPolicyRecords(
		r.Context(), cursor, limit, true,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_cursor": nextReviewPolicyCursor(items, limit),
	})
}

func (handler *Handler) SetReviewPolicyDefault(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	id, version, err := reviewPolicyPath(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if err := handler.service.SetReviewPolicyDefault(
		r.Context(), id, version, user.ID, true,
	); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"id": id, "version": version, "default": true,
	})
}

func (handler *Handler) SetReviewPolicyStatus(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	id, version, err := reviewPolicyPath(r)
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
	if err := handler.service.SetReviewPolicyActive(
		r.Context(), id, version, request.Active, user.ID, true,
	); err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"id": id, "version": version, "active": request.Active,
	})
}

func (handler *Handler) ListReviewPolicyAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminUser(w, r); !ok {
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	afterSeq, err := parseAfterSequence(r.URL.Query().Get("after_seq"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	id := strings.ToLower(strings.TrimSpace(r.PathValue("policy_id")))
	if id == "" {
		httputil.WriteBadRequest(w, "policy_id is required")
		return
	}
	items, err := handler.service.ListReviewPolicyAudit(
		r.Context(), id, afterSeq, limit, true,
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

// GetReviewPolicyRollout returns the current subject-kind rollout rule.
func (handler *Handler) GetReviewPolicyRollout(w http.ResponseWriter, r *http.Request) {
	if _, ok := authenticatedUser(w, r); !ok {
		return
	}
	kind, err := delivery.ParseSubjectKind(r.PathValue("subject_kind"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	rule, found, err := handler.service.GetReviewPolicyRollout(r.Context(), kind)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"found": found, "rollout": rule})
}

// SetReviewPolicyRollout publishes the next immutable rollout rule version.
func (handler *Handler) SetReviewPolicyRollout(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	kind, err := delivery.ParseSubjectKind(r.PathValue("subject_kind"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	var request reviewPolicyRolloutInput
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	candidateID, candidateVersion, percentageBPS, salt, active, err :=
		normalizeReviewPolicyRolloutInput(request)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	rule, err := handler.service.SetReviewPolicyRollout(
		r.Context(), kind, candidateID, candidateVersion, percentageBPS,
		salt, active, user.ID, true,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(
		r.Context(),
		"[feature-delivery] user %d set review policy rollout subject=%s candidate=%s@%d percentage_bps=%d active=%t rule_version=%d",
		user.ID, kind, rule.CandidatePolicyID, rule.CandidatePolicyVersion,
		rule.PercentageBPS, rule.Active, rule.RuleVersion,
	)
	httputil.WriteJSON(w, rule)
}

// ListReviewPolicyRolloutAudit returns the append-only rollout change history.
func (handler *Handler) ListReviewPolicyRolloutAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminUser(w, r); !ok {
		return
	}
	kind, err := delivery.ParseSubjectKind(r.PathValue("subject_kind"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	afterSeq, err := parseAfterSequence(r.URL.Query().Get("after_seq"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListReviewPolicyRolloutAudit(
		r.Context(), kind, afterSeq, limit, true,
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

// CreateReviewRound uses a published Policy reference or the server-owned default.
func (handler *Handler) CreateReviewRound(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	kind, err := delivery.ParseSubjectKind(r.PathValue("type"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	var request struct {
		PolicyID      string                              `json:"policy_id"`
		PolicyVersion int64                               `json:"policy_version"`
		ReuseReports  []delivery.ReviewReportReuseRequest `json:"reuse_reports,omitempty"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	request.PolicyID = strings.ToLower(strings.TrimSpace(request.PolicyID))
	if request.PolicyVersion < 0 ||
		(request.PolicyID == "") != (request.PolicyVersion == 0) {
		httputil.WriteBadRequest(w, "policy_id and positive policy_version must be provided together")
		return
	}
	reuseReports, err := normalizeReviewReportReuseRequests(request.ReuseReports)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	round, assignments, err := handler.service.CreateSubjectReviewRoundWithReuses(
		r.Context(), kind, strings.TrimSpace(r.PathValue("subject_id")),
		delivery.ReviewPolicyRef{ID: request.PolicyID, Version: request.PolicyVersion},
		reuseReports,
		user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(
		r.Context(),
		"[feature-delivery] user %d created review round %s subject=%s/%s policy=%s@%d",
		user.ID, round.ID, kind, round.Subject.ID, round.PolicyID, round.PolicyVersion,
	)
	httputil.WriteJSON(w, map[string]any{"round": round, "assignments": assignments})
}

// GetReviewRound serves one ownership-scoped review round.
func (handler *Handler) GetReviewRound(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	round, err := handler.service.GetReviewRound(
		r.Context(), r.PathValue("round_id"), user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, round)
}

func (handler *Handler) ListReviewRounds(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeReviewRoundCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	var filter delivery.ReviewRoundFilter
	filter.FeatureID = strings.TrimSpace(r.URL.Query().Get("feature_id"))
	if value := strings.TrimSpace(r.URL.Query().Get("subject_kind")); value != "" {
		filter.SubjectKind, err = delivery.ParseSubjectKind(value)
		if err != nil {
			httputil.WriteBadRequest(w, err.Error())
			return
		}
	}
	filter.SubjectID = strings.TrimSpace(r.URL.Query().Get("subject_id"))
	filter.Status = delivery.ReviewRoundStatus(strings.ToLower(
		strings.TrimSpace(r.URL.Query().Get("status")),
	))
	if filter.Status != "" && !validReviewRoundStatus(filter.Status) {
		httputil.WriteBadRequest(w, "status is invalid")
		return
	}
	items, hasMore, err := handler.service.ListReviewRounds(
		r.Context(), filter, cursor, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_cursor": nextReviewRoundCursor(items, hasMore),
	})
}

// ExecuteReviewRound restricts reviewer execution to administrators.
func (handler *Handler) ExecuteReviewRound(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	if handler.reviewCoordinator == nil {
		writeDomainError(w, delivery.ErrUnavailable)
		return
	}
	result, err := handler.reviewCoordinator.Execute(
		r.Context(), r.PathValue("round_id"), agentapi.Actor{UserID: user.ID}, true,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(
		r.Context(), "[feature-delivery] user %d executed review round %s decision=%s",
		user.ID, r.PathValue("round_id"), result.Decision,
	)
	httputil.WriteJSON(w, result)
}

// ListReviewAssignments serves a bounded assignment page.
func (handler *Handler) ListReviewAssignments(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r, 16, 16)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeReviewAssignmentCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListReviewAssignments(
		r.Context(), r.PathValue("round_id"), cursor, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_cursor": nextReviewAssignmentCursor(items, limit),
	})
}

// GetReviewReport serves one immutable assignment report.
func (handler *Handler) GetReviewReport(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	report, err := handler.service.GetReviewReport(
		r.Context(),
		r.PathValue("round_id"),
		r.PathValue("assignment_id"),
		user.ID,
		user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, report)
}

// ListReviewFindings serves bounded summaries with an optional severity filter.
func (handler *Handler) ListReviewFindings(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeFindingCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	severity, err := delivery.ParseSeverity(r.URL.Query().Get("severity"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListReviewFindings(
		r.Context(), r.PathValue("round_id"), severity, cursor, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_cursor": nextFindingCursor(items, limit),
	})
}

// ListReviewAdjudications serves immutable conflict decisions for one round.
func (handler *Handler) ListReviewAdjudications(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeReviewAdjudicationCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListReviewAdjudications(
		r.Context(), r.PathValue("round_id"), cursor, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_cursor": nextReviewAdjudicationCursor(items, limit),
	})
}

// GetReviewGateResult serves the immutable Gate for one authorized round.
func (handler *Handler) GetReviewGateResult(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	result, err := handler.service.GetReviewGateResult(
		r.Context(), r.PathValue("round_id"), user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, result)
}

// ListReviewEvents serves a bounded durable event page.
func (handler *Handler) ListReviewEvents(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	afterSeq, err := reviewEventAfterSeq(r)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	limit, err := requestLimit(r, 100, 500)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListReviewEvents(
		r.Context(), r.PathValue("round_id"), afterSeq, limit, user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"items": items})
}

// CancelReviewRound restricts persisted cancellation to administrators.
func (handler *Handler) CancelReviewRound(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	if handler.reviewCoordinator == nil {
		writeDomainError(w, delivery.ErrUnavailable)
		return
	}
	if err := handler.reviewCoordinator.Cancel(
		r.Context(),
		r.PathValue("round_id"),
		agentapi.Actor{UserID: user.ID},
		true,
	); err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(
		r.Context(), "[feature-delivery] user %d cancelled review round %s",
		user.ID, r.PathValue("round_id"),
	)
	httputil.WriteJSON(w, map[string]string{"status": "cancelled"})
}

// GetReviewFinding serves one finding with bounded evidence.
func (handler *Handler) GetReviewFinding(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	finding, err := handler.service.GetReviewFinding(
		r.Context(), r.PathValue("finding_id"), user.ID, user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, finding)
}

// ListFindingResolutions serves the bounded lifecycle audit trail.
func (handler *Handler) ListFindingResolutions(w http.ResponseWriter, r *http.Request) {
	user, ok := authenticatedUser(w, r)
	if !ok {
		return
	}
	limit, err := requestLimit(r, 20, 100)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	cursor, err := decodeFindingResolutionCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	items, err := handler.service.ListFindingResolutions(
		r.Context(),
		r.PathValue("finding_id"),
		cursor,
		limit,
		user.ID,
		user.IsAdmin,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"items": items, "next_cursor": nextFindingResolutionCursor(items, limit),
	})
}

// CreateFindingResolution records one administrator-authored lifecycle fact.
func (handler *Handler) CreateFindingResolution(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	var request struct {
		Resolution      string     `json:"resolution"`
		SubjectHash     string     `json:"subject_hash"`
		ReplacementHash string     `json:"replacement_hash"`
		Rationale       string     `json:"rationale"`
		ExpiresAt       *time.Time `json:"expires_at"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	resolutionKind, err := delivery.ParseFindingResolutionKind(request.Resolution)
	if err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	resolution, err := handler.service.CreateFindingResolution(
		r.Context(),
		r.PathValue("finding_id"),
		delivery.FindingResolutionRequest{
			Resolution:      resolutionKind,
			SubjectHash:     strings.ToLower(strings.TrimSpace(request.SubjectHash)),
			ReplacementHash: strings.ToLower(strings.TrimSpace(request.ReplacementHash)),
			Rationale:       strings.TrimSpace(request.Rationale),
			ExpiresAt:       request.ExpiresAt,
		},
		user.ID,
		true,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(
		r.Context(), "[feature-delivery] user %d resolved review finding %s as %s resolution=%s",
		user.ID, r.PathValue("finding_id"), resolution.Resolution, resolution.ID,
	)
	httputil.WriteJSON(w, resolution)
}

// CreateFindingWaiver records an administrator's Subject-bound exception.
func (handler *Handler) CreateFindingWaiver(w http.ResponseWriter, r *http.Request) {
	user, ok := adminUser(w, r)
	if !ok {
		return
	}
	var request struct {
		SubjectHash string     `json:"subject_hash"`
		Rationale   string     `json:"rationale"`
		ExpiresAt   *time.Time `json:"expires_at"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	request.SubjectHash = strings.ToLower(strings.TrimSpace(request.SubjectHash))
	request.Rationale = strings.TrimSpace(request.Rationale)
	resolution, err := handler.service.CreateFindingWaiver(
		r.Context(), r.PathValue("finding_id"), request.SubjectHash,
		request.Rationale, request.ExpiresAt, user.ID, true,
	)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	log.InfofCtx(
		r.Context(), "[feature-delivery] user %d waived review finding %s resolution=%s",
		user.ID, r.PathValue("finding_id"), resolution.ID,
	)
	httputil.WriteJSON(w, resolution)
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
		writeDomainError(w, delivery.ErrNotFound)
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
		httputil.WriteErrStatus(w, http.StatusForbidden, delivery.ErrForbidden)
		return nil, false
	}
	return user, true
}

func decodeReview(r *http.Request) (delivery.ReviewDecision, string, delivery.ReviewApprovalBinding, error) {
	var request struct {
		Decision      string `json:"decision"`
		Comment       string `json:"comment"`
		SubjectHash   string `json:"subject_hash"`
		ReviewRoundID string `json:"review_round_id"`
		GateResultID  string `json:"gate_result_id"`
	}
	if err := httputil.DecodeStrictJSON(r, &request); err != nil {
		return "", "", delivery.ReviewApprovalBinding{}, err
	}
	decision := delivery.ReviewDecision(strings.ToLower(strings.TrimSpace(request.Decision)))
	if decision != delivery.DecisionApproved && decision != delivery.DecisionRejected {
		return "", "", delivery.ReviewApprovalBinding{}, fmt.Errorf("decision must be approved or rejected")
	}
	return decision, strings.TrimSpace(request.Comment), delivery.ReviewApprovalBinding{
		SubjectHash:   strings.TrimSpace(request.SubjectHash),
		ReviewRoundID: strings.TrimSpace(request.ReviewRoundID),
		GateResultID:  strings.TrimSpace(request.GateResultID),
	}, nil
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, delivery.ErrForbidden),
		errors.Is(err, workflow.ErrForbidden):
		httputil.WriteErrStatus(w, http.StatusForbidden, err)
	case errors.Is(err, delivery.ErrInvalid),
		errors.Is(err, workflow.ErrInvalid):
		httputil.WriteBadRequest(w, err.Error())
	case errors.Is(err, delivery.ErrNotFound),
		errors.Is(err, workflow.ErrNotFound):
		httputil.WriteErrStatus(w, http.StatusNotFound, err)
	case errors.Is(err, delivery.ErrConflict),
		errors.Is(err, workflow.ErrConflict):
		httputil.WriteErrStatus(w, http.StatusConflict, err)
	case errors.Is(err, delivery.ErrUnavailable),
		errors.Is(err, workflow.ErrUnavailable):
		httputil.WriteErrStatus(w, http.StatusServiceUnavailable, err)
	default:
		httputil.WriteErr(w, err)
	}
}
