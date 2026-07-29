package featuredelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	maxFeatureTitle  = 512
	maxReviewComment = 8000
	maxArtifactPage  = 500
)

type Service struct {
	store             Store
	generator         *Generator
	implementations   *ImplementationManager
	generationTimeout time.Duration
	now               func() time.Time
}

func (service *Service) SetImplementationManager(manager *ImplementationManager) {
	service.implementations = manager
}

func (service *Service) CreateImplementation(ctx context.Context, requestID string, options ImplementationOptions, userID int64, admin bool) (*ImplementationRun, bool, error) {
	if !admin {
		return nil, false, ErrForbidden
	}
	feature, artifacts, lineage, err := service.GetFeature(ctx, requestID, userID, true)
	if err != nil {
		return nil, false, err
	}
	_ = artifacts
	if service.implementations == nil {
		return nil, false, ErrUnavailable
	}
	return service.implementations.Create(ctx, *feature, lineage, options, userID)
}

func (service *Service) GetImplementation(ctx context.Context, runID string, userID int64, admin bool) (*ImplementationRun, error) {
	run, err := service.store.GetImplementation(ctx, runID)
	if err != nil {
		return nil, err
	}
	if _, err := service.authorizedFeature(ctx, run.RequestID, userID, admin); err != nil {
		return nil, err
	}
	return run, nil
}

