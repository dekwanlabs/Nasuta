package featuredelivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	KindRequirement         ArtifactKind = "requirement"
	KindRequirementAnalysis ArtifactKind = "requirement_analysis"
	KindTechnicalProposal   ArtifactKind = "technical_proposal"
	KindSystemDesign        ArtifactKind = "system_design"
	KindImplementationPlan  ArtifactKind = "implementation_plan"
)

const (
	OriginUser  ArtifactOrigin = "user"
	OriginAgent ArtifactOrigin = "agent"
)

const (
	DecisionApproved ReviewDecision = "approved"
	DecisionRejected ReviewDecision = "rejected"
)

const (
	RunQueued      RunStatus = "queued"
	RunPreparing   RunStatus = "preparing"
	RunRunning     RunStatus = "running"
	RunValidating  RunStatus = "validating"
	RunSucceeded   RunStatus = "succeeded"
	RunFailed      RunStatus = "failed"
	RunCancelled   RunStatus = "cancelled"
	RunInterrupted RunStatus = "interrupted"
)

const (
	EventRunQueued          EventKind = "run_queued"
	EventRunPreparing       EventKind = "run_preparing"
	EventProviderStarted    EventKind = "provider_started"
	EventProviderMessage    EventKind = "provider_message"
	EventCommandStarted     EventKind = "command_started"
	EventCommandFinished    EventKind = "command_finished"
	EventFileChanged        EventKind = "file_changed"
	EventProviderFinished   EventKind = "provider_finished"
	EventValidationStarted  EventKind = "validation_started"
	EventValidationFinished EventKind = "validation_finished"
	EventChangeSetReady     EventKind = "change_set_ready"
	EventRunFailed          EventKind = "run_failed"
	EventRunCancelled       EventKind = "run_cancelled"
	EventRunInterrupted     EventKind = "run_interrupted"
	EventRunSucceeded       EventKind = "run_succeeded"
)

var (
	ErrInvalid     = errors.New("feature delivery input invalid")
	ErrNotFound    = errors.New("feature delivery resource not found")
	ErrForbidden   = errors.New("feature delivery operation forbidden")
	ErrConflict    = errors.New("feature delivery conflict")
	ErrUnavailable = errors.New("feature delivery capability unavailable")
)

type ArtifactKind string
type ArtifactOrigin string
type ReviewDecision string
type RunStatus string
type EventKind string

