package featuredelivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"
)

const (
	maxFeatureTitle  = 512
	maxReviewComment = 8000
	maxRepository    = 512
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
	feature, lineage, err := service.GetFeature(ctx, requestID, userID, true)
	if err != nil {
		return nil, false, err
	}
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
	_, reader, err := service.OpenRunEvents(ctx, runID, userID, admin)
	if err != nil {
		return nil, err
	}
	return reader.List(ctx, afterSeq, limit)
}

// RunEventReader scopes repeated event reads to one authorized run.
type RunEventReader struct {
	store Store
	runID string
}

// OpenRunEvents authorizes once before a bounded replay or live stream.
func (service *Service) OpenRunEvents(ctx context.Context, runID string, userID int64, admin bool) (*ImplementationRun, *RunEventReader, error) {
	run, err := service.GetImplementation(ctx, runID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	return run, &RunEventReader{store: service.store, runID: runID}, nil
}

func (reader *RunEventReader) List(ctx context.Context, afterSeq int64, limit int) ([]RunEvent, error) {
	if reader == nil || reader.store == nil {
		return nil, ErrUnavailable
	}
	return reader.store.ListRunEvents(ctx, reader.runID, afterSeq, limit)
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

func (service *Service) GetFeature(ctx context.Context, id string, userID int64, admin bool) (*FeatureRequest, Lineage, error) {
	feature, err := service.authorizedFeature(ctx, id, userID, admin)
	if err != nil {
		return nil, Lineage{}, err
	}
	lineage, err := service.store.GetCurrentLineage(ctx, id)
	if err != nil {
		return nil, Lineage{}, err
	}
	return feature, lineage, nil
}

func (service *Service) ListArtifacts(ctx context.Context, requestID string, kind ArtifactKind, cursor ArtifactCursor, limit int, userID int64, admin bool) ([]ArtifactSummary, Lineage, error) {
	if _, err := service.authorizedFeature(ctx, requestID, userID, admin); err != nil {
		return nil, Lineage{}, err
	}
	items, err := service.store.ListArtifacts(ctx, requestID, kind, cursor, limit)
	if err != nil {
		return nil, Lineage{}, err
	}
	lineage, err := service.store.GetCurrentLineage(ctx, requestID)
	if err != nil {
		return nil, Lineage{}, err
	}
	markStaleSummaries(items, lineage)
	return items, lineage, nil
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

func (service *Service) AddArtifact(ctx context.Context, requestID string, kind ArtifactKind, parentID, baseArtifactID string, documentJSON []byte, userID int64, admin bool) (*Artifact, error) {
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
	var evidence []EvidenceRef
	if baseArtifactID != "" {
		base, err := service.store.GetArtifact(ctx, baseArtifactID)
		if err != nil {
			return nil, err
		}
		if base.RequestID != requestID || base.Kind != kind || base.ParentArtifactID != parent.ID {
			return nil, ErrConflict
		}
		evidence = base.Evidence
	}
	artifact, err := BuildArtifact(kind, requestID, parent.ID, OriginUser, documentJSON, evidence, userID)
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
		if finishErr := service.store.FinishGenerationRun(context.WithoutCancel(ctx), run.ID, run.Status, inputTokens, outputTokens, run.ErrorSummary); finishErr != nil {
			generationErr = errors.Join(generationErr, fmt.Errorf("persist failed generation %q: %w", run.ID, finishErr))
		}
		return nil, &run, generationErr
	}
	artifact.CreatedAt = service.now()
	saved, err := service.store.CompleteGeneration(generationCtx, run.ID, artifact, inputTokens, outputTokens)
	if err != nil {
		run.Status = "failed"
		run.InputTokens = inputTokens
		run.OutputTokens = outputTokens
		run.ErrorSummary = truncateText(err.Error(), 2048)
		ended := service.now()
		run.EndedAt = &ended
		if finishErr := service.store.FinishGenerationRun(context.WithoutCancel(ctx), run.ID, run.Status, inputTokens, outputTokens, run.ErrorSummary); finishErr != nil {
			err = errors.Join(err, fmt.Errorf("persist failed generation %q: %w", run.ID, finishErr))
		}
		return nil, &run, err
	}
	run.Status = "succeeded"
	run.InputTokens = inputTokens
	run.OutputTokens = outputTokens
	ended := service.now()
	run.EndedAt = &ended
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

func (service *Service) GetGenerationRun(ctx context.Context, runID string, userID int64, admin bool) (*GenerationRun, error) {
	run, err := service.store.GetGenerationRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if _, err := service.authorizedFeature(ctx, run.RequestID, userID, admin); err != nil {
		return nil, err
	}
	return run, nil
}

func (service *Service) ListGenerationRuns(ctx context.Context, requestID string, cursor GenerationCursor, limit int, userID int64, admin bool) ([]GenerationRun, error) {
	if _, err := service.authorizedFeature(ctx, requestID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListGenerationRuns(ctx, requestID, cursor, limit)
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

func (service *Service) ValidationOutputPath(ctx context.Context, runID string, sequence int, userID int64, admin bool) (string, *ValidationResult, error) {
	if sequence <= 0 {
		return "", nil, ErrInvalid
	}
	run, err := service.GetImplementation(ctx, runID, userID, admin)
	if err != nil {
		return "", nil, err
	}
	if run.ChangeSet == nil || service.implementations == nil {
		return "", nil, ErrNotFound
	}
	for index := range run.ChangeSet.ValidationResults {
		result := &run.ChangeSet.ValidationResults[index]
		if result.Sequence != sequence {
			continue
		}
		if result.OutputRelPath == "" || result.OutputSHA256 == "" || result.OutputBytes < 0 {
			return "", nil, ErrNotFound
		}
		path, err := service.implementations.ValidationOutputPath(result.OutputRelPath)
		if err != nil {
			return "", nil, err
		}
		return path, result, nil
	}
	return "", nil, ErrNotFound
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
	lineage, err := service.store.GetCurrentLineage(ctx, requestID)
	if err != nil {
		return nil, err
	}
	return ExpectedParent(lineage, kind)
}

func markStaleSummaries(items []ArtifactSummary, lineage Lineage) {
	current := make(map[string]struct{}, 5)
	for _, artifact := range []*Artifact{
		lineage.Requirement, lineage.RequirementAnalysis, lineage.TechnicalProposal,
		lineage.SystemDesign, lineage.ImplementationPlan,
	} {
		if artifact != nil {
			current[artifact.ID] = struct{}{}
		}
	}
	for index := range items {
		_, active := current[items[index].ID]
		items[index].Stale = !active
	}
}

func NormalizeRepository(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("repository is required: %w", ErrInvalid)
	}
	if len(value) > maxRepository || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return "", fmt.Errorf("invalid repository %q: %w", value, ErrInvalid)
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return "", fmt.Errorf("invalid repository %q: %w", value, ErrInvalid)
		}
	}
	value = strings.TrimRight(value, "/")
	if value == "" || path.IsAbs(value) || path.Clean(value) != value || strings.HasPrefix(value, ".") {
		return "", fmt.Errorf("invalid repository %q: %w", value, ErrInvalid)
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
