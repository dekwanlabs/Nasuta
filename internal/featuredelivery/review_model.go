package featuredelivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/platform"
)

const (
	SubjectRequirement         SubjectKind = "requirement_artifact"
	SubjectRequirementAnalysis SubjectKind = "requirement_analysis_artifact"
	SubjectTechnicalProposal   SubjectKind = "technical_proposal_artifact"
	SubjectSystemDesign        SubjectKind = "system_design_artifact"
	SubjectImplementationPlan  SubjectKind = "implementation_plan_artifact"
	SubjectChangeSet           SubjectKind = "change_set"
	SubjectValidationBundle    SubjectKind = "validation_bundle"
	SubjectDeliveryBundle      SubjectKind = "delivery_bundle"
)

const (
	RoundCreated    ReviewRoundStatus = "created"
	RoundRunning    ReviewRoundStatus = "running"
	RoundEvaluating ReviewRoundStatus = "evaluating"
	RoundCompleted  ReviewRoundStatus = "completed"
	RoundFailed     ReviewRoundStatus = "failed"
	RoundCancelled  ReviewRoundStatus = "cancelled"
)

const (
	AssignmentQueued    ReviewAssignmentStatus = "queued"
	AssignmentRunning   ReviewAssignmentStatus = "running"
	AssignmentSucceeded ReviewAssignmentStatus = "succeeded"
	AssignmentReused    ReviewAssignmentStatus = "reused"
	AssignmentFailed    ReviewAssignmentStatus = "failed"
	AssignmentCancelled ReviewAssignmentStatus = "cancelled"
)

const (
	GatePass          GateDecision = "pass"
	GateRevise        GateDecision = "revise"
	GateHumanRequired GateDecision = "human_required"
	GateIncomplete    GateDecision = "incomplete"
	GateFailed        GateDecision = "failed"
)

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

const (
	ResolutionFixed       FindingResolutionKind = "fixed"
	ResolutionWaived      FindingResolutionKind = "waived"
	ResolutionInvalidated FindingResolutionKind = "invalidated"
	ResolutionSuperseded  FindingResolutionKind = "superseded"
)

const (
	AdjudicationConfirmed        AdjudicationDecision = "confirmed"
	AdjudicationNotSupported     AdjudicationDecision = "not_supported"
	AdjudicationDistinctFindings AdjudicationDecision = "distinct_findings"
	AdjudicationNeedsHuman       AdjudicationDecision = "needs_human"
)

const (
	ReviewEventRoundStarted         ReviewEventKind = "round_started"
	ReviewEventAssignmentStarted    ReviewEventKind = "assignment_started"
	ReviewEventAssignmentSucceeded  ReviewEventKind = "assignment_succeeded"
	ReviewEventAssignmentFailed     ReviewEventKind = "assignment_failed"
	ReviewEventRoundEvaluating      ReviewEventKind = "round_evaluating"
	ReviewEventAdjudicationStarted  ReviewEventKind = "adjudication_started"
	ReviewEventAdjudicationFinished ReviewEventKind = "adjudication_finished"
	ReviewEventRoundCompleted       ReviewEventKind = "round_completed"
	ReviewEventRoundFailed          ReviewEventKind = "round_failed"
	ReviewEventRoundCancelled       ReviewEventKind = "round_cancelled"
)

const (
	maxReviewersPerPolicy = 16
	maxFindingsPerReport  = 100
	maxEvidencePerFinding = 20
)

type SubjectKind string
type ReviewRoundStatus string
type ReviewAssignmentStatus string
type GateDecision string
type Severity string
type FindingResolutionKind string
type AdjudicationDecision string
type ReviewEventKind string
type OptionalReviewerAction string

const (
	OptionalReviewerContinue      OptionalReviewerAction = "continue"
	OptionalReviewerHumanRequired OptionalReviewerAction = "human_required"
)

const (
	RiskEqual              ReviewRiskOperator = "eq"
	RiskNotEqual           ReviewRiskOperator = "ne"
	RiskGreaterThan        ReviewRiskOperator = "gt"
	RiskGreaterThanOrEqual ReviewRiskOperator = "gte"
	RiskLessThan           ReviewRiskOperator = "lt"
	RiskLessThanOrEqual    ReviewRiskOperator = "lte"
)

// ReviewSubject pins every input whose change invalidates an earlier review.
type ReviewSubject struct {
	Kind              SubjectKind `json:"kind"`
	ID                string      `json:"id"`
	Version           int         `json:"version"`
	SourceContentHash string      `json:"source_content_hash"`
	ParentHash        string      `json:"parent_hash,omitempty"`
	EvidenceHash      string      `json:"evidence_hash,omitempty"`
	Repository        string      `json:"repository,omitempty"`
	BaseCommit        string      `json:"base_commit,omitempty"`
	HeadCommit        string      `json:"head_commit,omitempty"`
	ContentHash       string      `json:"content_hash"`
}

type ReviewerSpec struct {
	ID             string                 `json:"id"`
	Agent          agentapi.DefinitionRef `json:"agent"`
	DefinitionHash string                 `json:"definition_hash"`
	Categories     []string               `json:"categories"`
	Required       bool                   `json:"required"`
	ReadOnly       bool                   `json:"read_only"`
}

type AdjudicatorSpec struct {
	Agent          agentapi.DefinitionRef `json:"agent"`
	DefinitionHash string                 `json:"definition_hash"`
	ReadOnly       bool                   `json:"read_only"`
}

type ReviewRiskOperator string

type ReviewRiskFact struct {
	Key   string `json:"key"`
	Value int64  `json:"value"`
}

type ReviewRiskCondition struct {
	Fact     string             `json:"fact"`
	Operator ReviewRiskOperator `json:"operator"`
	Value    int64              `json:"value"`
}

type ReviewRiskRule struct {
	ID          string                `json:"id"`
	Conditions  []ReviewRiskCondition `json:"conditions"`
	ReviewerIDs []string              `json:"reviewer_ids"`
}

// ReviewPolicy is immutable after PrepareReviewPolicy computes its hash.
type ReviewPolicy struct {
	ID                     string                 `json:"id"`
	Version                int64                  `json:"version"`
	SubjectKind            SubjectKind            `json:"subject_kind"`
	Reviewers              []ReviewerSpec         `json:"reviewers"`
	Adjudicator            *AdjudicatorSpec       `json:"adjudicator,omitempty"`
	BlockingSeverities     []Severity             `json:"blocking_severities"`
	RequiredCategories     []string               `json:"required_categories"`
	MaxParallelism         int                    `json:"max_parallelism"`
	MaxInputTokens         int64                  `json:"max_input_tokens"`
	MaxOutputTokens        int64                  `json:"max_output_tokens"`
	MaxTotalTokens         int64                  `json:"max_total_tokens"`
	MaxToolCalls           int64                  `json:"max_tool_calls"`
	MaxCostMicros          int64                  `json:"max_cost_micros"`
	MaxRetries             int64                  `json:"max_retries"`
	Timeout                time.Duration          `json:"timeout"`
	OptionalReviewerAction OptionalReviewerAction `json:"optional_reviewer_action"`
	RiskRuleVersion        string                 `json:"risk_rule_version,omitempty"`
	RiskRules              []ReviewRiskRule       `json:"risk_rules,omitempty"`
	ContentHash            string                 `json:"content_hash"`
	CreatedAt              time.Time              `json:"created_at"`
}