type FeatureRequest struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	CreatedBy  int64      `json:"created_by"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type RequirementDocument struct {
	Description         string   `json:"description"`
	TargetRepositories  []string `json:"target_repositories"`
	BusinessConstraints []string `json:"business_constraints"`
	Attachments         []string `json:"attachments"`
	AcceptanceCriteria  []string `json:"acceptance_criteria"`
}

type RequirementAnalysisDocument struct {
	Background                string          `json:"background"`
	Goals                     []string        `json:"goals"`
	UsersAndScenarios         []string        `json:"users_and_scenarios"`
	FunctionalRequirements    []string        `json:"functional_requirements"`
	NonFunctionalRequirements []string        `json:"non_functional_requirements"`
	InScope                   []string        `json:"in_scope"`
	OutOfScope                []string        `json:"out_of_scope"`
	BusinessRules             []string        `json:"business_rules"`
	AcceptanceCriteria        []string        `json:"acceptance_criteria"`
	Assumptions               []string        `json:"assumptions"`
	BlockingQuestions         []string        `json:"blocking_questions"`
	OpenQuestions             []string        `json:"open_questions"`
	InitialImpact             []string        `json:"initial_impact"`
	Claims                    []EvidenceClaim `json:"claims"`
}

type TechnicalProposalDocument struct {
	CurrentFacts         []EvidenceClaim  `json:"current_facts"`
	AffectedAreas        []string         `json:"affected_areas"`
	Options              []ProposalOption `json:"options"`
	Recommendation       string           `json:"recommendation"`
	RecommendationReason string           `json:"recommendation_reason"`
	DataAndAPIImpact     []string         `json:"data_and_api_impact"`
	CompatibilityRisks   []string         `json:"compatibility_risks"`
	Rollout              []string         `json:"rollout"`
	Rollback             []string         `json:"rollback"`
	OpenDecisions        []string         `json:"open_decisions"`
	BlockingQuestions    []string         `json:"blocking_questions"`
}

type ProposalOption struct {
	Name     string   `json:"name"`
	Summary  string   `json:"summary"`
	Benefits []string `json:"benefits"`
	Costs    []string `json:"costs"`
	Risks    []string `json:"risks"`
}

type SystemDesignDocument struct {
	ArchitectureBoundaries []string        `json:"architecture_boundaries"`
	Modules                []DesignModule  `json:"modules"`
	KeyFlows               []string        `json:"key_flows"`
	APIContracts           []string        `json:"api_contracts"`
	DataModel              []string        `json:"data_model"`
	Consistency            []string        `json:"consistency"`
	Security               []string        `json:"security"`
	Configuration          []string        `json:"configuration"`
	ErrorsAndDegradation   []string        `json:"errors_and_degradation"`
	Observability          []string        `json:"observability"`
	Testing                []string        `json:"testing"`
	RolloutAndRollback     []string        `json:"rollout_and_rollback"`
	RejectedAlternatives   []string        `json:"rejected_alternatives"`
	BlockingQuestions      []string        `json:"blocking_questions"`
	Claims                 []EvidenceClaim `json:"claims"`
}

type DesignModule struct {
	Name             string   `json:"name"`
	Responsibilities []string `json:"responsibilities"`
	Dependencies     []string `json:"dependencies"`
}

type ImplementationPlanDocument struct {
	Repositories      []RepositoryPlan `json:"repositories"`
	Contracts         []string         `json:"contracts"`
	Migrations        []string         `json:"migrations"`
	Risks             []string         `json:"risks"`
	DoNotModify       []string         `json:"do_not_modify"`
	BlockingQuestions []string         `json:"blocking_questions"`
}

type RepositoryPlan struct {
	Repository         string               `json:"repository"`
	ExpectedPaths      []string             `json:"expected_paths"`
	Steps              []ImplementationStep `json:"steps"`
	ValidationCommands [][]string           `json:"validation_commands"`
}

type ImplementationStep struct {
	Description string   `json:"description"`
	DoneWhen    []string `json:"done_when"`
}

type EvidenceClaim struct {
	Statement      string `json:"statement"`
	Classification string `json:"classification"`
	EvidenceIDs    []int  `json:"evidence_ids"`
}

type EvidenceRef struct {
	Kind      string `json:"kind"`
	Repo      string `json:"repo,omitempty"`
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Service   string `json:"service,omitempty"`
	Summary   string `json:"summary"`
	Hash      string `json:"hash"`
}

type Artifact struct {
	ID               string          `json:"id"`
	RequestID        string          `json:"request_id"`
	Kind             ArtifactKind    `json:"kind"`
	Version          int             `json:"version"`
	ParentArtifactID string          `json:"parent_artifact_id,omitempty"`
	Origin           ArtifactOrigin  `json:"origin"`
	DocumentJSON     json.RawMessage `json:"document_json"`
	RenderedMarkdown string          `json:"rendered_markdown"`
	Evidence         []EvidenceRef   `json:"evidence"`
	ContentHash      string          `json:"content_hash"`
	CreatedBy        int64           `json:"created_by"`
	CreatedAt        time.Time       `json:"created_at"`
	Review           *ArtifactReview `json:"review,omitempty"`
	Stale            bool            `json:"stale"`
}

type ArtifactReview struct {
	ArtifactID string         `json:"artifact_id"`
	Decision   ReviewDecision `json:"decision"`
	Comment    string         `json:"comment"`
	Reviewer   int64          `json:"reviewer"`
	CreatedAt  time.Time      `json:"created_at"`
}

type GenerationRun struct {
	ID               string       `json:"id"`
	RequestID        string       `json:"request_id"`
	ArtifactKind     ArtifactKind `json:"artifact_kind"`
	ParentArtifactID string       `json:"parent_artifact_id"`
	Status           string       `json:"status"`
	Provider         string       `json:"provider"`
	Model            string       `json:"model"`
	RequestedBy      int64        `json:"requested_by"`
	InputTokens      int64        `json:"input_tokens"`
	OutputTokens     int64        `json:"output_tokens"`
	ErrorSummary     string       `json:"error_summary,omitempty"`
	StartedAt        time.Time    `json:"started_at"`
	EndedAt          *time.Time   `json:"ended_at,omitempty"`
}

type OwnerIdentity struct {
	UserID int64
	Name   string
	Email  string
}

type UserWorkspace struct {
	UserID           int64     `json:"user_id"`
	UsernameKey      string    `json:"username_key"`
	UsernameSnapshot string    `json:"username_snapshot"`
	CreatedAt        time.Time `json:"created_at"`
}

type ImplementationRun struct {
	ID                string        `json:"id"`
	RequestID         string        `json:"request_id"`
	ClientRequestID   string        `json:"client_request_id"`
	RequestHash       string        `json:"-"`
	DesignArtifactID  string        `json:"design_artifact_id"`
	PlanArtifactID    string        `json:"plan_artifact_id"`
	ParentRunID       string        `json:"parent_run_id,omitempty"`
	Repo              string        `json:"repo"`
	BaseRef           string        `json:"base_ref"`
	BaseCommit        string        `json:"base_commit"`
	WorkspaceUserID   int64         `json:"workspace_user_id"`
	WorkspaceUsername string        `json:"workspace_username"`
	Provider          string        `json:"provider"`
	Model             string        `json:"model,omitempty"`
	ProviderVersion   string        `json:"provider_version,omitempty"`
	NetworkEnabled    bool          `json:"network_enabled"`
	Status            RunStatus     `json:"status"`
	WorkerID          string        `json:"worker_id,omitempty"`
	LeaseExpiresAt    *time.Time    `json:"lease_expires_at,omitempty"`
	CancelRequestedAt *time.Time    `json:"cancel_requested_at,omitempty"`
	ProviderSessionID string        `json:"provider_session_id,omitempty"`
	ExitCode          *int          `json:"exit_code,omitempty"`
	ErrorSummary      string        `json:"error_summary,omitempty"`
	RequestedBy       int64         `json:"requested_by"`
	StartedAt         *time.Time    `json:"started_at,omitempty"`
	EndedAt           *time.Time    `json:"ended_at,omitempty"`
	RetainUntil       *time.Time    `json:"retain_until,omitempty"`
	WorktreeCleanedAt *time.Time    `json:"worktree_cleaned_at,omitempty"`
	CleanupError      string        `json:"cleanup_error,omitempty"`
	CreatedAt         time.Time     `json:"created_at"`
	ChangeSet         *ChangeSet    `json:"change_set,omitempty"`
	Review            *ChangeReview `json:"review,omitempty"`
}

type RunEvent struct {
	RunID     string          `json:"run_id"`
	Seq       int64           `json:"seq"`
	Kind      EventKind       `json:"kind"`
	Summary   string          `json:"summary"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type CodingRequest struct {
	RunID          string
	Provider       string
	Model          string
	WorktreePath   string
	BaseCommit     string
	TaskPackage    string
	NetworkEnabled bool
	Timeout        time.Duration
}

