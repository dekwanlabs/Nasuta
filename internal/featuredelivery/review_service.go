package featuredelivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/platform"
)

const (
	maxReviewEventSummaryBytes = 2048
	maxReviewEventDetailBytes  = 64 << 10
)

var errReviewRoundCancelled = fmt.Errorf("review round cancelled: %w", ErrConflict)

// ReviewAssignmentRequest carries one reviewer's immutable execution snapshot.
type ReviewAssignmentRequest struct {
	Round      ReviewRound
	Policy     ReviewPolicy
	Assignment ReviewAssignment
	Context    agentapi.ContextBlock
	Actor      agentapi.Actor
}

// ReviewRunner executes one isolated reviewer without access to peer reports.
type ReviewRunner interface {
	Run(context.Context, ReviewAssignmentRequest) (ReviewReport, error)
}

// ReviewUsage records model and evidence consumption for one review execution.
type ReviewUsage struct {
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	TotalTokens     int64
	ToolCalls       int64
	CostMicros      int64
}

// ReviewRunResult keeps usage available when structured report validation fails.
type ReviewRunResult struct {
	Report ReviewReport
	Usage  ReviewUsage
}

// ReviewRunnerWithUsage is an optional extension of ReviewRunner.
type ReviewRunnerWithUsage interface {
	RunWithUsage(context.Context, ReviewAssignmentRequest) (ReviewRunResult, error)
}

// ReviewAdjudicationRequest carries one isolated conflict group.
type ReviewAdjudicationRequest struct {
	Round    ReviewRound
	Policy   ReviewPolicy
	Findings []Finding
	Context  agentapi.ContextBlock
	Actor    agentapi.Actor
}

type AdjudicationOutcome struct {
	Decision  AdjudicationDecision `json:"decision"`
	Rationale string               `json:"rationale"`
}

type adjudicationFinding struct {
	ID             string            `json:"id"`
	Category       string            `json:"category"`
	Severity       Severity          `json:"severity"`
	Claim          string            `json:"claim"`
	Impact         string            `json:"impact"`
	Evidence       []FindingEvidence `json:"evidence"`
	Location       *FindingLocation  `json:"location,omitempty"`
	Recommendation string            `json:"recommendation"`
	Confidence     float64           `json:"confidence"`
}

// AdjudicationRunner executes one read-only second pass without reviewer identities.
type AdjudicationRunner interface {
	Run(context.Context, ReviewAdjudicationRequest) (AdjudicationOutcome, error)
}

// AdjudicationRunResult keeps model usage beside one conflict decision.
type AdjudicationRunResult struct {
	Outcome AdjudicationOutcome
	Usage   ReviewUsage
}

// AdjudicationRunnerWithUsage is an optional extension of AdjudicationRunner.
type AdjudicationRunnerWithUsage interface {
	RunWithUsage(context.Context, ReviewAdjudicationRequest) (AdjudicationRunResult, error)
}

// RuntimeReviewRunner adapts the generic Agent Runtime to structured reviews.
type RuntimeReviewRunner struct {
	runtime agentapi.Runtime
}

// RuntimeAdjudicationRunner adapts the generic Agent Runtime to conflict decisions.
type RuntimeAdjudicationRunner struct {
	runtime agentapi.Runtime
}

type runtimeReviewError struct {
	code      string
	message   string
	retryable bool
}

func (err runtimeReviewError) Error() string {
	return fmt.Sprintf("%s: %s", err.code, err.message)
}

func (err runtimeReviewError) Retryable() bool {
	return err.retryable
}

// NewRuntimeReviewRunner binds structured review execution to a generic Runtime.
func NewRuntimeReviewRunner(runtime agentapi.Runtime) *RuntimeReviewRunner {
	return &RuntimeReviewRunner{runtime: runtime}
}

// NewRuntimeAdjudicationRunner binds structured adjudication to a generic Runtime.
func NewRuntimeAdjudicationRunner(runtime agentapi.Runtime) *RuntimeAdjudicationRunner {
	return &RuntimeAdjudicationRunner{runtime: runtime}
}

func (runner *RuntimeReviewRunner) Run(ctx context.Context, request ReviewAssignmentRequest) (ReviewReport, error) {
	result, err := runner.RunWithUsage(ctx, request)
	return result.Report, err
}

func (runner *RuntimeReviewRunner) RunWithUsage(
	ctx context.Context,
	request ReviewAssignmentRequest,
) (ReviewRunResult, error) {
	if runner == nil || runner.runtime == nil {
		return ReviewRunResult{}, ErrUnavailable
	}
	input, err := json.Marshal(struct {
		Subject    ReviewSubject `json:"subject"`
		Categories []string      `json:"categories"`
		PolicyHash string        `json:"policy_hash"`
	}{
		Subject: request.Round.Subject, Categories: request.Assignment.Categories,
		PolicyHash: request.Policy.ContentHash,
	})
	if err != nil {
		return ReviewRunResult{}, fmt.Errorf("marshal reviewer input: %w", err)
	}
	runtimeResult, err := runner.runtime.Run(ctx, agentapi.RunRequest{
		RunID:          request.Assignment.AgentRunID,
		Agent:          request.Assignment.Agent,
		DefinitionHash: request.Assignment.DefinitionHash,
		Input:          input,
		Context:        []agentapi.ContextBlock{request.Context},
		Permissions:    agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		Policy:         agentapi.RunPolicy{RedactSensitive: true},
		Actor:          request.Actor,
		Correlation: agentapi.Correlation{
			WorkflowRunID: request.Round.WorkflowRunID,
			NodeID:        request.Assignment.ReviewerID,
		},
	})
	usage := reviewUsageFromRuntimeResult(runtimeResult)
	if err != nil {
		return ReviewRunResult{Usage: usage}, err
	}
	if runtimeResult.Status != agentapi.RunSucceeded {
		if runtimeResult.Error != nil {
			return ReviewRunResult{Usage: usage}, fmt.Errorf(
				"reviewer %q failed: %w",
				request.Assignment.ReviewerID,
				runtimeReviewError{
					code: runtimeResult.Error.Code, message: runtimeResult.Error.Message,
					retryable: runtimeResult.Error.Retryable,
				},
			)
		}
		return ReviewRunResult{Usage: usage}, fmt.Errorf(
			"reviewer %q ended with status %q",
			request.Assignment.ReviewerID,
			runtimeResult.Status,
		)
	}
	raw := runtimeResult.Output
	if len(raw) == 0 {
		raw = json.RawMessage(runtimeResult.Text)
	}
	var report ReviewReport
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return ReviewRunResult{Usage: usage}, fmt.Errorf(
			"decode reviewer %q report: %w",
			request.Assignment.ReviewerID,
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ReviewRunResult{Usage: usage}, fmt.Errorf(
			"decode reviewer %q report: trailing JSON content",
			request.Assignment.ReviewerID,
		)
	}
	report.RoundID = request.Round.ID
	report.AssignmentID = request.Assignment.ID
	report.ReviewerID = request.Assignment.ReviewerID
	report.SubjectHash = request.Round.Subject.ContentHash
	report.ID = ""
	report.ContentHash = ""
	report.CompletedAt = time.Time{}
	return ReviewRunResult{Report: report, Usage: usage}, nil
}

func (runner *RuntimeAdjudicationRunner) Run(
	ctx context.Context,
	request ReviewAdjudicationRequest,
) (AdjudicationOutcome, error) {
	result, err := runner.RunWithUsage(ctx, request)
	return result.Outcome, err
}

func (runner *RuntimeAdjudicationRunner) RunWithUsage(
	ctx context.Context,
	request ReviewAdjudicationRequest,
) (AdjudicationRunResult, error) {
	if runner == nil || runner.runtime == nil || request.Policy.Adjudicator == nil {
		return AdjudicationRunResult{}, ErrUnavailable
	}
	if len(request.Findings) < 2 {
		return AdjudicationRunResult{}, fmt.Errorf("adjudicator requires a conflict group: %w", ErrInvalid)
	}
	fingerprint := request.Findings[0].Fingerprint
	if fingerprint == "" {
		return AdjudicationRunResult{}, fmt.Errorf("adjudicator conflict fingerprint is required: %w", ErrInvalid)
	}
	findings := make([]adjudicationFinding, 0, len(request.Findings))
	for _, finding := range request.Findings {
		if finding.Fingerprint != fingerprint {
			return AdjudicationRunResult{}, fmt.Errorf("adjudicator findings span conflict groups: %w", ErrInvalid)
		}
		findings = append(findings, adjudicationFinding{
			ID: finding.ID, Category: finding.Category, Severity: finding.Severity,
			Claim: finding.Claim, Impact: finding.Impact, Evidence: finding.Evidence,
			Location: finding.Location, Recommendation: finding.Recommendation,
			Confidence: finding.Confidence,
		})
	}
	input, err := json.Marshal(struct {
		Subject     ReviewSubject         `json:"subject"`
		PolicyHash  string                `json:"policy_hash"`
		Fingerprint string                `json:"fingerprint"`
		Findings    []adjudicationFinding `json:"findings"`
	}{
		Subject: request.Round.Subject, PolicyHash: request.Policy.ContentHash,
		Fingerprint: fingerprint, Findings: findings,
	})
	if err != nil {
		return AdjudicationRunResult{}, fmt.Errorf("marshal adjudicator input: %w", err)
	}
	runHash, err := hashJSON(struct {
		RoundID     string `json:"round_id"`
		Fingerprint string `json:"fingerprint"`
	}{request.Round.ID, fingerprint})
	if err != nil {
		return AdjudicationRunResult{}, fmt.Errorf("hash adjudicator run id: %w", err)
	}
	spec := request.Policy.Adjudicator
	runtimeResult, err := runner.runtime.Run(ctx, agentapi.RunRequest{
		RunID:          "adjudication_run_" + runHash[:24],
		Agent:          spec.Agent,
		DefinitionHash: spec.DefinitionHash,
		Input:          input,
		Context:        []agentapi.ContextBlock{request.Context},
		Permissions:    agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		Policy:         agentapi.RunPolicy{RedactSensitive: true},
		Actor:          request.Actor,
		Correlation: agentapi.Correlation{
			WorkflowRunID: request.Round.WorkflowRunID,
			NodeID:        "adjudicator",
		},
	})
	usage := reviewUsageFromRuntimeResult(runtimeResult)
	if err != nil {
		return AdjudicationRunResult{Usage: usage}, err
	}
	if runtimeResult.Status != agentapi.RunSucceeded {
		if runtimeResult.Error != nil {
			return AdjudicationRunResult{Usage: usage}, fmt.Errorf(
				"adjudicator failed: %w",
				runtimeReviewError{
					code: runtimeResult.Error.Code, message: runtimeResult.Error.Message,
					retryable: runtimeResult.Error.Retryable,
				},
			)
		}
		return AdjudicationRunResult{Usage: usage}, fmt.Errorf(
			"adjudicator ended with status %q",
			runtimeResult.Status,
		)
	}
	raw := runtimeResult.Output
	if len(raw) == 0 {
		raw = json.RawMessage(runtimeResult.Text)
	}
	var outcome AdjudicationOutcome
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&outcome); err != nil {
		return AdjudicationRunResult{Usage: usage}, fmt.Errorf(
			"decode adjudicator outcome: %w",
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return AdjudicationRunResult{Usage: usage}, fmt.Errorf(
			"decode adjudicator outcome: trailing JSON content",
		)
	}
	outcome.Rationale = strings.TrimSpace(outcome.Rationale)
	if !validAdjudicationDecision(outcome.Decision) || outcome.Rationale == "" {
		return AdjudicationRunResult{Usage: usage}, fmt.Errorf(
			"adjudicator outcome is invalid: %w",
			ErrInvalid,
		)
	}
	return AdjudicationRunResult{Outcome: outcome, Usage: usage}, nil
}