// ReviewPolicyRecord adds rollout metadata without changing the immutable policy hash.
type ReviewPolicyRecord struct {
	ReviewPolicy
	Active    bool  `json:"active"`
	Default   bool  `json:"default"`
	CreatedBy int64 `json:"created_by"`
}

// ReviewPolicyRolloutRule selects one immutable Policy candidate for a stable subject population.
type ReviewPolicyRolloutRule struct {
	SubjectKind            SubjectKind `json:"subject_kind"`
	RuleVersion            int64       `json:"rule_version"`
	CandidatePolicyID      string      `json:"candidate_policy_id"`
	CandidatePolicyVersion int64       `json:"candidate_policy_version"`
	PercentageBPS          int         `json:"percentage_bps"`
	Salt                   string      `json:"salt"`
	RuleHash               string      `json:"rule_hash"`
	Active                 bool        `json:"active"`
	CreatedBy              int64       `json:"created_by"`
	CreatedAt              time.Time   `json:"created_at"`
}

// ReviewPolicyRolloutAuditEvent records one auditable change to a Policy rollout.
type ReviewPolicyRolloutAuditEvent struct {
	Seq                    int64       `json:"seq"`
	SubjectKind            SubjectKind `json:"subject_kind"`
	RuleVersion            int64       `json:"rule_version"`
	CandidatePolicyID      string      `json:"candidate_policy_id"`
	CandidatePolicyVersion int64       `json:"candidate_policy_version"`
	PercentageBPS          int         `json:"percentage_bps"`
	RuleHash               string      `json:"rule_hash"`
	Action                 string      `json:"action"`
	ActorUserID            int64       `json:"actor_user_id"`
	CreatedAt              time.Time   `json:"created_at"`
}

// ReviewPolicySelection fixes the Policy rollout decision for one Round.
type ReviewPolicySelection struct {
	RuleVersion            int64  `json:"rule_version,omitempty"`
	RuleHash               string `json:"rule_hash,omitempty"`
	CandidatePolicyID      string `json:"candidate_policy_id,omitempty"`
	CandidatePolicyVersion int64  `json:"candidate_policy_version,omitempty"`
	BucketBasisPoints      int    `json:"bucket_basis_points,omitempty"`
	PercentageBasisPoints  int    `json:"percentage_basis_points,omitempty"`
	StableKeyHash          string `json:"stable_key_hash,omitempty"`
	Reason                 string `json:"reason,omitempty"`
}

type ReviewPolicyCursor struct {
	ID      string
	Version int64
}

type ReviewPolicyAuditEvent struct {
	Seq         int64     `json:"seq"`
	PolicyID    string    `json:"policy_id"`
	Version     int64     `json:"version"`
	Action      string    `json:"action"`
	ActorUserID int64     `json:"actor_user_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// ReviewPolicyRef selects one previously published immutable policy version.
type ReviewPolicyRef struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

type ReviewRound struct {
	ID              string                `json:"id"`
	WorkflowRunID   string                `json:"workflow_run_id,omitempty"`
	Subject         ReviewSubject         `json:"subject"`
	PolicyID        string                `json:"policy_id"`
	PolicyVersion   int64                 `json:"policy_version"`
	PolicyHash      string                `json:"policy_hash"`
	PolicySelection ReviewPolicySelection `json:"policy_selection"`
	RiskFacts       []ReviewRiskFact      `json:"risk_facts"`
	RiskHash        string                `json:"risk_hash"`
	RuleVersion     string                `json:"selection_rule_version,omitempty"`
	Reviewers       []ReviewerSpec        `json:"selected_reviewers"`
	PanelHash       string                `json:"panel_hash"`
	Status          ReviewRoundStatus     `json:"status"`
	CreatedBy       int64                 `json:"created_by"`
	CreatedAt       time.Time             `json:"created_at"`
	CompletedAt     *time.Time            `json:"completed_at,omitempty"`
}

// ReviewRoundSummary excludes subject_json and report payloads from operations lists.
type ReviewRoundSummary struct {
	ID             string            `json:"id"`
	WorkflowRunID  string            `json:"workflow_run_id,omitempty"`
	FeatureID      string            `json:"feature_id"`
	SubjectKind    SubjectKind       `json:"subject_kind"`
	SubjectID      string            `json:"subject_id"`
	SubjectVersion int               `json:"subject_version"`
	SubjectHash    string            `json:"subject_hash"`
	PolicyID       string            `json:"policy_id"`
	PolicyVersion  int64             `json:"policy_version"`
	PolicyHash     string            `json:"policy_hash"`
	RiskHash       string            `json:"risk_hash"`
	RuleVersion    string            `json:"selection_rule_version,omitempty"`
	PanelHash      string            `json:"panel_hash"`
	ReviewerCount  int               `json:"reviewer_count"`
	Status         ReviewRoundStatus `json:"status"`
	CreatedBy      int64             `json:"created_by"`
	CreatedAt      time.Time         `json:"created_at"`
	CompletedAt    *time.Time        `json:"completed_at,omitempty"`
}

// ReviewEvent is the durable ordered audit stream for one Review Round.
type ReviewEvent struct {
	RoundID   string          `json:"round_id"`
	Seq       int64           `json:"seq"`
	Kind      ReviewEventKind `json:"kind"`
	Summary   string          `json:"summary"`
	Detail    json.RawMessage `json:"detail,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type ReviewAssignment struct {
	ID             string                 `json:"id"`
	RoundID        string                 `json:"round_id"`
	ReviewerID     string                 `json:"reviewer_id"`
	Agent          agentapi.DefinitionRef `json:"agent"`
	DefinitionHash string                 `json:"definition_hash"`
	Categories     []string               `json:"categories"`
	Required       bool                   `json:"required"`
	Status         ReviewAssignmentStatus `json:"status"`
	Attempt        int                    `json:"attempt"`
	AgentRunID     string                 `json:"agent_run_id,omitempty"`
	ErrorCode      string                 `json:"error_code,omitempty"`
	CreatedAt      time.Time              `json:"created_at"`
	StartedAt      *time.Time             `json:"started_at,omitempty"`
	CompletedAt    *time.Time             `json:"completed_at,omitempty"`
}

type CoverageItem struct {
	Category string `json:"category"`
	Covered  bool   `json:"covered"`
	Summary  string `json:"summary,omitempty"`
}

type Uncertainty struct {
	Category string `json:"category"`
	Summary  string `json:"summary"`
}