type CodingResult struct {
	ProviderSessionID string
	ProviderVersion   string
	ExitCode          int
	Summary           string
	TestSummary       string
	InputTokens       int64
	OutputTokens      int64
	EventCount        int
}

type ProviderEvent struct {
	Kind    EventKind
	Summary string
	Detail  json.RawMessage
}

type EventSink func(context.Context, ProviderEvent) error

type CodingRunner interface {
	Run(context.Context, CodingRequest, EventSink) (CodingResult, error)
	ProviderStatus(context.Context) map[string]CodingProviderStatus
}

type CodingProviderStatus struct {
	Enabled            bool   `json:"enabled"`
	BinaryFound        bool   `json:"binary_found"`
	BinaryVersion      string `json:"binary_version,omitempty"`
	ContractCompatible bool   `json:"contract_compatible"`
	CredentialIsolated bool   `json:"credential_isolated"`
	Reason             string `json:"reason,omitempty"`
}

type CapabilityStatus struct {
	Enabled bool `json:"enabled"`
}

type CodingCapabilityStatus struct {
	Enabled   bool                            `json:"enabled"`
	GitFound  bool                            `json:"git_found"`
	Isolation string                          `json:"isolation,omitempty"`
	Providers map[string]CodingProviderStatus `json:"providers"`
}