func reviewUsageFromRuntimeResult(result agentapi.RunResult) ReviewUsage {
	return ReviewUsage{
		InputTokens:     result.Usage.InputTokens,
		OutputTokens:    result.Usage.OutputTokens,
		ReasoningTokens: result.Usage.ReasoningTokens,
		TotalTokens:     result.Usage.TotalTokens,
		ToolCalls:       int64(result.Evidence.ToolCallCount),
		CostMicros:      result.Usage.CostMicros,
	}
}

func addReviewUsage(left, right ReviewUsage) ReviewUsage {
	return ReviewUsage{
		InputTokens:     left.InputTokens + right.InputTokens,
		OutputTokens:    left.OutputTokens + right.OutputTokens,
		ReasoningTokens: left.ReasoningTokens + right.ReasoningTokens,
		TotalTokens:     left.TotalTokens + right.TotalTokens,
		ToolCalls:       left.ToolCalls + right.ToolCalls,
		CostMicros:      left.CostMicros + right.CostMicros,
	}
}

// SetReviewConfiguration swaps the runtime and defaults used only by future rounds.
func (service *Service) SetReviewConfiguration(
	runner ReviewRunner,
	defaults map[SubjectKind]ReviewPolicyRef,
) {
	copied := make(map[SubjectKind]ReviewPolicyRef, len(defaults))
	for kind, ref := range defaults {
		copied[kind] = ref
	}
	service.reviewerMu.Lock()
	defer service.reviewerMu.Unlock()
	service.reviewer = runner
	service.defaultPolicies = copied
}

// SetReviewRunner enables test and extension runners without changing policy selection.
func (service *Service) SetReviewRunner(runner ReviewRunner) {
	service.reviewerMu.Lock()
	defer service.reviewerMu.Unlock()
	service.reviewer = runner
}

// SetAdjudicationRunner swaps the optional second-pass runner for future executions.
func (service *Service) SetAdjudicationRunner(runner AdjudicationRunner) {
	service.reviewerMu.Lock()
	defer service.reviewerMu.Unlock()
	service.adjudicator = runner
}

func (service *Service) reviewRunner() ReviewRunner {
	service.reviewerMu.RLock()
	defer service.reviewerMu.RUnlock()
	return service.reviewer
}

func (service *Service) adjudicationRunner() AdjudicationRunner {
	service.reviewerMu.RLock()
	defer service.reviewerMu.RUnlock()
	return service.adjudicator
}

// InstallDefaultReviewPolicies atomically publishes the standard panel policy set.
func (service *Service) InstallDefaultReviewPolicies(
	ctx context.Context,
	definitions []agentapi.Definition,
) (map[SubjectKind]ReviewPolicyRef, error) {
	_, refs, err := service.InstallDefaultReviewPolicySet(ctx, definitions)
	return refs, err
}

// InstallDefaultReviewPolicySet returns the immutable policies used by runtime composition.
func (service *Service) InstallDefaultReviewPolicySet(
	ctx context.Context,
	definitions []agentapi.Definition,
) ([]ReviewPolicy, map[SubjectKind]ReviewPolicyRef, error) {
	if service == nil || service.store == nil {
		return nil, nil, ErrUnavailable
	}
	policies, err := DefaultReviewPolicies(definitions, service.now())
	if err != nil {
		return nil, nil, err
	}
	if err := service.store.SaveReviewPolicies(ctx, policies); err != nil {
		return nil, nil, err
	}
	if control, ok := service.store.(ReviewPolicyControlStore); ok {
		for _, policy := range policies {
			if err := control.EnsureReviewPolicyDefault(
				ctx, policy.ID, policy.Version, 0,
			); err != nil {
				return nil, nil, fmt.Errorf(
					"ensure default review policy %q@%d: %w",
					policy.ID, policy.Version, err,
				)
			}
		}
	}
	refs := make(map[SubjectKind]ReviewPolicyRef, len(policies))
	for _, policy := range policies {
		refs[policy.SubjectKind] = ReviewPolicyRef{ID: policy.ID, Version: policy.Version}
	}
	return policies, refs, nil
}

// PublishReviewPolicy stores one immutable administrator-controlled policy version.
func (service *Service) PublishReviewPolicy(ctx context.Context, policy ReviewPolicy, admin bool) (*ReviewPolicy, error) {
	return service.PublishReviewPolicyAs(ctx, policy, 0, admin)
}

// PublishReviewPolicyAs records the administrator responsible for publication.
func (service *Service) PublishReviewPolicyAs(
	ctx context.Context,
	policy ReviewPolicy,
	actorUserID int64,
	admin bool,
) (*ReviewPolicy, error) {
	if !admin {
		return nil, ErrForbidden
	}
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	if policy.CreatedAt.IsZero() {
		policy.CreatedAt = service.now()
	}
	prepared, err := PrepareReviewPolicy(policy)
	if err != nil {
		return nil, err
	}
	if control, ok := service.store.(ReviewPolicyControlStore); ok {
		if err := control.PublishReviewPolicies(ctx, []ReviewPolicy{prepared}, actorUserID); err != nil {
			return nil, err
		}
	} else if err := service.store.SaveReviewPolicies(ctx, []ReviewPolicy{prepared}); err != nil {
		return nil, err
	}
	return &prepared, nil
}

func (service *Service) ListReviewPolicyRecords(
	ctx context.Context,
	cursor ReviewPolicyCursor,
	limit int,
	admin bool,
) ([]ReviewPolicyRecord, error) {
	if !admin {
		return nil, ErrForbidden
	}
	control, ok := service.reviewPolicyControl()
	if !ok {
		return nil, ErrUnavailable
	}
	return control.ListReviewPolicyRecords(ctx, cursor, limit)
}

func (service *Service) SetReviewPolicyDefault(
	ctx context.Context,
	id string,
	version int64,
	actorUserID int64,
	admin bool,
) error {
	if !admin {
		return ErrForbidden
	}
	if id == "" || version <= 0 {
		return ErrInvalid
	}
	control, ok := service.reviewPolicyControl()
	if !ok {
		return ErrUnavailable
	}
	return control.SetReviewPolicyDefault(ctx, id, version, actorUserID)
}

func (service *Service) SetReviewPolicyActive(
	ctx context.Context,
	id string,
	version int64,
	active bool,
	actorUserID int64,
	admin bool,
) error {
	if !admin {
		return ErrForbidden
	}
	if id == "" || version <= 0 {
		return ErrInvalid
	}
	control, ok := service.reviewPolicyControl()
	if !ok {
		return ErrUnavailable
	}
	return control.SetReviewPolicyActive(ctx, id, version, active, actorUserID)
}

func (service *Service) ListReviewPolicyAudit(
	ctx context.Context,
	id string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]ReviewPolicyAuditEvent, error) {
	if !admin {
		return nil, ErrForbidden
	}
	if id == "" || afterSeq < 0 {
		return nil, ErrInvalid
	}
	control, ok := service.reviewPolicyControl()
	if !ok {
		return nil, ErrUnavailable
	}
	return control.ListReviewPolicyAudit(ctx, id, afterSeq, limit)
}

func (service *Service) GetReviewPolicyRollout(
	ctx context.Context,
	kind SubjectKind,
) (ReviewPolicyRolloutRule, bool, error) {
	if !validSubjectKind(kind) {
		return ReviewPolicyRolloutRule{}, false, ErrInvalid
	}
	control, ok := service.reviewPolicyControl()
	if !ok {
		return ReviewPolicyRolloutRule{}, false, ErrUnavailable
	}
	rule, found, err := control.GetReviewPolicyRollout(ctx, kind)
	if err != nil || !found {
		return rule, found, err
	}
	prepared, err := prepareReviewPolicyRolloutRule(rule)
	if err != nil {
		return ReviewPolicyRolloutRule{}, false, fmt.Errorf(
			"validate stored review policy rollout for %q: %w", kind, err,
		)
	}
	return prepared, true, nil
}