type FindingLocation struct {
	Path      string `json:"path,omitempty"`
	Field     string `json:"field,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

type FindingEvidence struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Hash    string `json:"hash"`
	Summary string `json:"summary"`
}

type Finding struct {
	ID             string            `json:"id"`
	ReportID       string            `json:"report_id"`
	Category       string            `json:"category"`
	Severity       Severity          `json:"severity"`
	Claim          string            `json:"claim"`
	Impact         string            `json:"impact"`
	Evidence       []FindingEvidence `json:"evidence"`
	Location       *FindingLocation  `json:"location,omitempty"`
	Recommendation string            `json:"recommendation"`
	Confidence     float64           `json:"confidence"`
	Fingerprint    string            `json:"fingerprint"`
	ContentHash    string            `json:"content_hash"`
}

type ReviewReport struct {
	ID            string                `json:"id"`
	RoundID       string                `json:"round_id"`
	AssignmentID  string                `json:"assignment_id"`
	ReviewerID    string                `json:"reviewer_id"`
	SubjectHash   string                `json:"subject_hash"`
	Coverage      []CoverageItem        `json:"coverage"`
	Findings      []Finding             `json:"findings"`
	Uncertainties []Uncertainty         `json:"uncertainties"`
	Summary       string                `json:"summary"`
	ReportHash    string                `json:"report_hash"`
	ContentHash   string                `json:"content_hash"`
	Reuse         *ReviewReportReuseRef `json:"reuse,omitempty"`
	CompletedAt   time.Time             `json:"completed_at"`
}

// ReviewReportReuseRequest explicitly selects one immutable source Report.
type ReviewReportReuseRequest struct {
	ReviewerID     string `json:"reviewer_id"`
	SourceReportID string `json:"source_report_id"`
	ReportHash     string `json:"report_hash"`
	Reason         string `json:"reason"`
}

// ReviewReportReuseRef is copied into the target Report consumed by the Gate.
type ReviewReportReuseRef struct {
	SourceReportID     string `json:"source_report_id"`
	SourceRoundID      string `json:"source_round_id"`
	SourceAssignmentID string `json:"source_assignment_id"`
	Reason             string `json:"reason"`
}

// ReviewReportReuse is the append-only audit fact for one materialized reuse.
type ReviewReportReuse struct {
	ID                 string    `json:"id"`
	RoundID            string    `json:"round_id"`
	AssignmentID       string    `json:"assignment_id"`
	ReportID           string    `json:"report_id"`
	ReviewerID         string    `json:"reviewer_id"`
	SourceRoundID      string    `json:"source_round_id"`
	SourceAssignmentID string    `json:"source_assignment_id"`
	SourceReportID     string    `json:"source_report_id"`
	SubjectHash        string    `json:"subject_hash"`
	PolicyHash         string    `json:"policy_hash"`
	DefinitionHash     string    `json:"definition_hash"`
	ReportHash         string    `json:"report_hash"`
	Reason             string    `json:"reason"`
	ActorID            int64     `json:"actor_id"`
	CreatedAt          time.Time `json:"created_at"`
}

// ReviewReportReuseSource pins the source identities required for exact matching.
type ReviewReportReuseSource struct {
	Report        ReviewReport
	Assignment    ReviewAssignment
	PolicyID      string
	PolicyVersion int64
	PolicyHash    string
}

type ReviewGateResult struct {
	ID                 string       `json:"id"`
	RoundID            string       `json:"round_id"`
	SubjectHash        string       `json:"subject_hash"`
	Decision           GateDecision `json:"decision"`
	ReasonCodes        []string     `json:"reason_codes"`
	BlockingIDs        []string     `json:"blocking_ids"`
	ConflictIDs        []string     `json:"conflict_ids"`
	CoverageGaps       []string     `json:"coverage_gaps"`
	PolicyHash         string       `json:"policy_hash"`
	ReportHashes       []string     `json:"report_hashes"`
	AdjudicationHashes []string     `json:"adjudication_hashes"`
	ContentHash        string       `json:"content_hash"`
	CreatedAt          time.Time    `json:"created_at"`
}

// ReviewAdjudication is an immutable second-pass decision for one conflict group.
type ReviewAdjudication struct {
	ID             string                 `json:"id"`
	RoundID        string                 `json:"round_id"`
	SubjectHash    string                 `json:"subject_hash"`
	PolicyHash     string                 `json:"policy_hash"`
	Fingerprint    string                 `json:"fingerprint"`
	FindingIDs     []string               `json:"finding_ids"`
	Agent          agentapi.DefinitionRef `json:"agent"`
	DefinitionHash string                 `json:"definition_hash"`
	Decision       AdjudicationDecision   `json:"decision"`
	Rationale      string                 `json:"rationale"`
	ErrorCode      string                 `json:"error_code,omitempty"`
	ContentHash    string                 `json:"content_hash"`
	CreatedAt      time.Time              `json:"created_at"`
}

type FindingResolution struct {
	ID              string                `json:"id"`
	FindingID       string                `json:"finding_id"`
	Resolution      FindingResolutionKind `json:"resolution"`
	SubjectHash     string                `json:"subject_hash"`
	ReplacementHash string                `json:"replacement_hash,omitempty"`
	Rationale       string                `json:"rationale"`
	ActorID         int64                 `json:"actor_id"`
	ExpiresAt       *time.Time            `json:"expires_at,omitempty"`
	CreatedAt       time.Time             `json:"created_at"`
}

type FindingResolutionRequest struct {
	Resolution      FindingResolutionKind
	SubjectHash     string
	ReplacementHash string
	Rationale       string
	ExpiresAt       *time.Time
}

type ReviewEvaluation struct {
	Round               ReviewRound
	Policy              ReviewPolicy
	Assignments         []ReviewAssignment
	Reports             []ReviewReport
	Adjudications       []ReviewAdjudication
	Resolutions         []FindingResolution
	SubjectReasonCodes  []string
	SubjectBlockingIDs  []string
	SubjectCoverageGaps []string
}

type ReviewAssignmentCursor struct {
	CreatedAt time.Time
	ID        string
}

type ReviewRoundCursor struct {
	CreatedAt time.Time
	ID        string
}

type ReviewRoundFilter struct {
	FeatureID   string
	SubjectKind SubjectKind
	SubjectID   string
	Status      ReviewRoundStatus
}

type FindingCursor struct {
	ID string
}

type FindingResolutionCursor struct {
	CreatedAt time.Time
	ID        string
}

type ReviewAdjudicationCursor struct {
	Fingerprint string
	ID          string
}

// FindingSummary omits evidence so list reads remain bounded and narrow.
type FindingSummary struct {
	ID             string           `json:"id"`
	ReportID       string           `json:"report_id"`
	RoundID        string           `json:"round_id"`
	Category       string           `json:"category"`
	Severity       Severity         `json:"severity"`
	Claim          string           `json:"claim"`
	Impact         string           `json:"impact"`
	Location       *FindingLocation `json:"location,omitempty"`
	Recommendation string           `json:"recommendation"`
	Confidence     float64          `json:"confidence"`
	Fingerprint    string           `json:"fingerprint"`
	ContentHash    string           `json:"content_hash"`
	CreatedAt      time.Time        `json:"created_at"`
}

// FindingDetail adds the bounded evidence set for one finding.
type FindingDetail struct {
	FindingSummary
	Evidence []FindingEvidence `json:"evidence"`
}

type ReviewApprovalBinding struct {
	SubjectHash   string `json:"subject_hash"`
	ReviewRoundID string `json:"review_round_id"`
	GateResultID  string `json:"gate_result_id"`
}

func BuildArtifactReviewSubject(artifact Artifact) (ReviewSubject, error) {
	kind, err := subjectKindForArtifact(artifact.Kind)
	if err != nil {
		return ReviewSubject{}, err
	}
	evidenceHash, err := hashJSON(artifact.Evidence)
	if err != nil {
		return ReviewSubject{}, fmt.Errorf("hash artifact evidence: %w", err)
	}
	return prepareReviewSubject(ReviewSubject{
		Kind: kind, ID: artifact.ID, Version: artifact.Version,
		SourceContentHash: artifact.ContentHash, ParentHash: artifact.ParentArtifactID,
		EvidenceHash: evidenceHash,
	})
}

func BuildChangeSetReviewSubject(run ImplementationRun) (ReviewSubject, error) {
	if run.ChangeSet == nil {
		return ReviewSubject{}, fmt.Errorf("implementation %q has no change set: %w", run.ID, ErrConflict)
	}
	evidenceHash, err := hashJSON(struct {
		Files           []ChangedFile   `json:"files"`
		PlanDeviations  []PlanDeviation `json:"plan_deviations"`
		ProviderSummary string          `json:"provider_summary"`
	}{
		Files: run.ChangeSet.Files, PlanDeviations: run.ChangeSet.PlanDeviations,
		ProviderSummary: run.ChangeSet.ProviderSummary,
	})
	if err != nil {
		return ReviewSubject{}, fmt.Errorf("hash change set evidence: %w", err)
	}
	return prepareReviewSubject(ReviewSubject{
		Kind: SubjectChangeSet, ID: run.ID, Version: 1,
		SourceContentHash: run.ChangeSet.PatchSHA256, EvidenceHash: evidenceHash,
		Repository: run.Repo, BaseCommit: run.BaseCommit, HeadCommit: run.ChangeSet.WorktreeHead,
	})
}

// BuildValidationBundleReviewSubject keeps independent validation separate from code review.
func BuildValidationBundleReviewSubject(run ImplementationRun) (ReviewSubject, error) {
	changeSubject, err := BuildChangeSetReviewSubject(run)
	if err != nil {
		return ReviewSubject{}, err
	}
	validationHash, err := hashValidationResults(run.ChangeSet.ValidationResults)
	if err != nil {
		return ReviewSubject{}, err
	}
	return prepareReviewSubject(ReviewSubject{
		Kind: SubjectValidationBundle, ID: run.ID, Version: 1,
		SourceContentHash: validationHash, ParentHash: changeSubject.ContentHash,
		Repository: run.Repo, BaseCommit: run.BaseCommit, HeadCommit: run.ChangeSet.WorktreeHead,
	})
}

// BuildDeliveryBundleReviewSubject binds release readiness to exact design, plan, change, and validation inputs.
func BuildDeliveryBundleReviewSubject(
	run ImplementationRun,
	design Artifact,
	plan Artifact,
) (ReviewSubject, error) {
	if design.ID != run.DesignArtifactID || plan.ID != run.PlanArtifactID ||
		design.RequestID != run.RequestID || plan.RequestID != run.RequestID ||
		design.Kind != KindSystemDesign || plan.Kind != KindImplementationPlan ||
		plan.ParentArtifactID != design.ID {
		return ReviewSubject{}, fmt.Errorf("delivery artifacts do not match implementation %q: %w", run.ID, ErrConflict)
	}
	changeSubject, err := BuildChangeSetReviewSubject(run)
	if err != nil {
		return ReviewSubject{}, err
	}
	validationSubject, err := BuildValidationBundleReviewSubject(run)
	if err != nil {
		return ReviewSubject{}, err
	}
	sourceHash, err := hashJSON(struct {
		DesignHash     string `json:"design_hash"`
		PlanHash       string `json:"plan_hash"`
		ChangeSetHash  string `json:"change_set_hash"`
		ValidationHash string `json:"validation_hash"`
	}{
		DesignHash: design.ContentHash, PlanHash: plan.ContentHash,
		ChangeSetHash: changeSubject.ContentHash, ValidationHash: validationSubject.ContentHash,
	})
	if err != nil {
		return ReviewSubject{}, fmt.Errorf("hash delivery bundle: %w", err)
	}
	evidenceHash, err := hashJSON(struct {
		DesignArtifactID string `json:"design_artifact_id"`
		PlanArtifactID   string `json:"plan_artifact_id"`
	}{
		DesignArtifactID: design.ID,
		PlanArtifactID:   plan.ID,
	})
	if err != nil {
		return ReviewSubject{}, fmt.Errorf("hash delivery evidence: %w", err)
	}
	return prepareReviewSubject(ReviewSubject{
		Kind: SubjectDeliveryBundle, ID: run.ID, Version: 1,
		SourceContentHash: sourceHash, ParentHash: validationSubject.ContentHash,
		EvidenceHash: evidenceHash,
		Repository:   run.Repo, BaseCommit: run.BaseCommit, HeadCommit: run.ChangeSet.WorktreeHead,
	})
}

func hashValidationResults(results []ValidationResult) (string, error) {
	if len(results) == 0 || len(results) > maxValidationCommands {
		return "", fmt.Errorf("validation bundle is empty: %w", ErrConflict)
	}
	for index, result := range results {
		hasOutput := result.OutputRelPath != ""
		if result.Sequence != index+1 || !validValidationStatus(result.Status) ||
			(hasOutput && (result.OutputBytes < 0 || result.OutputBytes > maxValidationOutput ||
				len(result.OutputSHA256) != sha256.Size*2 || !isHex(result.OutputSHA256))) ||
			(!hasOutput && (result.OutputSHA256 != "" || result.OutputBytes != 0)) {
			return "", fmt.Errorf("validation result %d is invalid: %w", index, ErrConflict)
		}
	}
	hash, err := hashJSON(results)
	if err != nil {
		return "", fmt.Errorf("hash validation bundle: %w", err)
	}
	return hash, nil
}

func validValidationStatus(status string) bool {
	switch status {
	case "passed", "failed", "validation_not_configured":
		return true
	default:
		return false
	}
}

func PrepareReviewPolicy(policy ReviewPolicy) (ReviewPolicy, error) {
	prepared := policy
	prepared.ID = strings.TrimSpace(prepared.ID)
	prepared.ContentHash = ""
	prepared.Reviewers = append([]ReviewerSpec(nil), policy.Reviewers...)
	prepared.RiskRuleVersion = strings.TrimSpace(policy.RiskRuleVersion)
	prepared.RiskRules = make([]ReviewRiskRule, len(policy.RiskRules))
	for index, rule := range policy.RiskRules {
		prepared.RiskRules[index] = rule
		prepared.RiskRules[index].Conditions = append(
			[]ReviewRiskCondition(nil),
			rule.Conditions...,
		)
		prepared.RiskRules[index].ReviewerIDs = append(
			[]string(nil),
			rule.ReviewerIDs...,
		)
	}
	if policy.Adjudicator != nil {
		adjudicator := *policy.Adjudicator
		prepared.Adjudicator = &adjudicator
	}
	prepared.BlockingSeverities = append([]Severity(nil), policy.BlockingSeverities...)
	prepared.RequiredCategories = canonicalStrings(policy.RequiredCategories)
	if prepared.ID == "" || prepared.Version <= 0 {
		return ReviewPolicy{}, fmt.Errorf("review policy id and positive version are required: %w", ErrInvalid)
	}
	if !validSubjectKind(prepared.SubjectKind) {
		return ReviewPolicy{}, fmt.Errorf("review policy subject kind %q is invalid: %w", prepared.SubjectKind, ErrInvalid)
	}
	if len(prepared.Reviewers) < 2 || len(prepared.Reviewers) > maxReviewersPerPolicy {
		return ReviewPolicy{}, fmt.Errorf("review policy requires 2-%d reviewers: %w", maxReviewersPerPolicy, ErrInvalid)
	}
	if prepared.MaxParallelism <= 0 || prepared.MaxParallelism > len(prepared.Reviewers) {
		return ReviewPolicy{}, fmt.Errorf("review policy parallelism is invalid: %w", ErrInvalid)
	}
	reviewerIDs := make(map[string]struct{}, len(prepared.Reviewers))
	required := 0
	for index := range prepared.Reviewers {
		reviewer := &prepared.Reviewers[index]
		reviewer.ID = strings.TrimSpace(reviewer.ID)
		reviewer.DefinitionHash = strings.TrimSpace(reviewer.DefinitionHash)
		reviewer.Categories = canonicalStrings(reviewer.Categories)
		if reviewer.ID == "" || reviewer.Agent.ID == "" || reviewer.Agent.Version <= 0 || reviewer.DefinitionHash == "" {
			return ReviewPolicy{}, fmt.Errorf("reviewer %d is incomplete: %w", index, ErrInvalid)
		}
		if !reviewer.ReadOnly {
			return ReviewPolicy{}, fmt.Errorf("reviewer %q must be read-only: %w", reviewer.ID, ErrInvalid)
		}
		if len(reviewer.Categories) == 0 {
			return ReviewPolicy{}, fmt.Errorf("reviewer %q has no categories: %w", reviewer.ID, ErrInvalid)
		}
		if _, duplicate := reviewerIDs[reviewer.ID]; duplicate {
			return ReviewPolicy{}, fmt.Errorf("duplicate reviewer %q: %w", reviewer.ID, ErrInvalid)
		}
		reviewerIDs[reviewer.ID] = struct{}{}
		if reviewer.Required {
			required++
		}
	}
	if required < 2 {
		return ReviewPolicy{}, fmt.Errorf("review policy requires at least two required reviewers: %w", ErrInvalid)
	}
	if err := prepareReviewRiskRules(&prepared, reviewerIDs); err != nil {
		return ReviewPolicy{}, err
	}
	if prepared.Adjudicator != nil {
		adjudicator := prepared.Adjudicator
		adjudicator.Agent.ID = strings.TrimSpace(adjudicator.Agent.ID)
		adjudicator.DefinitionHash = strings.TrimSpace(adjudicator.DefinitionHash)
		if adjudicator.Agent.ID == "" || adjudicator.Agent.Version <= 0 ||
			adjudicator.DefinitionHash == "" {
			return ReviewPolicy{}, fmt.Errorf("review policy adjudicator is incomplete: %w", ErrInvalid)
		}
		if !adjudicator.ReadOnly {
			return ReviewPolicy{}, fmt.Errorf("review policy adjudicator must be read-only: %w", ErrInvalid)
		}
	}
	budgetedRuns := int64(len(prepared.Reviewers))
	if prepared.Adjudicator != nil {
		budgetedRuns++
	}
	if prepared.MaxInputTokens < budgetedRuns ||
		prepared.MaxOutputTokens < budgetedRuns ||
		prepared.MaxTotalTokens < budgetedRuns ||
		prepared.MaxToolCalls < budgetedRuns ||
		prepared.MaxCostMicros < budgetedRuns ||
		prepared.MaxRetries < 0 ||
		prepared.Timeout <= 0 {
		return ReviewPolicy{}, fmt.Errorf("review policy round budgets are invalid: %w", ErrInvalid)
	}
	switch prepared.OptionalReviewerAction {
	case OptionalReviewerContinue, OptionalReviewerHumanRequired:
	default:
		return ReviewPolicy{}, fmt.Errorf(
			"review policy optional reviewer action %q is invalid: %w",
			prepared.OptionalReviewerAction,
			ErrInvalid,
		)
	}
	if len(prepared.RequiredCategories) == 0 {
		return ReviewPolicy{}, fmt.Errorf("review policy required categories are empty: %w", ErrInvalid)
	}
	severities := make(map[Severity]struct{}, len(prepared.BlockingSeverities))
	for _, severity := range prepared.BlockingSeverities {
		if !validSeverity(severity) {
			return ReviewPolicy{}, fmt.Errorf("blocking severity %q is invalid: %w", severity, ErrInvalid)
		}
		severities[severity] = struct{}{}
	}
	if _, ok := severities[SeverityCritical]; !ok {
		return ReviewPolicy{}, fmt.Errorf("critical findings must block: %w", ErrInvalid)
	}
	if _, ok := severities[SeverityHigh]; !ok {
		return ReviewPolicy{}, fmt.Errorf("high findings must block: %w", ErrInvalid)
	}
	hashInput := prepared
	hashInput.CreatedAt = time.Time{}
	hash, err := hashJSON(hashInput)
	if err != nil {
		return ReviewPolicy{}, fmt.Errorf("hash review policy %q: %w", prepared.ID, err)
	}
	if policy.ContentHash != "" && policy.ContentHash != hash {
		return ReviewPolicy{}, fmt.Errorf("review policy %q content hash mismatch: %w", prepared.ID, ErrConflict)
	}
	prepared.ContentHash = hash
	return prepared, nil
}

// PrepareReviewAdjudication validates and hashes one detached immutable decision.
func PrepareReviewAdjudication(adjudication ReviewAdjudication) (ReviewAdjudication, error) {
	prepared := adjudication
	prepared.ID = ""
	prepared.ContentHash = ""
	prepared.RoundID = strings.TrimSpace(prepared.RoundID)
	prepared.SubjectHash = strings.TrimSpace(prepared.SubjectHash)
	prepared.PolicyHash = strings.TrimSpace(prepared.PolicyHash)
	prepared.Fingerprint = strings.TrimSpace(prepared.Fingerprint)
	prepared.Agent.ID = strings.TrimSpace(prepared.Agent.ID)
	prepared.DefinitionHash = strings.TrimSpace(prepared.DefinitionHash)
	prepared.FindingIDs = canonicalStrings(prepared.FindingIDs)
	prepared.Rationale = redactReviewText(prepared.Rationale)
	prepared.ErrorCode = redactReviewText(prepared.ErrorCode)
	if prepared.RoundID == "" || prepared.SubjectHash == "" || prepared.PolicyHash == "" ||
		prepared.Fingerprint == "" || prepared.Agent.ID == "" ||
		prepared.Agent.Version <= 0 || prepared.DefinitionHash == "" {
		return ReviewAdjudication{}, fmt.Errorf("review adjudication snapshot is incomplete: %w", ErrInvalid)
	}
	if len(prepared.FindingIDs) < 2 {
		return ReviewAdjudication{}, fmt.Errorf("review adjudication requires a conflict group: %w", ErrInvalid)
	}
	if !validAdjudicationDecision(prepared.Decision) || prepared.Rationale == "" {
		return ReviewAdjudication{}, fmt.Errorf("review adjudication decision or rationale is invalid: %w", ErrInvalid)
	}
	if prepared.Decision != AdjudicationNeedsHuman && prepared.ErrorCode != "" {
		return ReviewAdjudication{}, fmt.Errorf("review adjudication error code requires human decision: %w", ErrInvalid)
	}
	hashInput := prepared
	hashInput.CreatedAt = time.Time{}
	hash, err := hashJSON(hashInput)
	if err != nil {
		return ReviewAdjudication{}, fmt.Errorf("hash review adjudication: %w", err)
	}
	if adjudication.ContentHash != "" && adjudication.ContentHash != hash {
		return ReviewAdjudication{}, fmt.Errorf("review adjudication content hash mismatch: %w", ErrConflict)
	}
	prepared.ContentHash = hash
	prepared.ID = "adjudication_" + hash[:24]
	if adjudication.ID != "" && adjudication.ID != prepared.ID {
		return ReviewAdjudication{}, fmt.Errorf("review adjudication id mismatch: %w", ErrConflict)
	}
	return prepared, nil
}

func PrepareReviewReport(report ReviewReport, assignment ReviewAssignment, subjectHash string) (ReviewReport, error) {
	if report.Reuse != nil {
		return ReviewReport{}, fmt.Errorf("review report reuse is server-owned: %w", ErrInvalid)
	}
	return prepareReviewReport(report, assignment, subjectHash)
}

// PrepareReusedReviewReport materializes source content under a new Round identity.
func PrepareReusedReviewReport(
	source ReviewReport,
	assignment ReviewAssignment,
	subjectHash string,
	reuse ReviewReportReuseRef,
	completedAt time.Time,
) (ReviewReport, error) {
	if assignment.Status != AssignmentQueued {
		return ReviewReport{}, fmt.Errorf(
			"assignment %q is not queued for reuse: %w",
			assignment.ID,
			ErrConflict,
		)
	}
	reuse.SourceReportID = strings.TrimSpace(reuse.SourceReportID)
	reuse.SourceRoundID = strings.TrimSpace(reuse.SourceRoundID)
	reuse.SourceAssignmentID = strings.TrimSpace(reuse.SourceAssignmentID)
	reuse.Reason = redactReviewText(reuse.Reason)
	if reuse.SourceReportID == "" || reuse.SourceRoundID == "" ||
		reuse.SourceAssignmentID == "" || reuse.Reason == "" {
		return ReviewReport{}, fmt.Errorf("review report reuse source is incomplete: %w", ErrInvalid)
	}
	if source.ID != reuse.SourceReportID ||
		source.RoundID != reuse.SourceRoundID ||
		source.AssignmentID != reuse.SourceAssignmentID ||
		source.SubjectHash != subjectHash ||
		source.ReportHash == "" {
		return ReviewReport{}, fmt.Errorf("review report reuse source does not match: %w", ErrConflict)
	}
	sourceHash, err := reviewReportHash(source)
	if err != nil {
		return ReviewReport{}, err
	}
	if sourceHash != source.ReportHash {
		return ReviewReport{}, fmt.Errorf("source review report hash mismatch: %w", ErrConflict)
	}
	report := source
	report.ID = ""
	report.RoundID = assignment.RoundID
	report.AssignmentID = assignment.ID
	report.ReviewerID = assignment.ReviewerID
	report.SubjectHash = subjectHash
	report.ReportHash = ""
	report.ContentHash = ""
	report.Reuse = &reuse
	report.CompletedAt = completedAt
	running := assignment
	running.Status = AssignmentRunning
	prepared, err := prepareReviewReport(report, running, subjectHash)
	if err != nil {
		return ReviewReport{}, err
	}
	if prepared.ReportHash != source.ReportHash {
		return ReviewReport{}, fmt.Errorf("reused review report content changed: %w", ErrConflict)
	}
	return prepared, nil
}

// ValidateReviewReportSnapshot verifies a persisted Report against its Assignment.
func ValidateReviewReportSnapshot(
	report ReviewReport,
	assignment ReviewAssignment,
	subjectHash string,
) error {
	running := assignment
	running.Status = AssignmentRunning
	prepared, err := prepareReviewReport(report, running, subjectHash)
	if err != nil {
		return err
	}
	if prepared.ID != report.ID ||
		prepared.ReportHash != report.ReportHash ||
		prepared.ContentHash != report.ContentHash {
		return fmt.Errorf("review report snapshot changed during validation: %w", ErrConflict)
	}
	return nil
}

func prepareReviewReport(
	report ReviewReport,
	assignment ReviewAssignment,
	subjectHash string,
) (ReviewReport, error) {
	prepared := report
	requestedID := strings.TrimSpace(report.ID)
	prepared.ID = ""
	prepared.ReportHash = ""
	prepared.ContentHash = ""
	prepared.Coverage = append([]CoverageItem(nil), report.Coverage...)
	prepared.Findings = append([]Finding(nil), report.Findings...)
	prepared.Uncertainties = append([]Uncertainty(nil), report.Uncertainties...)
	if report.Reuse != nil {
		reuse := *report.Reuse
		reuse.SourceReportID = strings.TrimSpace(reuse.SourceReportID)
		reuse.SourceRoundID = strings.TrimSpace(reuse.SourceRoundID)
		reuse.SourceAssignmentID = strings.TrimSpace(reuse.SourceAssignmentID)
		reuse.Reason = redactReviewText(reuse.Reason)
		if reuse.SourceReportID == "" || reuse.SourceRoundID == "" ||
			reuse.SourceAssignmentID == "" || reuse.Reason == "" {
			return ReviewReport{}, fmt.Errorf("review report reuse source is incomplete: %w", ErrInvalid)
		}
		prepared.Reuse = &reuse
	}
	if prepared.RoundID != assignment.RoundID || prepared.AssignmentID != assignment.ID ||
		prepared.ReviewerID != assignment.ReviewerID || prepared.SubjectHash != subjectHash {
		return ReviewReport{}, fmt.Errorf("review report assignment or subject hash mismatch: %w", ErrConflict)
	}
	if assignment.Status != AssignmentRunning {
		return ReviewReport{}, fmt.Errorf("assignment %q is not running: %w", assignment.ID, ErrConflict)
	}
	if prepared.CompletedAt.IsZero() {
		return ReviewReport{}, fmt.Errorf("review report completion time is required: %w", ErrInvalid)
	}
	prepared.Summary = redactReviewText(prepared.Summary)
	seenCoverage := make(map[string]struct{}, len(prepared.Coverage))
	for index := range prepared.Coverage {
		prepared.Coverage[index].Category = redactReviewText(prepared.Coverage[index].Category)
		prepared.Coverage[index].Summary = redactReviewText(prepared.Coverage[index].Summary)
		if prepared.Coverage[index].Category == "" {
			return ReviewReport{}, fmt.Errorf("coverage category is required: %w", ErrInvalid)
		}
		if _, duplicate := seenCoverage[prepared.Coverage[index].Category]; duplicate {
			return ReviewReport{}, fmt.Errorf("duplicate coverage category %q: %w", prepared.Coverage[index].Category, ErrInvalid)
		}
		seenCoverage[prepared.Coverage[index].Category] = struct{}{}
	}
	for index := range prepared.Uncertainties {
		prepared.Uncertainties[index].Category = redactReviewText(
			prepared.Uncertainties[index].Category,
		)
		prepared.Uncertainties[index].Summary = redactReviewText(
			prepared.Uncertainties[index].Summary,
		)
	}
	if len(prepared.Findings) > maxFindingsPerReport {
		return ReviewReport{}, fmt.Errorf("review report exceeds %d findings: %w", maxFindingsPerReport, ErrInvalid)
	}
	seenFingerprints := make(map[string]struct{}, len(prepared.Findings))
	for index := range prepared.Findings {
		finding, err := prepareFinding(prepared.Findings[index], assignment.ID)
		if err != nil {
			return ReviewReport{}, fmt.Errorf("finding %d: %w", index, err)
		}
		if _, duplicate := seenFingerprints[finding.Fingerprint]; duplicate {
			return ReviewReport{}, fmt.Errorf(
				"duplicate finding fingerprint %q: %w",
				finding.Fingerprint,
				ErrInvalid,
			)
		}
		seenFingerprints[finding.Fingerprint] = struct{}{}
		prepared.Findings[index] = finding
	}
	sort.Slice(prepared.Findings, func(i, j int) bool {
		return prepared.Findings[i].ID < prepared.Findings[j].ID
	})
	reportHash, err := reviewReportHash(prepared)
	if err != nil {
		return ReviewReport{}, err
	}
	if report.ReportHash != "" && report.ReportHash != reportHash {
		return ReviewReport{}, fmt.Errorf("review report hash mismatch: %w", ErrConflict)
	}
	prepared.ReportHash = reportHash
	hash, err := hashJSON(prepared)
	if err != nil {
		return ReviewReport{}, fmt.Errorf("hash review report: %w", err)
	}
	if report.ContentHash != "" && report.ContentHash != hash {
		return ReviewReport{}, fmt.Errorf("review report content hash mismatch: %w", ErrConflict)
	}
	prepared.ContentHash = hash
	prepared.ID = "report_" + hash[:24]
	if requestedID != "" && requestedID != prepared.ID {
		return ReviewReport{}, fmt.Errorf("review report id mismatch: %w", ErrConflict)
	}
	for index := range prepared.Findings {
		prepared.Findings[index].ReportID = prepared.ID
	}
	return prepared, nil
}

func reviewReportHash(report ReviewReport) (string, error) {
	coverage := append([]CoverageItem(nil), report.Coverage...)
	sort.Slice(coverage, func(i, j int) bool {
		if coverage[i].Category != coverage[j].Category {
			return coverage[i].Category < coverage[j].Category
		}
		if coverage[i].Covered != coverage[j].Covered {
			return !coverage[i].Covered
		}
		return coverage[i].Summary < coverage[j].Summary
	})
	uncertainties := append([]Uncertainty(nil), report.Uncertainties...)
	sort.Slice(uncertainties, func(i, j int) bool {
		if uncertainties[i].Category != uncertainties[j].Category {
			return uncertainties[i].Category < uncertainties[j].Category
		}
		return uncertainties[i].Summary < uncertainties[j].Summary
	})
	type semanticFinding struct {
		Category       string            `json:"category"`
		Severity       Severity          `json:"severity"`
		Claim          string            `json:"claim"`
		Impact         string            `json:"impact"`
		Evidence       []FindingEvidence `json:"evidence"`
		Location       *FindingLocation  `json:"location,omitempty"`
		Recommendation string            `json:"recommendation"`
		Confidence     float64           `json:"confidence"`
		Fingerprint    string            `json:"fingerprint"`
	}
	findings := make([]semanticFinding, 0, len(report.Findings))
	for _, finding := range report.Findings {
		findings = append(findings, semanticFinding{
			Category: finding.Category, Severity: finding.Severity,
			Claim: finding.Claim, Impact: finding.Impact,
			Evidence: append([]FindingEvidence(nil), finding.Evidence...),
			Location: finding.Location, Recommendation: finding.Recommendation,
			Confidence: finding.Confidence, Fingerprint: finding.Fingerprint,
		})
	}
	sort.Slice(findings, func(i, j int) bool {
		return findings[i].Fingerprint < findings[j].Fingerprint
	})
	hash, err := hashJSON(struct {
		Coverage      []CoverageItem    `json:"coverage"`
		Findings      []semanticFinding `json:"findings"`
		Uncertainties []Uncertainty     `json:"uncertainties"`
		Summary       string            `json:"summary"`
	}{
		Coverage: coverage, Findings: findings,
		Uncertainties: uncertainties, Summary: report.Summary,
	})
	if err != nil {
		return "", fmt.Errorf("hash review report content: %w", err)
	}
	return hash, nil
}

func CanTransitionReviewRound(from, to ReviewRoundStatus) bool {
	switch from {
	case RoundCreated:
		return to == RoundRunning || to == RoundFailed || to == RoundCancelled
	case RoundRunning:
		return to == RoundEvaluating || to == RoundFailed || to == RoundCancelled
	case RoundEvaluating:
		return to == RoundCompleted || to == RoundFailed || to == RoundCancelled
	default:
		return false
	}
}

func CanTransitionReviewAssignment(from, to ReviewAssignmentStatus) bool {
	switch from {
	case AssignmentQueued:
		return to == AssignmentRunning || to == AssignmentReused || to == AssignmentCancelled
	case AssignmentRunning:
		return to == AssignmentSucceeded || to == AssignmentFailed || to == AssignmentCancelled
	default:
		return false
	}
}

func successfulReviewAssignment(status ReviewAssignmentStatus) bool {
	return status == AssignmentSucceeded || status == AssignmentReused
}

// CanAppendReviewEvent keeps the audit stream aligned with the persisted Round lifecycle.
func CanAppendReviewEvent(kind ReviewEventKind, status ReviewRoundStatus) bool {
	switch kind {
	case ReviewEventRoundStarted, ReviewEventAssignmentStarted,
		ReviewEventAssignmentSucceeded, ReviewEventAssignmentFailed:
		return status == RoundRunning
	case ReviewEventRoundEvaluating:
		return status == RoundEvaluating
	case ReviewEventAdjudicationStarted, ReviewEventAdjudicationFinished:
		return status == RoundEvaluating
	case ReviewEventRoundCompleted:
		return status == RoundCompleted
	case ReviewEventRoundFailed:
		return status == RoundFailed
	case ReviewEventRoundCancelled:
		return status == RoundCancelled
	default:
		return false
	}
}

// ParseSubjectKind canonicalizes an untrusted transport value.
func ParseSubjectKind(value string) (SubjectKind, error) {
	kind := SubjectKind(strings.ToLower(strings.TrimSpace(value)))
	if !validSubjectKind(kind) {
		return "", fmt.Errorf("review subject kind %q is invalid: %w", value, ErrInvalid)
	}
	return kind, nil
}

// ParseSeverity canonicalizes an optional transport filter.
func ParseSeverity(value string) (Severity, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	severity := Severity(strings.ToLower(strings.TrimSpace(value)))
	if !validSeverity(severity) {
		return "", fmt.Errorf("review finding severity %q is invalid: %w", value, ErrInvalid)
	}
	return severity, nil
}

// ParseOptionalReviewerAction canonicalizes an untrusted policy value.
func ParseOptionalReviewerAction(value string) (OptionalReviewerAction, error) {
	action := OptionalReviewerAction(strings.ToLower(strings.TrimSpace(value)))
	switch action {
	case OptionalReviewerContinue, OptionalReviewerHumanRequired:
		return action, nil
	default:
		return "", fmt.Errorf("optional reviewer action %q is invalid: %w", value, ErrInvalid)
	}
}

// ParseFindingResolutionKind canonicalizes an untrusted disposition value.
func ParseFindingResolutionKind(value string) (FindingResolutionKind, error) {
	kind := FindingResolutionKind(strings.ToLower(strings.TrimSpace(value)))
	if !validFindingResolutionKind(kind) {
		return "", fmt.Errorf("finding resolution %q is invalid: %w", value, ErrInvalid)
	}
	return kind, nil
}

func validGateDecision(decision GateDecision) bool {
	switch decision {
	case GatePass, GateRevise, GateHumanRequired, GateIncomplete, GateFailed:
		return true
	default:
		return false
	}
}

func validAdjudicationDecision(decision AdjudicationDecision) bool {
	switch decision {
	case AdjudicationConfirmed, AdjudicationNotSupported,
		AdjudicationDistinctFindings, AdjudicationNeedsHuman:
		return true
	default:
		return false
	}
}

func validFindingResolutionKind(kind FindingResolutionKind) bool {
	switch kind {
	case ResolutionFixed, ResolutionWaived, ResolutionInvalidated, ResolutionSuperseded:
		return true
	default:
		return false
	}
}

func prepareReviewSubject(subject ReviewSubject) (ReviewSubject, error) {
	subject.ContentHash = ""
	if !validSubjectKind(subject.Kind) || strings.TrimSpace(subject.ID) == "" ||
		subject.Version <= 0 || strings.TrimSpace(subject.SourceContentHash) == "" {
		return ReviewSubject{}, fmt.Errorf("review subject is incomplete: %w", ErrInvalid)
	}
	hash, err := hashJSON(subject)
	if err != nil {
		return ReviewSubject{}, fmt.Errorf("hash review subject: %w", err)
	}
	subject.ContentHash = hash
	return subject, nil
}

func prepareFinding(finding Finding, assignmentID string) (Finding, error) {
	prepared := finding
	prepared.ID = ""
	prepared.ReportID = ""
	prepared.ContentHash = ""
	prepared.Category = redactReviewText(prepared.Category)
	prepared.Claim = redactReviewText(prepared.Claim)
	prepared.Impact = redactReviewText(prepared.Impact)
	prepared.Recommendation = redactReviewText(prepared.Recommendation)
	prepared.Evidence = append([]FindingEvidence(nil), finding.Evidence...)
	if prepared.Location != nil {
		location := *prepared.Location
		location.Path = redactReviewText(location.Path)
		location.Field = redactReviewText(location.Field)
		prepared.Location = &location
	}
	if len(prepared.Evidence) > maxEvidencePerFinding {
		return Finding{}, fmt.Errorf("finding exceeds %d evidence items: %w", maxEvidencePerFinding, ErrInvalid)
	}
	if prepared.Category == "" || prepared.Claim == "" || prepared.Impact == "" || prepared.Recommendation == "" {
		return Finding{}, fmt.Errorf("category, claim, impact, and recommendation are required: %w", ErrInvalid)
	}
	if !validSeverity(prepared.Severity) || prepared.Confidence < 0 || prepared.Confidence > 1 {
		return Finding{}, fmt.Errorf("severity or confidence is invalid: %w", ErrInvalid)
	}
	for index := range prepared.Evidence {
		evidence := &prepared.Evidence[index]
		evidence.Kind = redactReviewText(evidence.Kind)
		evidence.Ref = redactReviewText(evidence.Ref)
		evidence.Hash = strings.TrimSpace(evidence.Hash)
		evidence.Summary = redactReviewText(evidence.Summary)
		if evidence.Kind == "" || evidence.Ref == "" || evidence.Summary == "" {
			return Finding{}, fmt.Errorf("evidence %d is incomplete: %w", index, ErrInvalid)
		}
	}
	fingerprint, err := hashJSON(struct {
		Category string           `json:"category"`
		Location *FindingLocation `json:"location,omitempty"`
		Claim    string           `json:"claim"`
	}{
		prepared.Category,
		prepared.Location,
		strings.ToLower(strings.Join(strings.Fields(prepared.Claim), " ")),
	})
	if err != nil {
		return Finding{}, err
	}
	prepared.Fingerprint = fingerprint
	hash, err := hashJSON(struct {
		AssignmentID string  `json:"assignment_id"`
		Finding      Finding `json:"finding"`
	}{assignmentID, prepared})
	if err != nil {
		return Finding{}, err
	}
	prepared.ContentHash = hash
	prepared.ID = "finding_" + hash[:24]
	return prepared, nil
}

func redactReviewText(value string) string {
	return strings.TrimSpace(platform.RedactSensitiveText(value))
}

func subjectKindForArtifact(kind ArtifactKind) (SubjectKind, error) {
	switch kind {
	case KindRequirement:
		return SubjectRequirement, nil
	case KindRequirementAnalysis:
		return SubjectRequirementAnalysis, nil
	case KindTechnicalProposal:
		return SubjectTechnicalProposal, nil
	case KindSystemDesign:
		return SubjectSystemDesign, nil
	case KindImplementationPlan:
		return SubjectImplementationPlan, nil
	default:
		return "", fmt.Errorf("artifact kind %q cannot be reviewed: %w", kind, ErrInvalid)
	}
}

func validSubjectKind(kind SubjectKind) bool {
	switch kind {
	case SubjectRequirement, SubjectRequirementAnalysis, SubjectTechnicalProposal,
		SubjectSystemDesign, SubjectImplementationPlan, SubjectChangeSet,
		SubjectValidationBundle, SubjectDeliveryBundle:
		return true
	default:
		return false
	}
}

func validReviewRoundStatus(status ReviewRoundStatus) bool {
	switch status {
	case RoundCreated, RoundRunning, RoundEvaluating, RoundCompleted, RoundFailed, RoundCancelled:
		return true
	default:
		return false
	}
}

func validSeverity(severity Severity) bool {
	switch severity {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow, SeverityInfo:
		return true
	default:
		return false
	}
}

func canonicalStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func hashJSON(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
