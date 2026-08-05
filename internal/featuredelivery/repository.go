package featuredelivery

import (
	"context"
	"time"
)

type FeatureCursor struct {
	UpdatedAt time.Time
	ID        string
}

type ArtifactCursor struct {
	Kind    ArtifactKind
	Version int
}

type GenerationCursor struct {
	StartedAt time.Time
	ID        string
}

type RunCursor struct {
	CreatedAt time.Time
	ID        string
}

type Store interface {
	CreateFeature(context.Context, FeatureRequest, Artifact) error
	GetFeature(context.Context, string) (*FeatureRequest, error)
	ListFeatures(context.Context, int64, bool, FeatureCursor, int) ([]FeatureRequest, error)
	ArchiveFeature(context.Context, string, int64, bool) error
	TouchFeature(context.Context, string) error

	CreateArtifact(context.Context, Artifact) (*Artifact, error)
	GetArtifact(context.Context, string) (*Artifact, error)
	ListArtifacts(context.Context, string, ArtifactKind, ArtifactCursor, int) ([]ArtifactSummary, error)
	GetCurrentLineage(context.Context, string) (Lineage, error)
	ReviewArtifact(context.Context, ArtifactReview) error

	CreateGenerationRun(context.Context, GenerationRun) error
	CompleteGeneration(context.Context, string, Artifact, int64, int64) (*Artifact, error)
	GetGenerationRun(context.Context, string) (*GenerationRun, error)
	ListGenerationRuns(context.Context, string, GenerationCursor, int) ([]GenerationRun, error)
	FinishGenerationRun(context.Context, string, string, int64, int64, string) error
	InterruptGenerationRuns(context.Context) error

	GetOwnerIdentity(context.Context, int64) (OwnerIdentity, error)
	GetUserWorkspace(context.Context, int64) (*UserWorkspace, error)
	CreateUserWorkspace(context.Context, UserWorkspace) error
	GetUserWorkspaceByKey(context.Context, string) (*UserWorkspace, error)

	CreateImplementation(context.Context, ImplementationRun) (*ImplementationRun, bool, error)
	GetImplementation(context.Context, string) (*ImplementationRun, error)
	ListImplementations(context.Context, string, RunCursor, int) ([]ImplementationRun, error)
	ClaimNextImplementation(context.Context, string, time.Time) (*ImplementationRun, error)
	TransitionImplementation(context.Context, string, string, RunStatus, RunStatus, RunUpdate) error
	RenewImplementationLease(context.Context, string, string, time.Time) (bool, bool, error)
	RequestCancel(context.Context, string) (RunStatus, error)
	InterruptActiveImplementations(context.Context, time.Time, time.Time) ([]string, error)

	AppendRunEvent(context.Context, RunEvent) (*RunEvent, error)
	AppendRunEvents(context.Context, []RunEvent) ([]RunEvent, error)
	ListRunEvents(context.Context, string, int64, int) ([]RunEvent, error)
	SaveChangeSetAndFinish(context.Context, ChangeSet, RunStatus, string, time.Time) error
	GetChangeSet(context.Context, string) (*ChangeSet, error)
	ReviewChangeSet(context.Context, ChangeReview) error

	SaveReviewPolicy(context.Context, ReviewPolicy) error
	GetReviewPolicy(context.Context, string, int64) (*ReviewPolicy, error)
	CreateReviewRound(context.Context, ReviewRound, []ReviewAssignment) error
	GetReviewRound(context.Context, string) (*ReviewRound, error)
	ListReviewAssignments(context.Context, string, ReviewAssignmentCursor, int) ([]ReviewAssignment, error)
	TransitionReviewRound(context.Context, string, ReviewRoundStatus, ReviewRoundStatus, time.Time) error
	TransitionReviewAssignment(context.Context, string, ReviewAssignmentStatus, ReviewAssignmentStatus, string, string, time.Time) error
	CompleteReviewAssignment(context.Context, ReviewReport) error
	LoadFullReviewEvaluation(context.Context, string) (ReviewEvaluation, error)
	CompleteReviewRound(context.Context, ReviewGateResult, time.Time) error
	GetReviewGateResult(context.Context, string) (*ReviewGateResult, error)
	CreateFindingResolution(context.Context, FindingResolution) error
	ListFindingResolutionsByIDs(context.Context, []string, string) ([]FindingResolution, error)

	ListExpiredWorktrees(context.Context, time.Time, int) ([]ImplementationRun, error)
	MarkWorktreeCleaned(context.Context, string, string) error
}

type RunUpdate struct {
	ProviderVersion   string
	ProviderSessionID string
	ExitCode          *int
	ErrorSummary      string
	RetainUntil       *time.Time
}