func (service *Service) SetReviewPolicyRollout(
	ctx context.Context,
	kind SubjectKind,
	candidateID string,
	candidateVersion int64,
	percentageBPS int,
	salt string,
	active bool,
	actorUserID int64,
	admin bool,
) (ReviewPolicyRolloutRule, error) {
	if !admin {
		return ReviewPolicyRolloutRule{}, ErrForbidden
	}
	if !validSubjectKind(kind) || candidateID == "" ||
		candidateVersion <= 0 || percentageBPS < 0 ||
		percentageBPS > reviewPolicyRolloutBucketCount || salt == "" {
		return ReviewPolicyRolloutRule{}, ErrInvalid
	}
	control, ok := service.reviewPolicyControl()
	if !ok {
		return ReviewPolicyRolloutRule{}, ErrUnavailable
	}
	candidate, err := control.GetReviewPolicyRecord(ctx, candidateID, candidateVersion)
	if err != nil {
		return ReviewPolicyRolloutRule{}, err
	}
	if candidate.SubjectKind != kind {
		return ReviewPolicyRolloutRule{}, fmt.Errorf(
			"review policy %q@%d does not match subject kind %q: %w",
			candidateID, candidateVersion, kind, ErrConflict,
		)
	}
	if !candidate.Active {
		return ReviewPolicyRolloutRule{}, fmt.Errorf(
			"review policy %q@%d is disabled: %w",
			candidateID, candidateVersion, ErrConflict,
		)
	}
	current, found, err := control.GetReviewPolicyRollout(ctx, kind)
	if err != nil {
		return ReviewPolicyRolloutRule{}, err
	}
	ruleVersion := int64(1)
	if found {
		ruleVersion = current.RuleVersion + 1
	}
	rule, err := prepareReviewPolicyRolloutRule(ReviewPolicyRolloutRule{
		SubjectKind:            kind,
		RuleVersion:            ruleVersion,
		CandidatePolicyID:      candidateID,
		CandidatePolicyVersion: candidateVersion,
		PercentageBPS:          percentageBPS,
		Salt:                   salt,
		Active:                 active,
		CreatedBy:              actorUserID,
		CreatedAt:              service.now(),
	})
	if err != nil {
		return ReviewPolicyRolloutRule{}, err
	}
	if err := control.SetReviewPolicyRollout(ctx, rule, actorUserID); err != nil {
		return ReviewPolicyRolloutRule{}, err
	}
	return rule, nil
}

func (service *Service) ListReviewPolicyRolloutAudit(
	ctx context.Context,
	kind SubjectKind,
	afterSeq int64,
	limit int,
	admin bool,
) ([]ReviewPolicyRolloutAuditEvent, error) {
	if !admin {
		return nil, ErrForbidden
	}
	if !validSubjectKind(kind) || afterSeq < 0 {
		return nil, ErrInvalid
	}
	control, ok := service.reviewPolicyControl()
	if !ok {
		return nil, ErrUnavailable
	}
	return control.ListReviewPolicyRolloutAudit(ctx, kind, afterSeq, limit)
}

func (service *Service) reviewPolicyControl() (ReviewPolicyControlStore, bool) {
	if service == nil || service.store == nil {
		return nil, false
	}
	control, ok := service.store.(ReviewPolicyControlStore)
	return control, ok
}

// GetReviewPolicy exposes immutable policy versions only through the admin control plane.
func (service *Service) GetReviewPolicy(
	ctx context.Context,
	id string,
	version int64,
	admin bool,
) (*ReviewPolicy, error) {
	if !admin {
		return nil, ErrForbidden
	}
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	if id == "" || version <= 0 {
		return nil, ErrInvalid
	}
	return service.store.GetReviewPolicy(ctx, id, version)
}

// CreateSubjectReviewRound creates a round from a server-resolved subject snapshot.
func (service *Service) CreateSubjectReviewRound(
	ctx context.Context,
	kind SubjectKind,
	subjectID string,
	policyRef ReviewPolicyRef,
	userID int64,
	admin bool,
) (*ReviewRound, []ReviewAssignment, error) {
	return service.CreateSubjectReviewRoundWithReuses(
		ctx,
		kind,
		subjectID,
		policyRef,
		nil,
		userID,
		admin,
	)
}

// CreateSubjectReviewRoundWithReuses materializes explicitly selected source Reports.
func (service *Service) CreateSubjectReviewRoundWithReuses(
	ctx context.Context,
	kind SubjectKind,
	subjectID string,
	policyRef ReviewPolicyRef,
	reuseRequests []ReviewReportReuseRequest,
	userID int64,
	admin bool,
) (*ReviewRound, []ReviewAssignment, error) {
	if service == nil || service.store == nil {
		return nil, nil, ErrUnavailable
	}
	if subjectID == "" {
		return nil, nil, ErrInvalid
	}
	switch kind {
	case SubjectRequirement, SubjectRequirementAnalysis, SubjectTechnicalProposal,
		SubjectSystemDesign, SubjectImplementationPlan:
		artifact, err := service.store.GetArtifact(ctx, subjectID)
		if err != nil {
			return nil, nil, err
		}
		if _, err := service.authorizedFeature(ctx, artifact.RequestID, userID, admin); err != nil {
			return nil, nil, err
		}
		subject, err := BuildArtifactReviewSubject(*artifact)
		if err != nil {
			return nil, nil, err
		}
		if subject.Kind != kind {
			return nil, nil, ErrNotFound
		}
		facts, err := BuildArtifactReviewRiskFacts(*artifact)
		if err != nil {
			return nil, nil, err
		}
		return service.createReviewRoundWithPolicyRefAndReuses(
			ctx,
			subject,
			facts,
			policyRef,
			reuseRequests,
			userID,
		)
	case SubjectChangeSet, SubjectValidationBundle, SubjectDeliveryBundle:
		run, err := service.GetImplementation(ctx, subjectID, userID, admin)
		if err != nil {
			return nil, nil, err
		}
		subject, err := service.buildImplementationReviewSubject(ctx, kind, *run)
		if err != nil {
			return nil, nil, err
		}
		facts, err := BuildImplementationReviewRiskFacts(kind, *run)
		if err != nil {
			return nil, nil, err
		}
		return service.createReviewRoundWithPolicyRefAndReuses(
			ctx,
			subject,
			facts,
			policyRef,
			reuseRequests,
			userID,
		)
	default:
		return nil, nil, ErrInvalid
	}
}

