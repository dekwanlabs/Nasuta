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

// ReviewPolicy is immutable after PrepareReviewPolicy computes its hash.
type ReviewPolicy struct {
	ID                 string         `json:"id"`
	Version            int64          `json:"version"`
	SubjectKind        SubjectKind    `json:"subject_kind"`
	Reviewers          []ReviewerSpec `json:"reviewers"`
	BlockingSeverities []Severity     `json:"blocking_severities"`
	RequiredCategories []string       `json:"required_categories"`
	MaxParallelism     int            `json:"max_parallelism"`
	ContentHash        string         `json:"content_hash"`
	CreatedAt          time.Time      `json:"created_at"`
}

type ReviewRound struct {
	ID            string            `json:"id"`
	Subject       ReviewSubject     `json:"subject"`
	PolicyID      string            `json:"policy_id"`
	PolicyVersion int64             `json:"policy_version"`
	PolicyHash    string            `json:"policy_hash"`
	Status        ReviewRoundStatus `json:"status"`
	CreatedBy     int64             `json:"created_by"`
	CreatedAt     time.Time         `json:"created_at"`
	CompletedAt   *time.Time        `json:"completed_at,omitempty"`
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
	ID            string         `json:"id"`
	RoundID       string         `json:"round_id"`
	AssignmentID  string         `json:"assignment_id"`
	ReviewerID    string         `json:"reviewer_id"`
	SubjectHash   string         `json:"subject_hash"`
	Coverage      []CoverageItem `json:"coverage"`
	Findings      []Finding      `json:"findings"`
	Uncertainties []Uncertainty  `json:"uncertainties"`
	Summary       string         `json:"summary"`
	ContentHash   string         `json:"content_hash"`
	CompletedAt   time.Time      `json:"completed_at"`
}