func (service *Service) ListImplementations(ctx context.Context, requestID string, cursor RunCursor, limit int, userID int64, admin bool) ([]ImplementationRun, error) {
	if _, err := service.authorizedFeature(ctx, requestID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListImplementations(ctx, requestID, cursor, limit)
}

func (service *Service) CancelImplementation(ctx context.Context, runID string, admin bool) error {
	if !admin {
		return ErrForbidden
	}
	if service.implementations == nil {
		return ErrUnavailable
	}
	return service.implementations.Cancel(ctx, runID)
}

func (service *Service) ListRunEvents(ctx context.Context, runID string, afterSeq int64, limit int, userID int64, admin bool) ([]RunEvent, error) {
	if _, err := service.GetImplementation(ctx, runID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListRunEvents(ctx, runID, afterSeq, limit)
}

func (service *Service) SubscribeRun(runID string) (<-chan RunEvent, func(), error) {
	if service.implementations == nil {
		return nil, nil, ErrUnavailable
	}
	channel, cancel := service.implementations.Subscribe(runID)
	return channel, cancel, nil
}

func NewService(store Store, generator *Generator, generationTimeout time.Duration) *Service {
	return &Service{
		store: store, generator: generator, generationTimeout: generationTimeout,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (service *Service) Status(ctx context.Context) FeatureDeliveryStatus {
	status := FeatureDeliveryStatus{
		Persistence: CapabilityStatus{Enabled: service != nil && service.store != nil},
		Generation:  CapabilityStatus{Enabled: service != nil && service.generator != nil && service.generator.Enabled()},
		Coding:      CodingCapabilityStatus{Providers: map[string]CodingProviderStatus{}},
	}
	if service != nil && service.implementations != nil {
		status.Coding = service.implementations.Status(ctx)
	}
	return status
}

func (service *Service) CreateFeature(ctx context.Context, title string, requirement RequirementDocument, userID int64) (*FeatureRequest, *Artifact, error) {
	if service == nil || service.store == nil {
		return nil, nil, ErrUnavailable
	}
	if title == "" || len(title) > maxFeatureTitle {
		return nil, nil, fmt.Errorf("title must be between 1 and %d bytes: %w", maxFeatureTitle, ErrInvalid)
	}
	requestID, err := NewID("feat")
	if err != nil {
		return nil, nil, err
	}
	raw, err := json.Marshal(requirement)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal requirement: %w", err)
	}
	artifact, err := BuildArtifact(KindRequirement, requestID, "", OriginUser, raw, nil, userID)
	if err != nil {
		return nil, nil, err
	}
	now := service.now()
	artifact.Version = 1
	artifact.CreatedAt = now
	feature := FeatureRequest{
		ID: requestID, Title: title, CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.store.CreateFeature(ctx, feature, artifact); err != nil {
		return nil, nil, err
	}
	return &feature, &artifact, nil
}

func (service *Service) GetFeature(ctx context.Context, id string, userID int64, admin bool) (*FeatureRequest, []Artifact, Lineage, error) {
	feature, err := service.authorizedFeature(ctx, id, userID, admin)
	if err != nil {
		return nil, nil, Lineage{}, err
	}
	artifacts, err := service.store.ListArtifacts(ctx, id, maxArtifactPage)
	if err != nil {
		return nil, nil, Lineage{}, err
	}
	lineage := DeriveLineage(artifacts)
	return feature, artifacts, lineage, nil
}

func (service *Service) ListFeatures(ctx context.Context, userID int64, admin bool, cursor FeatureCursor, limit int) ([]FeatureRequest, error) {
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	return service.store.ListFeatures(ctx, userID, admin, cursor, limit)
}

func (service *Service) ArchiveFeature(ctx context.Context, id string, userID int64, admin bool) error {
	if _, err := service.authorizedFeature(ctx, id, userID, admin); err != nil {
		return err
	}
	return service.store.ArchiveFeature(ctx, id, userID, admin)
}

func (service *Service) AddRequirement(ctx context.Context, requestID string, requirement RequirementDocument, userID int64, admin bool) (*Artifact, error) {
	if _, err := service.authorizedFeature(ctx, requestID, userID, admin); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(requirement)
	if err != nil {
		return nil, fmt.Errorf("marshal requirement: %w", err)
	}
	artifact, err := BuildArtifact(KindRequirement, requestID, "", OriginUser, raw, nil, userID)
	if err != nil {
		return nil, err
	}
	artifact.CreatedAt = service.now()
	return service.store.CreateArtifact(ctx, artifact)
}

func (service *Service) AddArtifact(ctx context.Context, requestID string, kind ArtifactKind, parentID string, documentJSON []byte, userID int64, admin bool) (*Artifact, error) {
	if kind == KindRequirement {
		return nil, ErrConflict
	}
	if _, err := service.authorizedFeature(ctx, requestID, userID, admin); err != nil {
		return nil, err
	}
	parent, err := service.currentParent(ctx, requestID, kind)
	if err != nil {
		return nil, err
	}
	if parentID != parent.ID {
		return nil, ErrConflict
	}
	artifact, err := BuildArtifact(kind, requestID, parent.ID, OriginUser, documentJSON, nil, userID)
	if err != nil {
		return nil, err
	}
	artifact.CreatedAt = service.now()
	return service.store.CreateArtifact(ctx, artifact)
}

func (service *Service) GenerateArtifact(ctx context.Context, requestID string, kind ArtifactKind, userID int64, admin bool) (*Artifact, *GenerationRun, error) {
	feature, err := service.authorizedFeature(ctx, requestID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	if service.generator == nil || !service.generator.Enabled() {
		return nil, nil, ErrUnavailable
	}
	parent, err := service.currentParent(ctx, requestID, kind)
	if err != nil {
		return nil, nil, err
	}
	runID, err := NewID("gen")
	if err != nil {
		return nil, nil, err
	}
	now := service.now()
	run := GenerationRun{
		ID: runID, RequestID: requestID, ArtifactKind: kind, ParentArtifactID: parent.ID,
		Status: "running", Provider: service.generator.provider, Model: service.generator.model,
		RequestedBy: userID, StartedAt: now,
	}
	if err := service.store.CreateGenerationRun(ctx, run); err != nil {
		return nil, nil, err
	}
	timeout := service.generationTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	generationCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	artifact, inputTokens, outputTokens, generationErr := service.generator.Generate(generationCtx, run.ID, *feature, *parent, kind, userID)
	if generationErr != nil {
		run.Status = "failed"
		run.InputTokens = inputTokens
		run.OutputTokens = outputTokens
		run.ErrorSummary = truncateText(generationErr.Error(), 2048)
		ended := service.now()
		run.EndedAt = &ended
		_ = service.store.FinishGenerationRun(context.WithoutCancel(ctx), run.ID, run.Status, inputTokens, outputTokens, run.ErrorSummary)
		return nil, &run, generationErr
	}
	artifact.CreatedAt = service.now()
	saved, err := service.store.CreateArtifact(ctx, artifact)
	if err != nil {
		_ = service.store.FinishGenerationRun(context.WithoutCancel(ctx), run.ID, "failed", inputTokens, outputTokens, truncateText(err.Error(), 2048))
		return nil, &run, err
	}
	run.Status = "succeeded"
	run.InputTokens = inputTokens
	run.OutputTokens = outputTokens
	ended := service.now()
	run.EndedAt = &ended
	if err := service.store.FinishGenerationRun(ctx, run.ID, run.Status, inputTokens, outputTokens, ""); err != nil {
		return nil, &run, err
	}
	return saved, &run, nil
}

func (service *Service) ReviewArtifact(ctx context.Context, requestID, artifactID string, decision ReviewDecision, comment string, reviewerID int64) error {
	if decision != DecisionApproved && decision != DecisionRejected {
		return fmt.Errorf("invalid review decision %q: %w", decision, ErrInvalid)
	}
	if len(comment) > maxReviewComment {
		return fmt.Errorf("review comment exceeds %d bytes: %w", maxReviewComment, ErrInvalid)
	}
	artifact, err := service.store.GetArtifact(ctx, artifactID)
	if err != nil {
		return err
	}
	if artifact.RequestID != requestID || artifact.Kind == KindRequirement {
		return ErrNotFound
	}
	parent, err := service.currentParent(ctx, requestID, artifact.Kind)
	if err != nil {
		return err
	}
	if artifact.ParentArtifactID != parent.ID {
		return ErrConflict
	}
	if decision == DecisionApproved {
		blocking, err := BlockingQuestions(*artifact)
		if err != nil {
			return err
		}
		if len(blocking) > 0 {
			return ErrConflict
		}
	}
	return service.store.ReviewArtifact(ctx, ArtifactReview{
		ArtifactID: artifactID, Decision: decision, Comment: comment,
		Reviewer: reviewerID, CreatedAt: service.now(),
	})
}

func (service *Service) ReviewChangeSet(ctx context.Context, runID string, decision ReviewDecision, comment string, reviewerID int64) error {
	if decision != DecisionApproved && decision != DecisionRejected {
		return fmt.Errorf("invalid review decision %q: %w", decision, ErrInvalid)
	}
	if len(comment) > maxReviewComment {
		return fmt.Errorf("review comment exceeds %d bytes: %w", maxReviewComment, ErrInvalid)
	}
	return service.store.ReviewChangeSet(ctx, ChangeReview{
		RunID: runID, Decision: decision, Comment: comment, Reviewer: reviewerID, CreatedAt: service.now(),
	})
}

func (service *Service) GetArtifact(ctx context.Context, requestID, artifactID string, userID int64, admin bool) (*Artifact, error) {
	if _, err := service.authorizedFeature(ctx, requestID, userID, admin); err != nil {
		return nil, err
	}
	artifact, err := service.store.GetArtifact(ctx, artifactID)
	if err != nil {
		return nil, err
	}
	if artifact.RequestID != requestID {
		return nil, ErrNotFound
	}
	return artifact, nil
}

func (service *Service) PatchPath(ctx context.Context, runID string, userID int64, admin bool) (string, *ChangeSet, error) {
	run, err := service.GetImplementation(ctx, runID, userID, admin)
	if err != nil {
		return "", nil, err
	}
	if run.ChangeSet == nil || service.implementations == nil {
		return "", nil, ErrNotFound
	}
	path, err := service.implementations.PatchPath(run.ChangeSet.PatchRelPath)
	if err != nil {
		return "", nil, err
	}
	return path, run.ChangeSet, nil
}

func (service *Service) authorizedFeature(ctx context.Context, id string, userID int64, admin bool) (*FeatureRequest, error) {
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	feature, err := service.store.GetFeature(ctx, id)
	if err != nil {
		return nil, err
	}
	if !admin && feature.CreatedBy != userID {
		return nil, ErrNotFound
	}
	return feature, nil
}

func (service *Service) currentParent(ctx context.Context, requestID string, kind ArtifactKind) (*Artifact, error) {
	artifacts, err := service.store.ListArtifacts(ctx, requestID, maxArtifactPage)
	if err != nil {
		return nil, err
	}
	lineage := DeriveLineage(artifacts)
	return ExpectedParent(lineage, kind)
}

func NormalizeRepository(value string) (string, error) {
	value = strings.Trim(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")), "/")
	if value == "" || strings.Contains(value, "\x00") || strings.HasPrefix(value, ".") {
		return "", fmt.Errorf("repository is required: %w", ErrInvalid)
	}
	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("invalid repository %q: %w", value, ErrInvalid)
		}
	}
	return strings.Join(parts, "/"), nil
}

func IsDomainError(err error, target error) bool {
	return errors.Is(err, target)
}