// CreateArtifactReviewRound creates a round for one authorized artifact version.
func (service *Service) CreateArtifactReviewRound(
	ctx context.Context,
	requestID, artifactID string,
	policyRef ReviewPolicyRef,
	userID int64,
	admin bool,
) (*ReviewRound, []ReviewAssignment, error) {
	artifact, err := service.GetArtifact(ctx, requestID, artifactID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	subject, err := BuildArtifactReviewSubject(*artifact)
	if err != nil {
		return nil, nil, err
	}
	facts, err := BuildArtifactReviewRiskFacts(*artifact)
	if err != nil {
		return nil, nil, err
	}
	return service.createReviewRoundWithPolicyRef(ctx, subject, facts, policyRef, userID)
}

// CreateChangeSetReviewRound creates a round for one completed implementation.
func (service *Service) CreateChangeSetReviewRound(
	ctx context.Context,
	runID string,
	policyRef ReviewPolicyRef,
	userID int64,
	admin bool,
) (*ReviewRound, []ReviewAssignment, error) {
	run, err := service.GetImplementation(ctx, runID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	subject, err := service.buildImplementationReviewSubject(ctx, SubjectChangeSet, *run)
	if err != nil {
		return nil, nil, err
	}
	facts, err := BuildImplementationReviewRiskFacts(SubjectChangeSet, *run)
	if err != nil {
		return nil, nil, err
	}
	return service.createReviewRoundWithPolicyRef(ctx, subject, facts, policyRef, userID)
}

func (service *Service) buildImplementationReviewSubject(
	ctx context.Context,
	kind SubjectKind,
	run ImplementationRun,
) (ReviewSubject, error) {
	if run.ChangeSet == nil {
		return ReviewSubject{}, fmt.Errorf("implementation %q has no change set: %w", run.ID, ErrConflict)
	}
	switch kind {
	case SubjectChangeSet:
		if run.Status != RunSucceeded && run.Status != RunFailed {
			return ReviewSubject{}, ErrConflict
		}
		return BuildChangeSetReviewSubject(run)
	case SubjectValidationBundle:
		if run.Status != RunSucceeded && run.Status != RunFailed {
			return ReviewSubject{}, ErrConflict
		}
		return BuildValidationBundleReviewSubject(run)
	case SubjectDeliveryBundle:
		if run.Status != RunSucceeded {
			return ReviewSubject{}, ErrConflict
		}
		design, plan, err := service.loadDeliveryArtifacts(ctx, run)
		if err != nil {
			return ReviewSubject{}, err
		}
		return BuildDeliveryBundleReviewSubject(run, design, plan)
	default:
		return ReviewSubject{}, ErrInvalid
	}
}

func (service *Service) loadDeliveryArtifacts(
	ctx context.Context,
	run ImplementationRun,
) (Artifact, Artifact, error) {
	if run.DesignArtifactID == "" || run.PlanArtifactID == "" {
		return Artifact{}, Artifact{}, fmt.Errorf("implementation %q has no delivery artifacts: %w", run.ID, ErrConflict)
	}
	design, err := service.store.GetArtifact(ctx, run.DesignArtifactID)
	if err != nil {
		return Artifact{}, Artifact{}, err
	}
	plan, err := service.store.GetArtifact(ctx, run.PlanArtifactID)
	if err != nil {
		return Artifact{}, Artifact{}, err
	}
	return *design, *plan, nil
}

func (service *Service) createReviewRoundWithPolicyRef(
	ctx context.Context,
	subject ReviewSubject,
	riskFacts []ReviewRiskFact,
	policyRef ReviewPolicyRef,
	userID int64,
) (*ReviewRound, []ReviewAssignment, error) {
	return service.createReviewRoundWithPolicyRefAndReuses(
		ctx,
		subject,
		riskFacts,
		policyRef,
		nil,
		userID,
	)
}

func (service *Service) createReviewRoundWithPolicyRefAndReuses(
	ctx context.Context,
	subject ReviewSubject,
	riskFacts []ReviewRiskFact,
	policyRef ReviewPolicyRef,
	reuseRequests []ReviewReportReuseRequest,
	userID int64,
) (*ReviewRound, []ReviewAssignment, error) {
	if service == nil || service.store == nil {
		return nil, nil, ErrUnavailable
	}
	resolvedRef, selection, err := service.resolveReviewPolicyRef(
		ctx,
		subject,
		policyRef,
		userID,
	)
	if err != nil {
		return nil, nil, err
	}
	policy, err := service.store.GetReviewPolicy(ctx, resolvedRef.ID, resolvedRef.Version)
	if err != nil {
		return nil, nil, err
	}
	if policy.SubjectKind != subject.Kind {
		return nil, nil, fmt.Errorf("review policy does not match subject kind: %w", ErrConflict)
	}
	preparedFacts, riskHash, reviewers, panelHash, err := PrepareReviewPanel(
		*policy,
		riskFacts,
	)
	if err != nil {
		return nil, nil, err
	}
	now := service.now()
	roundID, err := NewID("round")
	if err != nil {
		return nil, nil, err
	}
	round := ReviewRound{
		ID: roundID, Subject: subject,
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
		PolicySelection: selection, RiskFacts: preparedFacts, RiskHash: riskHash,
		RuleVersion: policy.RiskRuleVersion, Reviewers: reviewers, PanelHash: panelHash,
		Status: RoundCreated, CreatedBy: userID, CreatedAt: now,
	}
	assignments := make([]ReviewAssignment, 0, len(reviewers))
	for _, reviewer := range reviewers {
		assignmentID, err := NewID("assignment")
		if err != nil {
			return nil, nil, err
		}
		assignments = append(assignments, ReviewAssignment{
			ID: assignmentID, RoundID: round.ID, ReviewerID: reviewer.ID,
			Agent: reviewer.Agent, DefinitionHash: reviewer.DefinitionHash,
			Categories: append([]string(nil), reviewer.Categories...), Required: reviewer.Required,
			Status: AssignmentQueued, Attempt: 1, CreatedAt: now,
		})
	}
	if len(reuseRequests) == 0 {
		if err := service.store.CreateReviewRound(ctx, round, assignments); err != nil {
			return nil, nil, err
		}
		return &round, assignments, nil
	}
	reuseStore, ok := service.store.(ReviewReportReuseStore)
	if !ok {
		return nil, nil, ErrUnavailable
	}
	reports, reuses, err := service.prepareReviewReportReuses(
		ctx,
		reuseStore,
		round,
		assignments,
		reuseRequests,
		userID,
		now,
	)
	if err != nil {
		return nil, nil, err
	}
	if err := reuseStore.CreateReviewRoundWithReuses(
		ctx,
		round,
		assignments,
		reports,
		reuses,
	); err != nil {
		return nil, nil, err
	}
	return &round, assignments, nil
}

func (service *Service) prepareReviewReportReuses(
	ctx context.Context,
	store ReviewReportReuseStore,
	round ReviewRound,
	assignments []ReviewAssignment,
	requests []ReviewReportReuseRequest,
	userID int64,
	createdAt time.Time,
) ([]ReviewReport, []ReviewReportReuse, error) {
	if len(requests) > len(assignments) {
		return nil, nil, fmt.Errorf("review report reuse count is invalid: %w", ErrInvalid)
	}
	assignmentIndexes := make(map[string]int, len(assignments))
	for index, assignment := range assignments {
		assignmentIndexes[assignment.ReviewerID] = index
	}
	requestByReviewer := make(map[string]ReviewReportReuseRequest, len(requests))
	sourceIDs := make([]string, 0, len(requests))
	seenSources := make(map[string]struct{}, len(requests))
	for _, request := range requests {
		if request.ReviewerID == "" || request.SourceReportID == "" ||
			len(request.ReportHash) != sha256.Size*2 || !isHex(request.ReportHash) ||
			strings.TrimSpace(request.Reason) == "" {
			return nil, nil, fmt.Errorf("review report reuse request is incomplete: %w", ErrInvalid)
		}
		if _, ok := assignmentIndexes[request.ReviewerID]; !ok {
			return nil, nil, fmt.Errorf(
				"reviewer %q is not in the selected panel: %w",
				request.ReviewerID,
				ErrInvalid,
			)
		}
		if _, duplicate := requestByReviewer[request.ReviewerID]; duplicate {
			return nil, nil, fmt.Errorf(
				"duplicate reuse request for reviewer %q: %w",
				request.ReviewerID,
				ErrInvalid,
			)
		}
		if _, duplicate := seenSources[request.SourceReportID]; duplicate {
			return nil, nil, fmt.Errorf(
				"duplicate source report %q: %w",
				request.SourceReportID,
				ErrInvalid,
			)
		}
		requestByReviewer[request.ReviewerID] = request
		seenSources[request.SourceReportID] = struct{}{}
		sourceIDs = append(sourceIDs, request.SourceReportID)
	}
	sources, err := store.GetReviewReportReuseSources(ctx, sourceIDs)
	if err != nil {
		return nil, nil, err
	}
	sourceByID := make(map[string]ReviewReportReuseSource, len(sources))
	for _, source := range sources {
		sourceByID[source.Report.ID] = source
	}
	if len(sourceByID) != len(sourceIDs) {
		return nil, nil, ErrNotFound
	}
	reports := make([]ReviewReport, 0, len(requests))
	reuses := make([]ReviewReportReuse, 0, len(requests))
	for reviewerID, request := range requestByReviewer {
		assignmentIndex := assignmentIndexes[reviewerID]
		assignment := assignments[assignmentIndex]
		source, ok := sourceByID[request.SourceReportID]
		if !ok {
			return nil, nil, ErrNotFound
		}
		if !successfulReviewAssignment(source.Assignment.Status) ||
			source.Report.ReviewerID != reviewerID ||
			source.Assignment.ReviewerID != reviewerID ||
			source.Report.SubjectHash != round.Subject.ContentHash ||
			source.PolicyID != round.PolicyID ||
			source.PolicyVersion != round.PolicyVersion ||
			source.PolicyHash != round.PolicyHash ||
			source.Assignment.Agent != assignment.Agent ||
			source.Assignment.DefinitionHash != assignment.DefinitionHash ||
			source.Report.ReportHash != request.ReportHash {
			return nil, nil, fmt.Errorf(
				"source report %q does not match the target snapshot: %w",
				request.SourceReportID,
				ErrConflict,
			)
		}
		report, err := PrepareReusedReviewReport(
			source.Report,
			assignment,
			round.Subject.ContentHash,
			ReviewReportReuseRef{
				SourceReportID:     request.SourceReportID,
				SourceRoundID:      source.Report.RoundID,
				SourceAssignmentID: source.Report.AssignmentID,
				Reason:             request.Reason,
			},
			createdAt,
		)
		if err != nil {
			return nil, nil, err
		}
		reuseID, err := NewID("reportreuse")
		if err != nil {
			return nil, nil, err
		}
		completedAt := createdAt
		assignment.Status = AssignmentReused
		assignment.CompletedAt = &completedAt
		assignments[assignmentIndex] = assignment
		reports = append(reports, report)
		reuses = append(reuses, ReviewReportReuse{
			ID: reuseID, RoundID: round.ID, AssignmentID: assignment.ID,
			ReportID: report.ID, ReviewerID: reviewerID,
			SourceRoundID:      source.Report.RoundID,
			SourceAssignmentID: source.Report.AssignmentID,
			SourceReportID:     request.SourceReportID,
			SubjectHash:        round.Subject.ContentHash, PolicyHash: round.PolicyHash,
			DefinitionHash: assignment.DefinitionHash, ReportHash: report.ReportHash,
			Reason: report.Reuse.Reason, ActorID: userID, CreatedAt: createdAt,
		})
	}
	sort.Slice(reports, func(i, j int) bool {
		return reports[i].ReviewerID < reports[j].ReviewerID
	})
	sort.Slice(reuses, func(i, j int) bool {
		return reuses[i].ReviewerID < reuses[j].ReviewerID
	})
	return reports, reuses, nil
}

func (service *Service) resolveReviewPolicyRef(
	ctx context.Context,
	subject ReviewSubject,
	ref ReviewPolicyRef,
	userID int64,
) (ReviewPolicyRef, ReviewPolicySelection, error) {
	if ref.ID != "" || ref.Version != 0 {
		if ref.ID == "" || ref.Version <= 0 {
			return ReviewPolicyRef{}, ReviewPolicySelection{}, fmt.Errorf(
				"review policy id and positive version are required: %w", ErrInvalid,
			)
		}
		return ref, ReviewPolicySelection{Reason: "explicit_version"}, nil
	}
	if control, ok := service.reviewPolicyControl(); ok {
		defaultRef, err := control.GetDefaultReviewPolicy(ctx, subject.Kind)
		if err == nil {
			rule, found, rolloutErr := control.GetReviewPolicyRollout(ctx, subject.Kind)
			if rolloutErr != nil {
				return ReviewPolicyRef{}, ReviewPolicySelection{}, rolloutErr
			}
			if !found || !rule.Active {
				return defaultRef, ReviewPolicySelection{Reason: "default"}, nil
			}
			rule, rolloutErr = prepareReviewPolicyRolloutRule(rule)
			if rolloutErr != nil {
				return ReviewPolicyRef{}, ReviewPolicySelection{}, fmt.Errorf(
					"validate stored review policy rollout for %q: %w",
					subject.Kind, rolloutErr,
				)
			}
			candidate, candidateErr := control.GetReviewPolicyRecord(
				ctx,
				rule.CandidatePolicyID,
				rule.CandidatePolicyVersion,
			)
			if candidateErr != nil {
				return ReviewPolicyRef{}, ReviewPolicySelection{}, candidateErr
			}
			if candidate.SubjectKind != subject.Kind || !candidate.Active {
				return ReviewPolicyRef{}, ReviewPolicySelection{}, fmt.Errorf(
					"review policy rollout candidate %q@%d is unavailable for %q: %w",
					rule.CandidatePolicyID,
					rule.CandidatePolicyVersion,
					subject.Kind,
					ErrConflict,
				)
			}
			selection, useCandidate, selectionErr := selectReviewPolicyRollout(
				rule,
				StableReviewPolicySelectionKey(userID, subject.Kind, subject.ID),
			)
			if selectionErr != nil {
				return ReviewPolicyRef{}, ReviewPolicySelection{}, selectionErr
			}
			if useCandidate {
				return ReviewPolicyRef{
					ID: rule.CandidatePolicyID, Version: rule.CandidatePolicyVersion,
				}, selection, nil
			}
			return defaultRef, selection, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return ReviewPolicyRef{}, ReviewPolicySelection{}, err
		}
		return ReviewPolicyRef{}, ReviewPolicySelection{}, fmt.Errorf(
			"default review policy for %q is unavailable: %w",
			subject.Kind,
			ErrUnavailable,
		)
	}
	service.reviewerMu.RLock()
	defaultRef, ok := service.defaultPolicies[subject.Kind]
	service.reviewerMu.RUnlock()
	if !ok {
		return ReviewPolicyRef{}, ReviewPolicySelection{}, fmt.Errorf(
			"default review policy for %q is unavailable: %w",
			subject.Kind,
			ErrUnavailable,
		)
	}
	return defaultRef, ReviewPolicySelection{Reason: "default"}, nil
}

// ListReviewRounds returns only persisted round metadata for operations views.
func (service *Service) ListReviewRounds(
	ctx context.Context,
	filter ReviewRoundFilter,
	cursor ReviewRoundCursor,
	limit int,
	userID int64,
	admin bool,
) ([]ReviewRoundSummary, bool, error) {
	if service == nil || service.store == nil {
		return nil, false, ErrUnavailable
	}
	if filter.SubjectKind != "" && !validSubjectKind(filter.SubjectKind) {
		return nil, false, ErrInvalid
	}
	if filter.Status != "" && !validReviewRoundStatus(filter.Status) {
		return nil, false, ErrInvalid
	}
	if cursor.CreatedAt.IsZero() != (cursor.ID == "") {
		return nil, false, ErrInvalid
	}
	query, ok := service.store.(ReviewRoundQueryStore)
	if !ok {
		return nil, false, ErrUnavailable
	}
	return query.ListReviewRoundSummaries(ctx, filter, cursor, limit, userID, admin)
}

// GetReviewRound returns a round only after checking subject ownership.
func (service *Service) GetReviewRound(
	ctx context.Context,
	roundID string,
	userID int64,
	admin bool,
) (*ReviewRound, error) {
	return service.authorizeReviewRound(ctx, roundID, userID, admin)
}

// ListReviewAssignments returns one bounded assignment page for an authorized round.
func (service *Service) ListReviewAssignments(
	ctx context.Context,
	roundID string,
	cursor ReviewAssignmentCursor,
	limit int,
	userID int64,
	admin bool,
) ([]ReviewAssignment, error) {
	if _, err := service.authorizeReviewRound(ctx, roundID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListReviewAssignments(ctx, roundID, cursor, limit)
}

// GetReviewReport returns the immutable report for one authorized assignment.
func (service *Service) GetReviewReport(
	ctx context.Context,
	roundID string,
	assignmentID string,
	userID int64,
	admin bool,
) (*ReviewReport, error) {
	if _, err := service.authorizeReviewRound(ctx, roundID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.GetReviewReportByAssignment(ctx, roundID, assignmentID)
}

// ListReviewFindings returns bounded finding summaries without loading evidence.
func (service *Service) ListReviewFindings(
	ctx context.Context,
	roundID string,
	severity Severity,
	cursor FindingCursor,
	limit int,
	userID int64,
	admin bool,
) ([]FindingSummary, error) {
	if severity != "" && !validSeverity(severity) {
		return nil, ErrInvalid
	}
	if _, err := service.authorizeReviewRound(ctx, roundID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListReviewFindings(ctx, roundID, severity, cursor, limit)
}

// ListReviewAdjudications returns immutable conflict decisions for an authorized round.
func (service *Service) ListReviewAdjudications(
	ctx context.Context,
	roundID string,
	cursor ReviewAdjudicationCursor,
	limit int,
	userID int64,
	admin bool,
) ([]ReviewAdjudication, error) {
	if _, err := service.authorizeReviewRound(ctx, roundID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.ListReviewAdjudications(ctx, roundID, cursor, limit)
}

// GetReviewFinding loads bounded evidence for one authorized finding.
func (service *Service) GetReviewFinding(
	ctx context.Context,
	findingID string,
	userID int64,
	admin bool,
) (*FindingDetail, error) {
	finding, err := service.store.GetReviewFinding(ctx, findingID)
	if err != nil {
		return nil, err
	}
	if _, err := service.authorizeReviewRound(ctx, finding.RoundID, userID, admin); err != nil {
		return nil, err
	}
	return finding, nil
}

// GetReviewGateResult returns the immutable Gate result for an authorized round.
func (service *Service) GetReviewGateResult(
	ctx context.Context,
	roundID string,
	userID int64,
	admin bool,
) (*ReviewGateResult, error) {
	if _, err := service.authorizeReviewRound(ctx, roundID, userID, admin); err != nil {
		return nil, err
	}
	return service.store.GetReviewGateResultByRound(ctx, roundID)
}

// ListReviewEvents returns one bounded ordered page for an authorized round.
func (service *Service) ListReviewEvents(
	ctx context.Context,
	roundID string,
	afterSeq int64,
	limit int,
	userID int64,
	admin bool,
) ([]ReviewEvent, error) {
	if afterSeq < 0 {
		return nil, ErrInvalid
	}
	_, reader, err := service.OpenReviewEvents(ctx, roundID, userID, admin)
	if err != nil {
		return nil, err
	}
	return reader.List(ctx, afterSeq, limit)
}

// ReviewEventReader scopes replay and live notifications to one authorized round.
type ReviewEventReader struct {
	store   Store
	hub     *ReviewEventHub
	roundID string
}

// OpenReviewEvents authorizes once before replaying or following a round.
func (service *Service) OpenReviewEvents(
	ctx context.Context,
	roundID string,
	userID int64,
	admin bool,
) (*ReviewRound, *ReviewEventReader, error) {
	round, err := service.authorizeReviewRound(ctx, roundID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	return round, &ReviewEventReader{
		store: service.store, hub: service.reviewHub, roundID: roundID,
	}, nil
}

// List returns one bounded durable page without repeating authorization.
func (reader *ReviewEventReader) List(
	ctx context.Context,
	afterSeq int64,
	limit int,
) ([]ReviewEvent, error) {
	if reader == nil || reader.store == nil {
		return nil, ErrUnavailable
	}
	if afterSeq < 0 {
		return nil, ErrInvalid
	}
	return reader.store.ListReviewEvents(ctx, reader.roundID, afterSeq, limit)
}

// Subscribe follows durable notifications after the reader has been authorized.
func (reader *ReviewEventReader) Subscribe() (<-chan ReviewEvent, func(), error) {
	if reader == nil || reader.hub == nil {
		return nil, nil, ErrUnavailable
	}
	channel, cancel := reader.hub.Subscribe(reader.roundID)
	return channel, cancel, nil
}

// SubscribeReviewRound authorizes before exposing persisted live notifications.
func (service *Service) SubscribeReviewRound(
	ctx context.Context,
	roundID string,
	userID int64,
	admin bool,
) (<-chan ReviewEvent, func(), error) {
	_, reader, err := service.OpenReviewEvents(ctx, roundID, userID, admin)
	if err != nil {
		return nil, nil, err
	}
	return reader.Subscribe()
}

// CancelReviewRound atomically persists cancellation before stopping local reviewers.
func (service *Service) CancelReviewRound(ctx context.Context, roundID string, admin bool) error {
	if !admin {
		return ErrForbidden
	}
	if service == nil || service.store == nil {
		return ErrUnavailable
	}
	return service.requestReviewRoundCancel(ctx, roundID)
}

// ListFindingResolutions returns the append-only audit facts for one authorized finding.
func (service *Service) ListFindingResolutions(
	ctx context.Context,
	findingID string,
	cursor FindingResolutionCursor,
	limit int,
	userID int64,
	admin bool,
) ([]FindingResolution, error) {
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	finding, err := service.store.GetReviewFinding(ctx, findingID)
	if err != nil {
		return nil, err
	}
	round, err := service.authorizeReviewRound(ctx, finding.RoundID, userID, admin)
	if err != nil {
		return nil, err
	}
	return service.store.ListFindingResolutions(
		ctx,
		findingID,
		round.Subject.ContentHash,
		cursor,
		limit,
	)
}

// CreateFindingResolution records one administrator-authored lifecycle fact.
func (service *Service) CreateFindingResolution(
	ctx context.Context,
	findingID string,
	request FindingResolutionRequest,
	actorID int64,
	admin bool,
) (*FindingResolution, error) {
	if !admin {
		return nil, ErrForbidden
	}
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	if findingID == "" || actorID <= 0 || request.SubjectHash == "" ||
		request.Rationale == "" || !validFindingResolutionKind(request.Resolution) {
		return nil, fmt.Errorf("resolution, subject_hash, rationale, and actor are required: %w", ErrInvalid)
	}
	if len(request.Rationale) > maxReviewComment {
		return nil, fmt.Errorf("resolution rationale exceeds %d bytes: %w", maxReviewComment, ErrInvalid)
	}
	now := service.now()
	switch request.Resolution {
	case ResolutionWaived:
		if request.ReplacementHash != "" {
			return nil, fmt.Errorf("waived resolution cannot have replacement_hash: %w", ErrInvalid)
		}
		if request.ExpiresAt != nil && !request.ExpiresAt.After(now) {
			return nil, fmt.Errorf("waiver expiry must be in the future: %w", ErrInvalid)
		}
	case ResolutionInvalidated:
		if request.ReplacementHash != "" || request.ExpiresAt != nil {
			return nil, fmt.Errorf("invalidated resolution cannot have replacement_hash or expires_at: %w", ErrInvalid)
		}
	case ResolutionFixed, ResolutionSuperseded:
		if request.ReplacementHash == "" || request.ReplacementHash == request.SubjectHash {
			return nil, fmt.Errorf("replacement_hash must identify a different subject: %w", ErrInvalid)
		}
		if request.ExpiresAt != nil {
			return nil, fmt.Errorf("%s resolution cannot expire: %w", request.Resolution, ErrInvalid)
		}
	}
	finding, err := service.store.GetReviewFinding(ctx, findingID)
	if err != nil {
		return nil, err
	}
	round, err := service.store.GetReviewRound(ctx, finding.RoundID)
	if err != nil {
		return nil, err
	}
	if round.Subject.ContentHash != request.SubjectHash {
		return nil, ErrNotFound
	}
	if request.Resolution == ResolutionFixed || request.Resolution == ResolutionSuperseded {
		if err := service.validateFindingReplacement(
			ctx,
			*round,
			request.ReplacementHash,
			request.Resolution == ResolutionFixed,
		); err != nil {
			return nil, err
		}
	}
	resolutionID, err := NewID("resolution")
	if err != nil {
		return nil, err
	}
	resolution := FindingResolution{
		ID: resolutionID, FindingID: findingID, Resolution: request.Resolution,
		SubjectHash: request.SubjectHash, ReplacementHash: request.ReplacementHash,
		Rationale: request.Rationale, ActorID: actorID,
		ExpiresAt: request.ExpiresAt, CreatedAt: now,
	}
	if err := service.store.CreateFindingResolution(ctx, resolution); err != nil {
		return nil, err
	}
	return &resolution, nil
}

// CreateFindingWaiver preserves the dedicated first-release mutation contract.
func (service *Service) CreateFindingWaiver(
	ctx context.Context,
	findingID, subjectHash, rationale string,
	expiresAt *time.Time,
	actorID int64,
	admin bool,
) (*FindingResolution, error) {
	return service.CreateFindingResolution(ctx, findingID, FindingResolutionRequest{
		Resolution: ResolutionWaived, SubjectHash: subjectHash,
		Rationale: rationale, ExpiresAt: expiresAt,
	}, actorID, admin)
}

func (service *Service) validateFindingReplacement(
	ctx context.Context,
	sourceRound ReviewRound,
	replacementHash string,
	requirePass bool,
) error {
	replacementRound, err := service.store.GetLatestCompletedReviewRoundBySubjectHash(
		ctx,
		replacementHash,
	)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("replacement subject has no completed review: %w", ErrConflict)
	}
	if err != nil {
		return err
	}
	if replacementRound.Status != RoundCompleted ||
		replacementRound.Subject.ContentHash != replacementHash ||
		replacementRound.Subject.Kind != sourceRound.Subject.Kind {
		return fmt.Errorf("replacement review does not match the finding subject: %w", ErrConflict)
	}
	if err := service.validateReplacementFamily(ctx, sourceRound.Subject, replacementRound.Subject); err != nil {
		return err
	}
	if !requirePass {
		return nil
	}
	gate, err := service.store.GetReviewGateResultByRound(ctx, replacementRound.ID)
	if err != nil {
		return err
	}
	if gate.RoundID != replacementRound.ID ||
		gate.SubjectHash != replacementHash ||
		gate.Decision != GatePass {
		return fmt.Errorf("fixed resolution requires a passing replacement review: %w", ErrConflict)
	}
	return nil
}

func (service *Service) validateReplacementFamily(
	ctx context.Context,
	source ReviewSubject,
	replacement ReviewSubject,
) error {
	switch source.Kind {
	case SubjectRequirement, SubjectRequirementAnalysis, SubjectTechnicalProposal,
		SubjectSystemDesign, SubjectImplementationPlan:
		original, err := service.store.GetArtifact(ctx, source.ID)
		if err != nil {
			return err
		}
		candidate, err := service.store.GetArtifact(ctx, replacement.ID)
		if err != nil {
			return err
		}
		current, err := BuildArtifactReviewSubject(*candidate)
		if err != nil {
			return err
		}
		if current != replacement ||
			candidate.RequestID != original.RequestID ||
			candidate.Kind != original.Kind ||
			candidate.Version <= original.Version {
			return fmt.Errorf("replacement artifact is not a newer subject version: %w", ErrConflict)
		}
	case SubjectChangeSet, SubjectValidationBundle, SubjectDeliveryBundle:
		original, err := service.store.GetImplementation(ctx, source.ID)
		if err != nil {
			return err
		}
		candidate, err := service.store.GetImplementation(ctx, replacement.ID)
		if err != nil {
			return err
		}
		current, err := service.buildImplementationReviewSubject(ctx, source.Kind, *candidate)
		if err != nil {
			return err
		}
		if current != replacement ||
			candidate.RequestID != original.RequestID ||
			candidate.Repo != original.Repo ||
			(candidate.ParentRunID != original.ID &&
				!candidate.CreatedAt.After(original.CreatedAt)) {
			return fmt.Errorf("replacement implementation is not a newer subject version: %w", ErrConflict)
		}
	default:
		return ErrConflict
	}
	return nil
}

// ExecuteReviewRound runs the pinned reviewer panel and persists its deterministic Gate.
func (service *Service) ExecuteReviewRound(
	ctx context.Context,
	roundID string,
	actor agentapi.Actor,
	admin bool,
) (*ReviewGateResult, error) {
	if !admin {
		return nil, ErrForbidden
	}
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	adjudicator := service.adjudicationRunner()
	round, err := service.store.GetReviewRound(ctx, roundID)
	if err != nil {
		return nil, err
	}
	policy, err := service.store.GetReviewPolicy(ctx, round.PolicyID, round.PolicyVersion)
	if err != nil {
		return nil, err
	}
	if policy.ContentHash != round.PolicyHash {
		snapshotErr := fmt.Errorf(
			"review round policy snapshot does not match published policy: %w",
			ErrConflict,
		)
		return nil, service.failReviewRound(ctx, round.ID, RoundCreated, snapshotErr)
	}
	if err := ValidateReviewRoundSnapshot(*policy, *round); err != nil {
		return nil, service.failReviewRound(ctx, round.ID, RoundCreated, err)
	}
	assignments, err := service.store.ListReviewAssignments(ctx, round.ID, ReviewAssignmentCursor{}, maxReviewersPerPolicy)
	if err != nil {
		return nil, err
	}
	if err := validateReviewAssignmentSnapshot(round.Reviewers, assignments); err != nil {
		return nil, service.failReviewRound(ctx, round.ID, RoundCreated, err)
	}
	runner := service.reviewRunner()
	var reviewContext agentapi.ContextBlock
	if reviewAssignmentsNeedRunner(assignments) {
		if runner == nil {
			return nil, ErrUnavailable
		}
		reviewContext, err = service.buildReviewContext(ctx, round.Subject)
		if err != nil {
			return nil, service.failReviewRound(ctx, round.ID, RoundCreated, err)
		}
	}
	executionCtx, cancel := context.WithCancelCause(ctx)
	if !service.registerReviewCancel(round.ID, cancel) {
		cancel(nil)
		return nil, ErrConflict
	}
	defer func() {
		service.unregisterReviewCancel(round.ID)
		cancel(nil)
	}()
	now := service.now()
	if err := service.store.TransitionReviewRound(executionCtx, round.ID, RoundCreated, RoundRunning, now); err != nil {
		if cause := context.Cause(executionCtx); cause != nil {
			return nil, errors.Join(cause, err)
		}
		return nil, err
	}
	round.Status = RoundRunning
	if _, err := service.appendReviewEvent(
		context.WithoutCancel(executionCtx), round.ID,
		ReviewEventRoundStarted, "review round started", nil,
	); err != nil {
		return nil, service.failReviewRound(executionCtx, round.ID, RoundRunning, err)
	}

	sem := make(chan struct{}, policy.MaxParallelism)
	var wg sync.WaitGroup
	var runErrors []error
	var errorsMu sync.Mutex
	recordError := func(err error) {
		if err == nil {
			return
		}
		errorsMu.Lock()
		runErrors = append(runErrors, err)
		errorsMu.Unlock()
	}
	for index := range assignments {
		assignment := assignments[index]
		if successfulReviewAssignment(assignment.Status) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-executionCtx.Done():
				return
			}
			startedAt := service.now()
			if err := service.store.TransitionReviewAssignment(
				executionCtx, assignment.ID, AssignmentQueued, AssignmentRunning, assignment.ID, "", startedAt,
			); err != nil {
				if executionCtx.Err() != nil {
					return
				}
				recordError(fmt.Errorf("start review assignment %q: %w", assignment.ID, err))
				return
			}
			assignment.Status = AssignmentRunning
			assignment.AgentRunID = assignment.ID
			if _, err := service.appendReviewEvent(
				context.WithoutCancel(executionCtx), round.ID,
				ReviewEventAssignmentStarted, "review assignment started",
				reviewAssignmentEventDetail(assignment, ""),
			); err != nil {
				recordError(fmt.Errorf("append start event for review assignment %q: %w", assignment.ID, err))
				if transitionErr := service.store.TransitionReviewAssignment(
					context.WithoutCancel(executionCtx), assignment.ID,
					AssignmentRunning, AssignmentFailed, assignment.ID,
					"event_persistence_failed", service.now(),
				); transitionErr != nil && !errors.Is(transitionErr, ErrConflict) {
					recordError(fmt.Errorf("fail review assignment %q after event error: %w", assignment.ID, transitionErr))
				}
				return
			}
			report, runErr := runner.Run(executionCtx, ReviewAssignmentRequest{
				Round: *round, Policy: *policy, Assignment: assignment,
				Context: reviewContext, Actor: actor,
			})
			if runErr != nil {
				if executionCtx.Err() != nil {
					return
				}
				errorCode := reviewErrorCode(runErr)
				if err := service.store.TransitionReviewAssignment(
					context.WithoutCancel(executionCtx), assignment.ID, AssignmentRunning, AssignmentFailed,
					assignment.ID, errorCode, service.now(),
				); err != nil {
					recordError(fmt.Errorf("fail review assignment %q: %w", assignment.ID, err))
					return
				}
				if _, err := service.appendReviewEvent(
					context.WithoutCancel(executionCtx), round.ID,
					ReviewEventAssignmentFailed, "review assignment failed",
					reviewAssignmentEventDetail(assignment, errorCode),
				); err != nil {
					recordError(fmt.Errorf("append failure event for review assignment %q: %w", assignment.ID, err))
				}
				return
			}
			if report.CompletedAt.IsZero() {
				report.CompletedAt = service.now()
			}
			prepared, prepareErr := PrepareReviewReport(report, assignment, round.Subject.ContentHash)
			if prepareErr != nil {
				if err := service.store.TransitionReviewAssignment(
					context.WithoutCancel(executionCtx), assignment.ID, AssignmentRunning, AssignmentFailed,
					assignment.ID, "invalid_report", service.now(),
				); err != nil {
					recordError(fmt.Errorf("reject review assignment %q report: %w", assignment.ID, err))
					return
				}
				if _, err := service.appendReviewEvent(
					context.WithoutCancel(executionCtx), round.ID,
					ReviewEventAssignmentFailed, "review assignment report rejected",
					reviewAssignmentEventDetail(assignment, "invalid_report"),
				); err != nil {
					recordError(fmt.Errorf("append rejection event for review assignment %q: %w", assignment.ID, err))
				}
				return
			}
			if err := service.store.CompleteReviewAssignment(context.WithoutCancel(executionCtx), prepared); err != nil {
				if executionCtx.Err() != nil {
					return
				}
				recordError(fmt.Errorf("persist review assignment %q report: %w", assignment.ID, err))
				if transitionErr := service.store.TransitionReviewAssignment(
					context.WithoutCancel(executionCtx), assignment.ID, AssignmentRunning, AssignmentFailed,
					assignment.ID, "report_persistence_failed", service.now(),
				); transitionErr != nil && !errors.Is(transitionErr, ErrConflict) {
					recordError(fmt.Errorf("fail review assignment %q after report error: %w", assignment.ID, transitionErr))
				}
				return
			}
			if _, err := service.appendReviewEvent(
				context.WithoutCancel(executionCtx), round.ID,
				ReviewEventAssignmentSucceeded, "review assignment succeeded",
				reviewAssignmentEventDetail(assignment, ""),
			); err != nil {
				recordError(fmt.Errorf("append success event for review assignment %q: %w", assignment.ID, err))
			}
		}()
	}
	wg.Wait()
	if cause := context.Cause(executionCtx); cause != nil {
		cleanupErr := service.requestReviewRoundCancel(context.WithoutCancel(executionCtx), round.ID)
		return nil, errors.Join(cause, cleanupErr)
	}
	if len(runErrors) > 0 {
		runErr := errors.Join(runErrors...)
		return nil, service.failReviewRound(executionCtx, round.ID, RoundRunning, runErr)
	}
	if err := service.store.TransitionReviewRound(executionCtx, round.ID, RoundRunning, RoundEvaluating, service.now()); err != nil {
		return nil, service.failReviewRound(executionCtx, round.ID, RoundRunning, err)
	}
	if _, err := service.appendReviewEvent(
		context.WithoutCancel(executionCtx), round.ID,
		ReviewEventRoundEvaluating, "review round evaluating", nil,
	); err != nil {
		return nil, service.failReviewRound(executionCtx, round.ID, RoundEvaluating, err)
	}
	evaluation, err := service.store.LoadFullReviewEvaluation(executionCtx, round.ID)
	if err != nil {
		return nil, service.failReviewRound(executionCtx, round.ID, RoundEvaluating, err)
	}
	if err := service.attachReviewSubjectGateFacts(executionCtx, &evaluation); err != nil {
		return nil, service.failReviewRound(executionCtx, round.ID, RoundEvaluating, err)
	}
	result, err := EvaluateReviewGate(evaluation, service.now())
	if err != nil {
		return nil, service.failReviewRound(executionCtx, round.ID, RoundEvaluating, err)
	}
	if len(result.ConflictIDs) > 0 && policy.Adjudicator != nil {
		if _, err := service.executeReviewAdjudications(
			executionCtx,
			adjudicator,
			actor,
			reviewContext,
			evaluation,
			result.ConflictIDs,
		); err != nil {
			return nil, service.failReviewRound(executionCtx, round.ID, RoundEvaluating, err)
		}
		evaluation, err = service.store.LoadFullReviewEvaluation(executionCtx, round.ID)
		if err != nil {
			return nil, service.failReviewRound(executionCtx, round.ID, RoundEvaluating, err)
		}
		if err := service.attachReviewSubjectGateFacts(executionCtx, &evaluation); err != nil {
			return nil, service.failReviewRound(executionCtx, round.ID, RoundEvaluating, err)
		}
		result, err = EvaluateReviewGate(evaluation, service.now())
		if err != nil {
			return nil, service.failReviewRound(executionCtx, round.ID, RoundEvaluating, err)
		}
	}
	if err := service.store.CompleteReviewRound(executionCtx, result, service.now()); err != nil {
		return nil, service.failReviewRound(executionCtx, round.ID, RoundEvaluating, err)
	}
	if _, err := service.appendReviewEvent(
		context.WithoutCancel(executionCtx), round.ID,
		ReviewEventRoundCompleted, "review round completed",
		reviewGateEventDetail(result),
	); err != nil {
		return nil, err
	}
	return &result, nil
}

type adjudicationConflict struct {
	fingerprint string
	findings    []Finding
}

func (service *Service) executeReviewAdjudications(
	ctx context.Context,
	runner AdjudicationRunner,
	actor agentapi.Actor,
	reviewContext agentapi.ContextBlock,
	evaluation ReviewEvaluation,
	conflictIDs []string,
) (ReviewUsage, error) {
	conflicts, err := adjudicationConflicts(evaluation.Reports, conflictIDs)
	if err != nil {
		return ReviewUsage{}, err
	}
	persisted := make(map[string]struct{}, len(evaluation.Adjudications))
	for _, adjudication := range evaluation.Adjudications {
		persisted[adjudication.Fingerprint] = struct{}{}
	}
	adjudications := make([]ReviewAdjudication, 0, len(conflicts))
	var usage ReviewUsage
	for _, conflict := range conflicts {
		if _, ok := persisted[conflict.fingerprint]; ok {
			continue
		}
		if _, err := service.appendReviewEvent(
			context.WithoutCancel(ctx),
			evaluation.Round.ID,
			ReviewEventAdjudicationStarted,
			"review adjudication started",
			reviewAdjudicationEventDetail(conflict.fingerprint, "", ""),
		); err != nil {
			return usage, err
		}
		outcome := AdjudicationOutcome{}
		var runErr error
		if runner == nil {
			runErr = ErrUnavailable
		} else {
			request := ReviewAdjudicationRequest{
				Round: evaluation.Round, Policy: evaluation.Policy,
				Findings: conflict.findings, Context: reviewContext, Actor: actor,
			}
			if usageRunner, ok := runner.(AdjudicationRunnerWithUsage); ok {
				runResult, err := usageRunner.RunWithUsage(ctx, request)
				outcome, runErr = runResult.Outcome, err
				usage = addReviewUsage(usage, runResult.Usage)
			} else {
				outcome, runErr = runner.Run(ctx, request)
			}
		}
		errorCode := ""
		if runErr != nil {
			outcome.Decision = AdjudicationNeedsHuman
			outcome.Rationale = truncateText(runErr.Error(), maxReviewEventSummaryBytes)
			errorCode = adjudicationErrorCode(runErr)
		}
		spec := evaluation.Policy.Adjudicator
		adjudication, err := PrepareReviewAdjudication(ReviewAdjudication{
			RoundID: evaluation.Round.ID, SubjectHash: evaluation.Round.Subject.ContentHash,
			PolicyHash: evaluation.Policy.ContentHash, Fingerprint: conflict.fingerprint,
			FindingIDs: findingIDs(conflict.findings), Agent: spec.Agent,
			DefinitionHash: spec.DefinitionHash, Decision: outcome.Decision,
			Rationale: outcome.Rationale, ErrorCode: errorCode, CreatedAt: service.now(),
		})
		if err != nil {
			return usage, err
		}
		adjudications = append(adjudications, adjudication)
	}
	if err := service.store.SaveReviewAdjudications(ctx, adjudications); err != nil {
		return usage, err
	}
	for _, adjudication := range adjudications {
		if _, err := service.appendReviewEvent(
			context.WithoutCancel(ctx),
			evaluation.Round.ID,
			ReviewEventAdjudicationFinished,
			"review adjudication finished",
			reviewAdjudicationEventDetail(
				adjudication.Fingerprint,
				adjudication.Decision,
				adjudication.ErrorCode,
			),
		); err != nil {
			return usage, err
		}
	}
	return usage, nil
}

func adjudicationConflicts(
	reports []ReviewReport,
	conflictIDs []string,
) ([]adjudicationConflict, error) {
	pending := make(map[string]struct{}, len(conflictIDs))
	for _, id := range conflictIDs {
		pending[id] = struct{}{}
	}
	byFingerprint := make(map[string][]Finding)
	for _, report := range reports {
		for _, finding := range report.Findings {
			if _, ok := pending[finding.ID]; !ok {
				continue
			}
			byFingerprint[finding.Fingerprint] = append(
				byFingerprint[finding.Fingerprint],
				finding,
			)
			delete(pending, finding.ID)
		}
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("review conflict findings are incomplete: %w", ErrConflict)
	}
	conflicts := make([]adjudicationConflict, 0, len(byFingerprint))
	for fingerprint, findings := range byFingerprint {
		sort.Slice(findings, func(i, j int) bool {
			return findings[i].ID < findings[j].ID
		})
		if fingerprint == "" || len(findings) < 2 {
			return nil, fmt.Errorf("review conflict group is invalid: %w", ErrConflict)
		}
		conflicts = append(conflicts, adjudicationConflict{
			fingerprint: fingerprint,
			findings:    findings,
		})
	}
	sort.Slice(conflicts, func(i, j int) bool {
		return conflicts[i].fingerprint < conflicts[j].fingerprint
	})
	return conflicts, nil
}

func findingIDs(findings []Finding) []string {
	ids := make([]string, 0, len(findings))
	for _, finding := range findings {
		ids = append(ids, finding.ID)
	}
	return ids
}

func validateReviewAssignmentSnapshot(reviewers []ReviewerSpec, assignments []ReviewAssignment) error {
	if len(assignments) != len(reviewers) {
		return fmt.Errorf("review round assignment snapshot is incomplete: %w", ErrConflict)
	}
	byID := make(map[string]ReviewerSpec, len(reviewers))
	for _, reviewer := range reviewers {
		byID[reviewer.ID] = reviewer
	}
	seen := make(map[string]struct{}, len(assignments))
	for _, assignment := range assignments {
		reviewer, ok := byID[assignment.ReviewerID]
		if _, duplicate := seen[assignment.ReviewerID]; duplicate {
			return fmt.Errorf("review assignment %q duplicates reviewer %q: %w", assignment.ID, assignment.ReviewerID, ErrConflict)
		}
		seen[assignment.ReviewerID] = struct{}{}
		if !ok || assignment.ID == "" || assignment.Agent != reviewer.Agent ||
			assignment.DefinitionHash != reviewer.DefinitionHash ||
			assignment.Required != reviewer.Required ||
			!slices.Equal(assignment.Categories, reviewer.Categories) ||
			(assignment.Status != AssignmentQueued &&
				assignment.Status != AssignmentReused) {
			return fmt.Errorf("review assignment %q does not match policy snapshot: %w", assignment.ID, ErrConflict)
		}
	}
	return nil
}

func (service *Service) failReviewRound(
	ctx context.Context,
	roundID string,
	from ReviewRoundStatus,
	cause error,
) error {
	transitionErr := service.store.TransitionReviewRound(
		context.WithoutCancel(ctx), roundID, from, RoundFailed, service.now(),
	)
	if transitionErr != nil {
		return errors.Join(cause, transitionErr)
	}
	_, eventErr := service.appendReviewEvent(
		context.WithoutCancel(ctx), roundID,
		ReviewEventRoundFailed, "review round failed",
		reviewFailureEventDetail(cause),
	)
	return errors.Join(cause, eventErr)
}

func (service *Service) authorizeReviewRound(
	ctx context.Context,
	roundID string,
	userID int64,
	admin bool,
) (*ReviewRound, error) {
	if service == nil || service.store == nil {
		return nil, ErrUnavailable
	}
	round, err := service.store.GetReviewRound(ctx, roundID)
	if err != nil {
		return nil, err
	}
	switch round.Subject.Kind {
	case SubjectRequirement, SubjectRequirementAnalysis, SubjectTechnicalProposal,
		SubjectSystemDesign, SubjectImplementationPlan:
		artifact, err := service.store.GetArtifact(ctx, round.Subject.ID)
		if err != nil {
			return nil, err
		}
		if artifact.ContentHash != round.Subject.SourceContentHash {
			return nil, ErrConflict
		}
		if _, err := service.authorizedFeature(ctx, artifact.RequestID, userID, admin); err != nil {
			return nil, err
		}
	case SubjectChangeSet, SubjectValidationBundle, SubjectDeliveryBundle:
		run, err := service.GetImplementation(ctx, round.Subject.ID, userID, admin)
		if err != nil {
			return nil, err
		}
		current, err := service.buildImplementationReviewSubject(ctx, round.Subject.Kind, *run)
		if err != nil {
			return nil, err
		}
		if current != round.Subject {
			return nil, ErrConflict
		}
	default:
		return nil, ErrConflict
	}
	return round, nil
}

func (service *Service) attachReviewSubjectGateFacts(
	ctx context.Context,
	evaluation *ReviewEvaluation,
) error {
	switch evaluation.Round.Subject.Kind {
	case SubjectValidationBundle, SubjectDeliveryBundle:
	default:
		return nil
	}
	run, err := service.store.GetImplementation(ctx, evaluation.Round.Subject.ID)
	if err != nil {
		return err
	}
	current, err := service.buildImplementationReviewSubject(
		ctx,
		evaluation.Round.Subject.Kind,
		*run,
	)
	if err != nil {
		return err
	}
	if current != evaluation.Round.Subject {
		return fmt.Errorf("review subject changed before Gate evaluation: %w", ErrConflict)
	}
	for _, result := range run.ChangeSet.ValidationResults {
		switch result.Status {
		case "failed":
			evaluation.SubjectReasonCodes = append(
				evaluation.SubjectReasonCodes,
				reasonValidationFailed,
			)
			evaluation.SubjectBlockingIDs = append(
				evaluation.SubjectBlockingIDs,
				fmt.Sprintf("validation:%d", result.Sequence),
			)
		case "validation_not_configured":
			evaluation.SubjectReasonCodes = append(
				evaluation.SubjectReasonCodes,
				reasonValidationNotConfigured,
			)
			evaluation.SubjectCoverageGaps = append(
				evaluation.SubjectCoverageGaps,
				"validation_execution",
			)
		}
	}
	return nil
}

func (service *Service) requestReviewRoundCancel(ctx context.Context, roundID string) error {
	changed, err := service.store.RequestReviewRoundCancel(ctx, roundID, service.now())
	if err != nil {
		return err
	}
	service.reviewCancelMu.Lock()
	cancel := service.reviewCancels[roundID]
	service.reviewCancelMu.Unlock()
	if cancel != nil {
		cancel(errReviewRoundCancelled)
	}
	if !changed {
		return nil
	}
	_, err = service.appendReviewEvent(
		context.WithoutCancel(ctx), roundID,
		ReviewEventRoundCancelled, "review round cancelled", nil,
	)
	return err
}

func (service *Service) appendReviewEvent(
	ctx context.Context,
	roundID string,
	kind ReviewEventKind,
	summary string,
	detail json.RawMessage,
) (*ReviewEvent, error) {
	if len(detail) > maxReviewEventDetailBytes || (len(detail) > 0 && !json.Valid(detail)) {
		return nil, fmt.Errorf("review event detail is invalid or exceeds %d bytes: %w", maxReviewEventDetailBytes, ErrInvalid)
	}
	summary = platform.RedactSensitiveText(summary)
	if len(detail) > 0 {
		detail = json.RawMessage(platform.RedactSensitiveText(string(detail)))
	}
	event, err := service.store.AppendReviewEvent(ctx, ReviewEvent{
		RoundID: roundID,
		Kind:    kind, Summary: truncateText(summary, maxReviewEventSummaryBytes),
		Detail: append(json.RawMessage(nil), detail...), CreatedAt: service.now(),
	})
	if err != nil {
		return nil, err
	}
	if service.reviewHub != nil {
		service.reviewHub.Publish(*event)
	}
	return event, nil
}

func (service *Service) registerReviewCancel(roundID string, cancel context.CancelCauseFunc) bool {
	service.reviewCancelMu.Lock()
	defer service.reviewCancelMu.Unlock()
	if _, exists := service.reviewCancels[roundID]; exists {
		return false
	}
	service.reviewCancels[roundID] = cancel
	return true
}

func (service *Service) unregisterReviewCancel(roundID string) {
	service.reviewCancelMu.Lock()
	delete(service.reviewCancels, roundID)
	service.reviewCancelMu.Unlock()
}

func reviewAssignmentEventDetail(assignment ReviewAssignment, errorCode string) json.RawMessage {
	raw, _ := json.Marshal(struct {
		AssignmentID string `json:"assignment_id"`
		ReviewerID   string `json:"reviewer_id"`
		ErrorCode    string `json:"error_code,omitempty"`
	}{
		AssignmentID: assignment.ID,
		ReviewerID:   assignment.ReviewerID,
		ErrorCode:    errorCode,
	})
	return raw
}

func reviewGateEventDetail(result ReviewGateResult) json.RawMessage {
	raw, _ := json.Marshal(struct {
		GateResultID string       `json:"gate_result_id"`
		Decision     GateDecision `json:"decision"`
	}{
		GateResultID: result.ID,
		Decision:     result.Decision,
	})
	return raw
}

func reviewAdjudicationEventDetail(
	fingerprint string,
	decision AdjudicationDecision,
	errorCode string,
) json.RawMessage {
	raw, _ := json.Marshal(struct {
		Fingerprint string               `json:"fingerprint"`
		Decision    AdjudicationDecision `json:"decision,omitempty"`
		ErrorCode   string               `json:"error_code,omitempty"`
	}{
		Fingerprint: fingerprint,
		Decision:    decision,
		ErrorCode:   errorCode,
	})
	return raw
}

func reviewFailureEventDetail(cause error) json.RawMessage {
	raw, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{
		Error: truncateText(
			platform.RedactSensitiveText(cause.Error()),
			maxReviewEventSummaryBytes,
		),
	})
	return raw
}

func (service *Service) validateApprovalBinding(
	ctx context.Context,
	subject ReviewSubject,
	binding ReviewApprovalBinding,
	decision ReviewDecision,
	comment string,
) error {
	if binding.SubjectHash == "" || binding.ReviewRoundID == "" || binding.GateResultID == "" {
		return fmt.Errorf("subject_hash, review_round_id, and gate_result_id are required: %w", ErrInvalid)
	}
	if binding.SubjectHash != subject.ContentHash {
		return fmt.Errorf("review subject is stale: %w", ErrConflict)
	}
	round, err := service.store.GetReviewRound(ctx, binding.ReviewRoundID)
	if err != nil {
		return err
	}
	if round.Status != RoundCompleted || round.Subject.ID != subject.ID ||
		round.Subject.Kind != subject.Kind || round.Subject.ContentHash != subject.ContentHash {
		return fmt.Errorf("review round does not match current subject: %w", ErrConflict)
	}
	gate, err := service.store.GetReviewGateResult(ctx, binding.GateResultID)
	if err != nil {
		return err
	}
	if gate.RoundID != round.ID || gate.SubjectHash != subject.ContentHash || !validGateDecision(gate.Decision) {
		return fmt.Errorf("gate result does not match review round: %w", ErrConflict)
	}
	if decision == DecisionRejected {
		return nil
	}
	switch gate.Decision {
	case GatePass:
		return nil
	case GateHumanRequired:
		if comment == "" {
			return fmt.Errorf("human-required approval needs an explicit disposition: %w", ErrConflict)
		}
		return nil
	case GateRevise:
		resolutions, err := service.store.ListFindingResolutionsByIDs(ctx, gate.BlockingIDs, subject.ContentHash)
		if err != nil {
			return err
		}
		disposed := make(map[string]struct{}, len(resolutions))
		now := service.now()
		for findingID, resolution := range latestFindingResolutions(resolutions, subject.ContentHash) {
			if (resolution.Resolution != ResolutionWaived &&
				resolution.Resolution != ResolutionInvalidated) ||
				(resolution.ExpiresAt != nil && !resolution.ExpiresAt.After(now)) {
				continue
			}
			disposed[findingID] = struct{}{}
		}
		for _, findingID := range gate.BlockingIDs {
			if _, ok := disposed[findingID]; !ok {
				return fmt.Errorf("blocking finding %q is not waived or invalidated: %w", findingID, ErrConflict)
			}
		}
		return nil
	case GateIncomplete, GateFailed:
		return fmt.Errorf("gate decision %q cannot be approved: %w", gate.Decision, ErrConflict)
	default:
		return fmt.Errorf("unsupported gate decision %q: %w", gate.Decision, ErrConflict)
	}
}

func reviewErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "reviewer_timeout"
	case errors.Is(err, context.Canceled):
		return "reviewer_cancelled"
	default:
		return "reviewer_failed"
	}
}

func adjudicationErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "adjudicator_timeout"
	case errors.Is(err, context.Canceled):
		return "adjudicator_cancelled"
	case errors.Is(err, ErrUnavailable):
		return "adjudicator_unavailable"
	default:
		return "adjudicator_failed"
	}
}