type FeatureDeliveryStatus struct {
	Persistence CapabilityStatus       `json:"persistence"`
	Generation  CapabilityStatus       `json:"generation"`
	Coding      CodingCapabilityStatus `json:"coding"`
}

type ChangedFile struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
}

type ValidationResult struct {
	Sequence      int      `json:"sequence"`
	Argv          []string `json:"argv"`
	Status        string   `json:"status"`
	ExitCode      int      `json:"exit_code"`
	DurationMS    int64    `json:"duration_ms"`
	OutputSummary string   `json:"output_summary"`
	OutputRelPath string   `json:"output_rel_path,omitempty"`
	OutputSHA256  string   `json:"output_sha256,omitempty"`
	TimedOut      bool     `json:"timed_out"`
}

type ChangeSet struct {
	RunID             string             `json:"run_id"`
	WorktreeHead      string             `json:"worktree_head"`
	PatchRelPath      string             `json:"patch_rel_path"`
	PatchSHA256       string             `json:"patch_sha256"`
	PatchBytes        int64              `json:"patch_bytes"`
	FilesChanged      int                `json:"files_changed"`
	Additions         int                `json:"additions"`
	Deletions         int                `json:"deletions"`
	Files             []ChangedFile      `json:"files"`
	ValidationResults []ValidationResult `json:"validation_results"`
	ProviderSummary   string             `json:"provider_summary"`
	CreatedAt         time.Time          `json:"created_at"`
}

type ChangeReview struct {
	RunID     string         `json:"run_id"`
	Decision  ReviewDecision `json:"decision"`
	Comment   string         `json:"comment"`
	Reviewer  int64          `json:"reviewer"`
	CreatedAt time.Time      `json:"created_at"`
}

type Lineage struct {
	Requirement         *Artifact `json:"requirement,omitempty"`
	RequirementAnalysis *Artifact `json:"requirement_analysis,omitempty"`
	TechnicalProposal   *Artifact `json:"technical_proposal,omitempty"`
	SystemDesign        *Artifact `json:"system_design,omitempty"`
	ImplementationPlan  *Artifact `json:"implementation_plan,omitempty"`
}

func NewID(prefix string) (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(data[:]), nil
}

func ParseArtifactKind(value string) (ArtifactKind, error) {
	kind := ArtifactKind(strings.TrimSpace(value))
	switch kind {
	case KindRequirement, KindRequirementAnalysis, KindTechnicalProposal, KindSystemDesign, KindImplementationPlan:
		return kind, nil
	default:
		return "", fmt.Errorf("unsupported artifact kind %q: %w", value, ErrInvalid)
	}
}

func ParentKind(kind ArtifactKind) (ArtifactKind, bool) {
	switch kind {
	case KindRequirementAnalysis:
		return KindRequirement, true
	case KindTechnicalProposal:
		return KindRequirementAnalysis, true
	case KindSystemDesign:
		return KindTechnicalProposal, true
	case KindImplementationPlan:
		return KindSystemDesign, true
	default:
		return "", false
	}
}

func IsActiveRun(status RunStatus) bool {
	switch status {
	case RunQueued, RunPreparing, RunRunning, RunValidating:
		return true
	default:
		return false
	}
}

func IsTerminalRun(status RunStatus) bool {
	return status == RunSucceeded || status == RunFailed || status == RunCancelled || status == RunInterrupted
}

func ValidateDecision(value string) (ReviewDecision, error) {
	decision := ReviewDecision(strings.TrimSpace(value))
	if decision != DecisionApproved && decision != DecisionRejected {
		return "", fmt.Errorf("decision must be approved or rejected")
	}
	return decision, nil
}
