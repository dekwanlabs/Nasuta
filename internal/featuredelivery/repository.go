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
	GetGenerationForArtifact(context.Context, string) (*GenerationRun, error)
	GetSuccessfulGenerationForWorkflowNode(context.Context, string, string) (*GenerationRun, error)
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

	SaveReviewPolicies(context.Context, []ReviewPolicy) error
	GetReviewPolicy(context.Context, string, int64) (*ReviewPolicy, error)
	CreateReviewRound(context.Context, ReviewRound, []ReviewAssignment) error
	GetReviewRound(context.Context, string) (*ReviewRound, error)
	GetLatestCompletedReviewRoundBySubjectHash(context.Context, string) (*ReviewRound, error)
	BindReviewRoundWorkflow(context.Context, string, string, time.Time) error
	ListReviewAssignments(context.Context, string, ReviewAssignmentCursor, int) ([]ReviewAssignment, error)
	GetLatestReviewAssignment(context.Context, string, string) (*ReviewAssignment, error)
	StartReviewAssignmentAttempt(context.Context, string, string, int, string, time.Time) (*ReviewAssignment, error)
	GetSuccessfulReviewReport(context.Context, string, string) (*ReviewReport, error)
	GetReviewReportByAssignment(context.Context, string, string) (*ReviewReport, error)
	ListReviewFindings(context.Context, string, Severity, FindingCursor, int) ([]FindingSummary, error)
	GetReviewFinding(context.Context, string) (*FindingDetail, error)
	TransitionReviewRound(context.Context, string, ReviewRoundStatus, ReviewRoundStatus, time.Time) error
	TransitionReviewAssignment(context.Context, string, ReviewAssignmentStatus, ReviewAssignmentStatus, string, string, time.Time) error
	RequestReviewRoundCancel(context.Context, string, time.Time) (bool, error)
	AppendReviewEvent(context.Context, ReviewEvent) (*ReviewEvent, error)
	ListReviewEvents(context.Context, string, int64, int) ([]ReviewEvent, error)
	CompleteReviewAssignment(context.Context, ReviewReport) error
	SaveReviewAdjudications(context.Context, []ReviewAdjudication) error
	ListReviewAdjudications(context.Context, string, ReviewAdjudicationCursor, int) ([]ReviewAdjudication, error)
	LoadFullReviewEvaluation(context.Context, string) (ReviewEvaluation, error)
	CompleteReviewRound(context.Context, ReviewGateResult, time.Time) error
	GetReviewGateResult(context.Context, string) (*ReviewGateResult, error)
	GetReviewGateResultByRound(context.Context, string) (*ReviewGateResult, error)
	CreateFindingResolution(context.Context, FindingResolution) error
	ListFindingResolutions(context.Context, string, string, FindingResolutionCursor, int) ([]FindingResolution, error)
	ListFindingResolutionsByIDs(context.Context, []string, string) ([]FindingResolution, error)

	ListExpiredWorktrees(context.Context, time.Time, int) ([]ImplementationRun, error)
	MarkWorktreeCleaned(context.Context, string, string) error
}

// ReviewPolicyControlStore owns mutable rollout metadata around immutable policies.
type ReviewPolicyControlStore interface {
	PublishReviewPolicies(context.Context, []ReviewPolicy, int64) error
	ListReviewPolicyRecords(context.Context, ReviewPolicyCursor, int) ([]ReviewPolicyRecord, error)
	GetReviewPolicyRecord(context.Context, string, int64) (ReviewPolicyRecord, error)
	GetDefaultReviewPolicy(context.Context, SubjectKind) (ReviewPolicyRef, error)
	EnsureReviewPolicyDefault(context.Context, string, int64, int64) error
	SetReviewPolicyDefault(context.Context, string, int64, int64) error
	SetReviewPolicyActive(context.Context, string, int64, bool, int64) error
	ListReviewPolicyAudit(context.Context, string, int64, int) ([]ReviewPolicyAuditEvent, error)
	GetReviewPolicyRollout(context.Context, SubjectKind) (ReviewPolicyRolloutRule, bool, error)
	SetReviewPolicyRollout(context.Context, ReviewPolicyRolloutRule, int64) error
	ListReviewPolicyRolloutAudit(context.Context, SubjectKind, int64, int) ([]ReviewPolicyRolloutAuditEvent, error)
}

// ReviewRoundQueryStore provides bounded summary reads for operations views.
type ReviewRoundQueryStore interface {
	ListReviewRoundSummaries(
		context.Context,
		ReviewRoundFilter,
		ReviewRoundCursor,
		int,
		int64,
		bool,
	) ([]ReviewRoundSummary, bool, error)
}

// ReviewReportReuseStore owns exact source lookup and atomic target materialization.
type ReviewReportReuseStore interface {
	GetReviewReportReuseSources(context.Context, []string) ([]ReviewReportReuseSource, error)
	CreateReviewRoundWithReuses(
		context.Context,
		ReviewRound,
		[]ReviewAssignment,
		[]ReviewReport,
		[]ReviewReportReuse,
	) error
}

type RunUpdate struct {
	ProviderVersion   string
	ProviderSessionID string
	ExitCode          *int
	ErrorSummary      string
	RetainUntil       *time.Time
}