type ReviewGateResult struct {
	ID           string       `json:"id"`
	RoundID      string       `json:"round_id"`
	SubjectHash  string       `json:"subject_hash"`
	Decision     GateDecision `json:"decision"`
	ReasonCodes  []string     `json:"reason_codes"`
	BlockingIDs  []string     `json:"blocking_ids"`
	ConflictIDs  []string     `json:"conflict_ids"`
	CoverageGaps []string     `json:"coverage_gaps"`
	PolicyHash   string       `json:"policy_hash"`
	ReportHashes []string     `json:"report_hashes"`
	ContentHash  string       `json:"content_hash"`
	CreatedAt    time.Time    `json:"created_at"`
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

type ReviewEvaluation struct {
	Round       ReviewRound
	Policy      ReviewPolicy
	Assignments []ReviewAssignment
	Reports     []ReviewReport
	Resolutions []FindingResolution
}

type ReviewAssignmentCursor struct {
	CreatedAt time.Time
	ID        string
}

type FindingCursor struct {
	ID string
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
		Files             []ChangedFile      `json:"files"`
		PlanDeviations    []PlanDeviation    `json:"plan_deviations"`
		ValidationResults []ValidationResult `json:"validation_results"`
		ProviderSummary   string             `json:"provider_summary"`
	}{
		Files: run.ChangeSet.Files, PlanDeviations: run.ChangeSet.PlanDeviations,
		ValidationResults: run.ChangeSet.ValidationResults, ProviderSummary: run.ChangeSet.ProviderSummary,
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

func PrepareReviewPolicy(policy ReviewPolicy) (ReviewPolicy, error) {
	prepared := policy
	prepared.ID = strings.TrimSpace(prepared.ID)
	prepared.ContentHash = ""
	prepared.Reviewers = append([]ReviewerSpec(nil), policy.Reviewers...)
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
	hash, err := hashJSON(prepared)
	if err != nil {
		return ReviewPolicy{}, fmt.Errorf("hash review policy %q: %w", prepared.ID, err)
	}
	if policy.ContentHash != "" && policy.ContentHash != hash {
		return ReviewPolicy{}, fmt.Errorf("review policy %q content hash mismatch: %w", prepared.ID, ErrConflict)
	}
	prepared.ContentHash = hash
	return prepared, nil
}

func PrepareReviewReport(report ReviewReport, assignment ReviewAssignment, subjectHash string) (ReviewReport, error) {
	prepared := report
	prepared.ContentHash = ""
	prepared.Coverage = append([]CoverageItem(nil), report.Coverage...)
	prepared.Findings = append([]Finding(nil), report.Findings...)
	prepared.Uncertainties = append([]Uncertainty(nil), report.Uncertainties...)
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
	seenCoverage := make(map[string]struct{}, len(prepared.Coverage))
	for index := range prepared.Coverage {
		prepared.Coverage[index].Category = strings.TrimSpace(prepared.Coverage[index].Category)
		if prepared.Coverage[index].Category == "" {
			return ReviewReport{}, fmt.Errorf("coverage category is required: %w", ErrInvalid)
		}
		if _, duplicate := seenCoverage[prepared.Coverage[index].Category]; duplicate {
			return ReviewReport{}, fmt.Errorf("duplicate coverage category %q: %w", prepared.Coverage[index].Category, ErrInvalid)
		}
		seenCoverage[prepared.Coverage[index].Category] = struct{}{}
	}
	for index := range prepared.Findings {
		if len(prepared.Findings) > maxFindingsPerReport {
			return ReviewReport{}, fmt.Errorf("review report exceeds %d findings: %w", maxFindingsPerReport, ErrInvalid)
		}
		finding, err := prepareFinding(prepared.Findings[index], assignment.ID)
		if err != nil {
			return ReviewReport{}, fmt.Errorf("finding %d: %w", index, err)
		}
		prepared.Findings[index] = finding
	}
	sort.Slice(prepared.Findings, func(i, j int) bool {
		return prepared.Findings[i].ID < prepared.Findings[j].ID
	})
	hash, err := hashJSON(prepared)
	if err != nil {
		return ReviewReport{}, fmt.Errorf("hash review report: %w", err)
	}
	if report.ContentHash != "" && report.ContentHash != hash {
		return ReviewReport{}, fmt.Errorf("review report content hash mismatch: %w", ErrConflict)
	}
	prepared.ContentHash = hash
	if prepared.ID == "" {
		prepared.ID = "report_" + hash[:24]
	}
	for index := range prepared.Findings {
		prepared.Findings[index].ReportID = prepared.ID
	}
	return prepared, nil
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
		return to == AssignmentRunning || to == AssignmentCancelled
	case AssignmentRunning:
		return to == AssignmentSucceeded || to == AssignmentFailed || to == AssignmentCancelled
	default:
		return false
	}
}

func validGateDecision(decision GateDecision) bool {
	switch decision {
	case GatePass, GateRevise, GateHumanRequired, GateIncomplete, GateFailed:
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
	prepared.Category = strings.TrimSpace(prepared.Category)
	prepared.Claim = strings.TrimSpace(prepared.Claim)
	prepared.Impact = strings.TrimSpace(prepared.Impact)
	prepared.Recommendation = strings.TrimSpace(prepared.Recommendation)
	prepared.Evidence = append([]FindingEvidence(nil), finding.Evidence...)
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
		evidence.Kind = strings.TrimSpace(evidence.Kind)
		evidence.Ref = strings.TrimSpace(evidence.Ref)
		evidence.Hash = strings.TrimSpace(evidence.Hash)
		evidence.Summary = strings.TrimSpace(evidence.Summary)
		if evidence.Kind == "" || evidence.Ref == "" || evidence.Summary == "" {
			return Finding{}, fmt.Errorf("evidence %d is incomplete: %w", index, ErrInvalid)
		}
	}
	if prepared.Fingerprint == "" {
		fingerprint, err := hashJSON(struct {
			Category string           `json:"category"`
			Location *FindingLocation `json:"location,omitempty"`
			Claim    string           `json:"claim"`
		}{prepared.Category, prepared.Location, strings.ToLower(prepared.Claim)})
		if err != nil {
			return Finding{}, err
		}
		prepared.Fingerprint = fingerprint
	}
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
