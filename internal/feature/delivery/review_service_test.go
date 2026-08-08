package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

type reviewServiceStore struct {
	Store
	feature            FeatureRequest
	artifact           Artifact
	artifacts          map[string]Artifact
	implementations    map[string]ImplementationRun
	policy             ReviewPolicy
	savedPolicy        ReviewPolicy
	round              ReviewRound
	replacementRound   ReviewRound
	assignments        []ReviewAssignment
	report             ReviewReport
	finding            FindingDetail
	gate               ReviewGateResult
	resolution         FindingResolution
	resolutions        []FindingResolution
	resolutionSubject  string
	resolutionCursor   FindingResolutionCursor
	resolutionLimit    int
	resolutionIDs      []string
	events             []ReviewEvent
	eventAfter         int64
	eventLimit         int
	adjudications      []ReviewAdjudication
	adjudicationCursor ReviewAdjudicationCursor
	adjudicationLimit  int
	reuseSources       []ReviewReportReuseSource
	reusedReports      []ReviewReport
	reportReuses       []ReviewReportReuse
}

type reviewRolloutServiceStore struct {
	*reviewServiceStore
	policies       map[ReviewPolicyRef]ReviewPolicyRecord
	defaults       map[SubjectKind]ReviewPolicyRef
	rollout        ReviewPolicyRolloutRule
	rolloutFound   bool
	rolloutAudit   []ReviewPolicyRolloutAuditEvent
	rolloutAfter   int64
	rolloutLimit   int
	savedRollout   ReviewPolicyRolloutRule
	savedRolloutBy int64
}

func (store *reviewRolloutServiceStore) PublishReviewPolicies(
	context.Context,
	[]ReviewPolicy,
	int64,
) error {
	return nil
}

func (store *reviewRolloutServiceStore) ListReviewPolicyRecords(
	context.Context,
	ReviewPolicyCursor,
	int,
) ([]ReviewPolicyRecord, error) {
	return nil, nil
}

func (store *reviewRolloutServiceStore) GetReviewPolicyRecord(
	_ context.Context,
	id string,
	version int64,
) (ReviewPolicyRecord, error) {
	record, ok := store.policies[ReviewPolicyRef{ID: id, Version: version}]
	if !ok {
		return ReviewPolicyRecord{}, ErrNotFound
	}
	return record, nil
}

func (store *reviewRolloutServiceStore) GetDefaultReviewPolicy(
	_ context.Context,
	kind SubjectKind,
) (ReviewPolicyRef, error) {
	ref, ok := store.defaults[kind]
	if !ok {
		return ReviewPolicyRef{}, ErrNotFound
	}
	return ref, nil
}

func (store *reviewRolloutServiceStore) EnsureReviewPolicyDefault(
	context.Context,
	string,
	int64,
	int64,
) error {
	return nil
}

func (store *reviewRolloutServiceStore) SetReviewPolicyDefault(
	context.Context,
	string,
	int64,
	int64,
) error {
	return nil
}

func (store *reviewRolloutServiceStore) SetReviewPolicyActive(
	context.Context,
	string,
	int64,
	bool,
	int64,
) error {
	return nil
}

func (store *reviewRolloutServiceStore) ListReviewPolicyAudit(
	context.Context,
	string,
	int64,
	int,
) ([]ReviewPolicyAuditEvent, error) {
	return nil, nil
}

func (store *reviewRolloutServiceStore) GetReviewPolicyRollout(
	_ context.Context,
	_ SubjectKind,
) (ReviewPolicyRolloutRule, bool, error) {
	return store.rollout, store.rolloutFound, nil
}

func (store *reviewRolloutServiceStore) SetReviewPolicyRollout(
	_ context.Context,
	rule ReviewPolicyRolloutRule,
	actorUserID int64,
) error {
	store.savedRollout = rule
	store.savedRolloutBy = actorUserID
	store.rollout = rule
	store.rolloutFound = true
	return nil
}

func (store *reviewRolloutServiceStore) ListReviewPolicyRolloutAudit(
	_ context.Context,
	_ SubjectKind,
	afterSeq int64,
	limit int,
) ([]ReviewPolicyRolloutAuditEvent, error) {
	store.rolloutAfter = afterSeq
	store.rolloutLimit = limit
	return append([]ReviewPolicyRolloutAuditEvent(nil), store.rolloutAudit...), nil
}

func (store *reviewRolloutServiceStore) GetReviewPolicy(
	_ context.Context,
	id string,
	version int64,
) (*ReviewPolicy, error) {
	record, ok := store.policies[ReviewPolicyRef{ID: id, Version: version}]
	if !ok {
		return nil, ErrNotFound
	}
	policy := record.ReviewPolicy
	return &policy, nil
}

type executionReviewStore struct {
	Store
	mu                sync.Mutex
	feature           FeatureRequest
	artifacts         map[string]Artifact
	implementation    ImplementationRun
	round             ReviewRound
	policy            ReviewPolicy
	assignments       []ReviewAssignment
	reports           []ReviewReport
	adjudications     []ReviewAdjudication
	gate              ReviewGateResult
	reportError       error
	adjudicationError error
	eventError        error
	events            []ReviewEvent
	evaluationLoads   int
	gateCompletions   int
}

type reviewRunnerFunc func(context.Context, ReviewAssignmentRequest) (ReviewReport, error)

func (runner reviewRunnerFunc) Run(
	ctx context.Context,
	request ReviewAssignmentRequest,
) (ReviewReport, error) {
	return runner(ctx, request)
}

type adjudicationRunnerFunc func(
	context.Context,
	ReviewAdjudicationRequest,
) (AdjudicationOutcome, error)

func (runner adjudicationRunnerFunc) Run(
	ctx context.Context,
	request ReviewAdjudicationRequest,
) (AdjudicationOutcome, error) {
	return runner(ctx, request)
}

type agentRuntimeFunc func(context.Context, agentapi.RunRequest) (agentapi.RunResult, error)

func (runtime agentRuntimeFunc) Run(
	ctx context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	return runtime(ctx, request)
}

func (store *executionReviewStore) GetReviewRound(_ context.Context, id string) (*ReviewRound, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if id != store.round.ID {
		return nil, ErrNotFound
	}
	round := store.round
	return &round, nil
}

func (store *executionReviewStore) BindReviewRoundWorkflow(
	_ context.Context,
	roundID, workflowRunID string,
	_ time.Time,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if roundID != store.round.ID {
		return ErrNotFound
	}
	if store.round.WorkflowRunID != "" && store.round.WorkflowRunID != workflowRunID {
		return ErrConflict
	}
	store.round.WorkflowRunID = workflowRunID
	return nil
}

func (store *executionReviewStore) GetFeature(_ context.Context, id string) (*FeatureRequest, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if id != store.feature.ID {
		return nil, ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *executionReviewStore) GetArtifact(_ context.Context, id string) (*Artifact, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	artifact, ok := store.artifacts[id]
	if !ok {
		return nil, ErrNotFound
	}
	artifact.DocumentJSON = append([]byte(nil), artifact.DocumentJSON...)
	artifact.Evidence = append([]EvidenceRef(nil), artifact.Evidence...)
	return &artifact, nil
}

func (store *executionReviewStore) GetImplementation(
	_ context.Context,
	id string,
) (*ImplementationRun, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if id != store.implementation.ID {
		return nil, ErrNotFound
	}
	run := store.implementation
	if run.ChangeSet != nil {
		changeSet := *run.ChangeSet
		changeSet.Files = append([]ChangedFile(nil), changeSet.Files...)
		changeSet.PlanDeviations = append([]PlanDeviation(nil), changeSet.PlanDeviations...)
		changeSet.ValidationResults = append([]ValidationResult(nil), changeSet.ValidationResults...)
		run.ChangeSet = &changeSet
	}
	return &run, nil
}

func (store *executionReviewStore) CreateReviewRound(
	_ context.Context,
	round ReviewRound,
	assignments []ReviewAssignment,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.round = round
	store.assignments = append([]ReviewAssignment(nil), assignments...)
	return nil
}

func (store *executionReviewStore) GetReviewPolicy(
	_ context.Context,
	id string,
	version int64,
) (*ReviewPolicy, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if id != store.policy.ID || version != store.policy.Version {
		return nil, ErrNotFound
	}
	policy := store.policy
	return &policy, nil
}

func (store *executionReviewStore) ListReviewAssignments(
	_ context.Context,
	roundID string,
	_ ReviewAssignmentCursor,
	_ int,
) ([]ReviewAssignment, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if roundID != store.round.ID {
		return nil, ErrNotFound
	}
	assignments := append([]ReviewAssignment(nil), store.assignments...)
	for index := range assignments {
		assignments[index].Categories = append([]string(nil), assignments[index].Categories...)
	}
	return assignments, nil
}

func (store *executionReviewStore) GetLatestReviewAssignment(
	_ context.Context,
	roundID, reviewerID string,
) (*ReviewAssignment, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if roundID != store.round.ID {
		return nil, ErrNotFound
	}
	var latest *ReviewAssignment
	for index := range store.assignments {
		assignment := &store.assignments[index]
		if assignment.RoundID != roundID || assignment.ReviewerID != reviewerID {
			continue
		}
		if latest == nil || assignment.Attempt > latest.Attempt {
			latest = assignment
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	copy := *latest
	copy.Categories = append([]string(nil), latest.Categories...)
	return &copy, nil
}

func (store *executionReviewStore) StartReviewAssignmentAttempt(
	_ context.Context,
	roundID, reviewerID string,
	attempt int,
	agentRunID string,
	at time.Time,
) (*ReviewAssignment, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if roundID != store.round.ID {
		return nil, ErrNotFound
	}
	if store.round.Status != RoundRunning {
		return nil, ErrConflict
	}
	latestIndex := -1
	for index := range store.assignments {
		assignment := &store.assignments[index]
		if assignment.RoundID == roundID && assignment.ReviewerID == reviewerID &&
			(latestIndex < 0 || assignment.Attempt > store.assignments[latestIndex].Attempt) {
			latestIndex = index
		}
	}
	if latestIndex < 0 {
		return nil, ErrNotFound
	}
	latest := &store.assignments[latestIndex]
	if latest.Attempt == attempt && latest.Status == AssignmentRunning &&
		latest.AgentRunID == agentRunID {
		copy := *latest
		copy.Categories = append([]string(nil), latest.Categories...)
		return &copy, nil
	}
	if attempt == 1 {
		if latest.Attempt != 1 || latest.Status != AssignmentQueued {
			return nil, ErrConflict
		}
		latest.Status = AssignmentRunning
		latest.AgentRunID = agentRunID
		latest.ErrorCode = ""
		latest.StartedAt = &at
		latest.CompletedAt = nil
		copy := *latest
		copy.Categories = append([]string(nil), latest.Categories...)
		return &copy, nil
	}
	if latest.Attempt != attempt-1 ||
		(latest.Status != AssignmentQueued &&
			latest.Status != AssignmentRunning &&
			latest.Status != AssignmentFailed) {
		return nil, ErrConflict
	}
	if latest.Status != AssignmentFailed {
		latest.Status = AssignmentFailed
		latest.ErrorCode = "workflow_restarted"
		latest.CompletedAt = &at
	}
	assignment := ReviewAssignment{
		ID:      fmt.Sprintf("assignment-%s-%d", reviewerID, attempt),
		RoundID: roundID, ReviewerID: reviewerID,
		Agent: latest.Agent, DefinitionHash: latest.DefinitionHash,
		Categories: append([]string(nil), latest.Categories...), Required: latest.Required,
		Status: AssignmentRunning, Attempt: attempt,
		AgentRunID: agentRunID, CreatedAt: at, StartedAt: &at,
	}
	store.assignments = append(store.assignments, assignment)
	return &assignment, nil
}

func (store *executionReviewStore) TransitionReviewRound(
	_ context.Context,
	id string,
	from, to ReviewRoundStatus,
	at time.Time,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if id != store.round.ID || store.round.Status != from || !CanTransitionReviewRound(from, to) {
		return ErrConflict
	}
	store.round.Status = to
	if to == RoundCompleted || to == RoundFailed || to == RoundCancelled {
		store.round.CompletedAt = &at
	}
	return nil
}

func (store *executionReviewStore) TransitionReviewAssignment(
	_ context.Context,
	id string,
	from, to ReviewAssignmentStatus,
	agentRunID, errorCode string,
	at time.Time,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.round.Status != RoundRunning {
		return ErrConflict
	}
	for index := range store.assignments {
		assignment := &store.assignments[index]
		if assignment.ID != id {
			continue
		}
		if assignment.Status != from || !CanTransitionReviewAssignment(from, to) {
			return ErrConflict
		}
		assignment.Status = to
		assignment.AgentRunID = agentRunID
		assignment.ErrorCode = errorCode
		if to == AssignmentRunning {
			assignment.StartedAt = &at
		} else {
			assignment.CompletedAt = &at
		}
		return nil
	}
	return ErrNotFound
}

func (store *executionReviewStore) RequestReviewRoundCancel(
	_ context.Context,
	id string,
	at time.Time,
) (bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if id != store.round.ID {
		return false, ErrNotFound
	}
	switch store.round.Status {
	case RoundCancelled:
		return false, nil
	case RoundCreated, RoundRunning, RoundEvaluating:
	default:
		return false, ErrConflict
	}
	store.round.Status = RoundCancelled
	store.round.CompletedAt = &at
	for index := range store.assignments {
		assignment := &store.assignments[index]
		if assignment.Status != AssignmentQueued && assignment.Status != AssignmentRunning {
			continue
		}
		assignment.Status = AssignmentCancelled
		assignment.ErrorCode = "review_cancelled"
		assignment.CompletedAt = &at
	}
	return true, nil
}

func (store *executionReviewStore) AppendReviewEvent(
	_ context.Context,
	event ReviewEvent,
) (*ReviewEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if event.RoundID != store.round.ID {
		return nil, ErrNotFound
	}
	if store.eventError != nil {
		return nil, store.eventError
	}
	if !CanAppendReviewEvent(event.Kind, store.round.Status) {
		return nil, ErrConflict
	}
	event.Seq = int64(len(store.events) + 1)
	event.Detail = append([]byte(nil), event.Detail...)
	store.events = append(store.events, event)
	persisted := event
	return &persisted, nil
}

func (store *executionReviewStore) ListReviewEvents(
	_ context.Context,
	roundID string,
	afterSeq int64,
	limit int,
) ([]ReviewEvent, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if roundID != store.round.ID {
		return nil, ErrNotFound
	}
	events := make([]ReviewEvent, 0, limit)
	for _, event := range store.events {
		if event.Seq <= afterSeq {
			continue
		}
		event.Detail = append([]byte(nil), event.Detail...)
		events = append(events, event)
		if len(events) == limit {
			break
		}
	}
	return events, nil
}

func (store *executionReviewStore) CompleteReviewAssignment(
	_ context.Context,
	report ReviewReport,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.reportError != nil {
		return store.reportError
	}
	if store.round.Status != RoundRunning || report.RoundID != store.round.ID {
		return ErrConflict
	}
	for index := range store.assignments {
		assignment := &store.assignments[index]
		if assignment.ID != report.AssignmentID {
			continue
		}
		if assignment.Status != AssignmentRunning ||
			assignment.ReviewerID != report.ReviewerID {
			return ErrConflict
		}
		assignment.Status = AssignmentSucceeded
		assignment.CompletedAt = &report.CompletedAt
		store.reports = append(store.reports, report)
		return nil
	}
	return ErrNotFound
}

func (store *executionReviewStore) GetSuccessfulReviewReport(
	_ context.Context,
	roundID, reviewerID string,
) (*ReviewReport, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	bestAttempt := 0
	var best *ReviewReport
	for index := range store.assignments {
		assignment := store.assignments[index]
		if assignment.RoundID != roundID || assignment.ReviewerID != reviewerID ||
			!successfulReviewAssignment(assignment.Status) || assignment.Attempt < bestAttempt {
			continue
		}
		for reportIndex := range store.reports {
			report := &store.reports[reportIndex]
			if report.AssignmentID != assignment.ID {
				continue
			}
			bestAttempt = assignment.Attempt
			best = report
			break
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	copy := *best
	copy.Coverage = append([]CoverageItem(nil), best.Coverage...)
	copy.Findings = append([]Finding(nil), best.Findings...)
	copy.Uncertainties = append([]Uncertainty(nil), best.Uncertainties...)
	return &copy, nil
}

func (store *executionReviewStore) SaveReviewAdjudications(
	_ context.Context,
	adjudications []ReviewAdjudication,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.adjudicationError != nil {
		return store.adjudicationError
	}
	if store.round.Status != RoundEvaluating {
		return ErrConflict
	}
	store.adjudications = append(
		store.adjudications,
		adjudications...,
	)
	return nil
}

func (store *executionReviewStore) LoadFullReviewEvaluation(
	_ context.Context,
	roundID string,
) (ReviewEvaluation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if roundID != store.round.ID || store.round.Status != RoundEvaluating {
		return ReviewEvaluation{}, ErrConflict
	}
	store.evaluationLoads++
	return ReviewEvaluation{
		Round:         store.round,
		Policy:        store.policy,
		Assignments:   append([]ReviewAssignment(nil), store.assignments...),
		Reports:       append([]ReviewReport(nil), store.reports...),
		Adjudications: append([]ReviewAdjudication(nil), store.adjudications...),
	}, nil
}

func (store *executionReviewStore) CompleteReviewRound(
	_ context.Context,
	result ReviewGateResult,
	at time.Time,
) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.round.Status != RoundEvaluating ||
		result.RoundID != store.round.ID ||
		result.SubjectHash != store.round.Subject.ContentHash ||
		result.PolicyHash != store.round.PolicyHash {
		return ErrConflict
	}
	store.gate = result
	store.round.Status = RoundCompleted
	store.round.CompletedAt = &at
	store.gateCompletions++
	return nil
}

func (store *executionReviewStore) GetReviewGateResultByRound(
	_ context.Context,
	roundID string,
) (*ReviewGateResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if roundID != store.round.ID || store.gate.ID == "" {
		return nil, ErrNotFound
	}
	result := store.gate
	return &result, nil
}

func (store *reviewServiceStore) GetFeature(_ context.Context, id string) (*FeatureRequest, error) {
	if id != store.feature.ID {
		return nil, ErrNotFound
	}
	feature := store.feature
	return &feature, nil
}

func (store *reviewServiceStore) GetArtifact(_ context.Context, id string) (*Artifact, error) {
	if artifact, ok := store.artifacts[id]; ok {
		copy := artifact
		return &copy, nil
	}
	if id != store.artifact.ID {
		return nil, ErrNotFound
	}
	artifact := store.artifact
	return &artifact, nil
}

func (store *reviewServiceStore) GetImplementation(
	_ context.Context,
	id string,
) (*ImplementationRun, error) {
	run, ok := store.implementations[id]
	if !ok {
		return nil, ErrNotFound
	}
	copy := run
	return &copy, nil
}

func (store *reviewServiceStore) GetReviewPolicy(_ context.Context, id string, version int64) (*ReviewPolicy, error) {
	if id != store.policy.ID || version != store.policy.Version {
		return nil, ErrNotFound
	}
	policy := store.policy
	return &policy, nil
}

func (store *reviewServiceStore) SaveReviewPolicies(_ context.Context, policies []ReviewPolicy) error {
	if len(policies) == 0 {
		return nil
	}
	store.policy = policies[len(policies)-1]
	store.savedPolicy = store.policy
	return nil
}

func (store *reviewServiceStore) CreateReviewRound(
	_ context.Context,
	round ReviewRound,
	assignments []ReviewAssignment,
) error {
	store.round = round
	store.assignments = append([]ReviewAssignment(nil), assignments...)
	return nil
}

func (store *reviewServiceStore) GetReviewReportReuseSources(
	_ context.Context,
	reportIDs []string,
) ([]ReviewReportReuseSource, error) {
	requested := make(map[string]struct{}, len(reportIDs))
	for _, reportID := range reportIDs {
		requested[reportID] = struct{}{}
	}
	sources := make([]ReviewReportReuseSource, 0, len(reportIDs))
	for _, source := range store.reuseSources {
		if _, ok := requested[source.Report.ID]; ok {
			sources = append(sources, source)
		}
	}
	return sources, nil
}

func (store *reviewServiceStore) CreateReviewRoundWithReuses(
	_ context.Context,
	round ReviewRound,
	assignments []ReviewAssignment,
	reports []ReviewReport,
	reuses []ReviewReportReuse,
) error {
	store.round = round
	store.assignments = append([]ReviewAssignment(nil), assignments...)
	store.reusedReports = append([]ReviewReport(nil), reports...)
	store.reportReuses = append([]ReviewReportReuse(nil), reuses...)
	return nil
}

func (store *reviewServiceStore) GetReviewRound(_ context.Context, id string) (*ReviewRound, error) {
	if id != store.round.ID {
		return nil, ErrNotFound
	}
	round := store.round
	return &round, nil
}

func (store *reviewServiceStore) GetLatestCompletedReviewRoundBySubjectHash(
	_ context.Context,
	subjectHash string,
) (*ReviewRound, error) {
	if store.replacementRound.ID == "" ||
		subjectHash != store.replacementRound.Subject.ContentHash {
		return nil, ErrNotFound
	}
	round := store.replacementRound
	return &round, nil
}

func (store *reviewServiceStore) GetReviewFinding(_ context.Context, id string) (*FindingDetail, error) {
	if id != store.finding.ID {
		return nil, ErrNotFound
	}
	finding := store.finding
	return &finding, nil
}

func (store *reviewServiceStore) GetReviewReportByAssignment(
	_ context.Context,
	roundID, assignmentID string,
) (*ReviewReport, error) {
	if roundID != store.report.RoundID || assignmentID != store.report.AssignmentID {
		return nil, ErrNotFound
	}
	report := store.report
	return &report, nil
}

func (store *reviewServiceStore) ListReviewEvents(
	_ context.Context,
	roundID string,
	afterSeq int64,
	limit int,
) ([]ReviewEvent, error) {
	if roundID != store.round.ID {
		return nil, ErrNotFound
	}
	store.eventAfter = afterSeq
	store.eventLimit = limit
	return append([]ReviewEvent(nil), store.events...), nil
}

func (store *reviewServiceStore) ListReviewAdjudications(
	_ context.Context,
	roundID string,
	cursor ReviewAdjudicationCursor,
	limit int,
) ([]ReviewAdjudication, error) {
	if roundID != store.round.ID {
		return nil, ErrNotFound
	}
	store.adjudicationCursor = cursor
	store.adjudicationLimit = limit
	return append([]ReviewAdjudication(nil), store.adjudications...), nil
}

func (store *reviewServiceStore) GetReviewGateResult(
	_ context.Context,
	id string,
) (*ReviewGateResult, error) {
	if id != store.gate.ID {
		return nil, ErrNotFound
	}
	gate := store.gate
	return &gate, nil
}

func (store *reviewServiceStore) GetReviewGateResultByRound(
	_ context.Context,
	roundID string,
) (*ReviewGateResult, error) {
	if roundID != store.gate.RoundID {
		return nil, ErrNotFound
	}
	gate := store.gate
	return &gate, nil
}

func (store *reviewServiceStore) CreateFindingResolution(_ context.Context, resolution FindingResolution) error {
	store.resolution = resolution
	return nil
}

func (store *reviewServiceStore) ListFindingResolutions(
	_ context.Context,
	findingID, subjectHash string,
	cursor FindingResolutionCursor,
	limit int,
) ([]FindingResolution, error) {
	if findingID != store.finding.ID {
		return nil, ErrNotFound
	}
	store.resolutionSubject = subjectHash
	store.resolutionCursor = cursor
	store.resolutionLimit = limit
	return append([]FindingResolution(nil), store.resolutions...), nil
}

func (store *reviewServiceStore) ListFindingResolutionsByIDs(
	_ context.Context,
	findingIDs []string,
	subjectHash string,
) ([]FindingResolution, error) {
	store.resolutionIDs = append([]string(nil), findingIDs...)
	store.resolutionSubject = subjectHash
	return append([]FindingResolution(nil), store.resolutions...), nil
}

func TestCreateSubjectReviewRoundUsesPublishedPolicy(t *testing.T) {
	policy := testReviewPolicy(t)
	store := &reviewServiceStore{
		feature: FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: Artifact{
			ID: "artifact-1", RequestID: "feat-1", Kind: KindSystemDesign,
			Version: 2, ContentHash: "artifact-hash",
		},
		policy: policy,
	}
	service := NewService(store, nil, time.Second)
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	round, assignments, err := service.CreateSubjectReviewRound(
		context.Background(), SubjectSystemDesign, "artifact-1",
		ReviewPolicyRef{ID: policy.ID, Version: policy.Version}, 7, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if round.PolicyHash != policy.ContentHash || round.Subject.ContentHash == "" ||
		round.Status != RoundCreated || round.CreatedBy != 7 {
		t.Fatalf("round = %+v", round)
	}
	if len(assignments) != len(policy.Reviewers) || len(store.assignments) != len(policy.Reviewers) {
		t.Fatalf("assignments = %d, stored = %d", len(assignments), len(store.assignments))
	}
	for index, assignment := range assignments {
		if assignment.Agent != policy.Reviewers[index].Agent ||
			assignment.DefinitionHash != policy.Reviewers[index].DefinitionHash {
			t.Fatalf("assignment %d does not pin policy reviewer: %+v", index, assignment)
		}
	}
}

func TestCreateSubjectReviewRoundExplicitlyReusesMatchingReport(t *testing.T) {
	store, source := reviewReuseFixture(t)
	service := NewService(store, nil, time.Second)
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	round, assignments, err := service.CreateSubjectReviewRoundWithReuses(
		context.Background(),
		SubjectSystemDesign,
		store.artifact.ID,
		ReviewPolicyRef{ID: store.policy.ID, Version: store.policy.Version},
		[]ReviewReportReuseRequest{{
			ReviewerID: source.Report.ReviewerID, SourceReportID: source.Report.ID,
			ReportHash: source.Report.ReportHash,
			Reason:     "The immutable subject and reviewer definition are unchanged.",
		}},
		7,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	var reused, queued ReviewAssignment
	for _, assignment := range assignments {
		switch assignment.ReviewerID {
		case source.Report.ReviewerID:
			reused = assignment
		default:
			queued = assignment
		}
	}
	if reused.Status != AssignmentReused || reused.CompletedAt == nil ||
		queued.Status != AssignmentQueued {
		t.Fatalf("assignments = %+v", assignments)
	}
	if len(store.reusedReports) != 1 || len(store.reportReuses) != 1 {
		t.Fatalf(
			"reports = %+v, reuses = %+v",
			store.reusedReports,
			store.reportReuses,
		)
	}
	target := store.reusedReports[0]
	audit := store.reportReuses[0]
	if target.ID == source.Report.ID ||
		target.RoundID != round.ID ||
		target.AssignmentID != reused.ID ||
		target.ContentHash == source.Report.ContentHash ||
		target.ReportHash != source.Report.ReportHash ||
		target.Reuse == nil ||
		target.Reuse.SourceReportID != source.Report.ID ||
		audit.ReportID != target.ID ||
		audit.SourceReportID != source.Report.ID ||
		audit.SubjectHash != round.Subject.ContentHash ||
		audit.PolicyHash != round.PolicyHash ||
		audit.DefinitionHash != reused.DefinitionHash ||
		audit.ReportHash != target.ReportHash {
		t.Fatalf("target = %+v, audit = %+v", target, audit)
	}
}

func TestCreateSubjectReviewRoundRejectsReuseSnapshotMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ReviewReportReuseSource, *ReviewReportReuseRequest)
	}{
		{
			name: "subject",
			mutate: func(source *ReviewReportReuseSource, _ *ReviewReportReuseRequest) {
				source.Report.SubjectHash = strings.Repeat("a", 64)
			},
		},
		{
			name: "policy",
			mutate: func(source *ReviewReportReuseSource, _ *ReviewReportReuseRequest) {
				source.PolicyHash = strings.Repeat("b", 64)
			},
		},
		{
			name: "reviewer definition",
			mutate: func(source *ReviewReportReuseSource, _ *ReviewReportReuseRequest) {
				source.Assignment.DefinitionHash = strings.Repeat("c", 64)
			},
		},
		{
			name: "report hash",
			mutate: func(_ *ReviewReportReuseSource, request *ReviewReportReuseRequest) {
				request.ReportHash = strings.Repeat("d", 64)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, source := reviewReuseFixture(t)
			request := ReviewReportReuseRequest{
				ReviewerID:     source.Report.ReviewerID,
				SourceReportID: source.Report.ID,
				ReportHash:     source.Report.ReportHash,
				Reason:         "Reuse the unchanged review.",
			}
			test.mutate(&store.reuseSources[0], &request)
			_, _, err := NewService(store, nil, time.Second).
				CreateSubjectReviewRoundWithReuses(
					context.Background(),
					SubjectSystemDesign,
					store.artifact.ID,
					ReviewPolicyRef{ID: store.policy.ID, Version: store.policy.Version},
					[]ReviewReportReuseRequest{request},
					7,
					false,
				)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v, want conflict", err)
			}
			if store.round.ID != "" || len(store.reusedReports) != 0 {
				t.Fatalf("mismatched reuse was persisted: %+v", store)
			}
		})
	}
}

func reviewReuseFixture(t *testing.T) (*reviewServiceStore, ReviewReportReuseSource) {
	t.Helper()
	policy := testReviewPolicy(t)
	artifact := Artifact{
		ID: "artifact-1", RequestID: "feat-1", Kind: KindSystemDesign,
		Version: 2, ContentHash: "artifact-hash",
	}
	subject, err := BuildArtifactReviewSubject(artifact)
	if err != nil {
		t.Fatal(err)
	}
	reviewer := policy.Reviewers[0]
	sourceAssignment := ReviewAssignment{
		ID: "source-assignment", RoundID: "source-round", ReviewerID: reviewer.ID,
		Agent: reviewer.Agent, DefinitionHash: reviewer.DefinitionHash,
		Categories: append([]string(nil), reviewer.Categories...),
		Required:   reviewer.Required, Status: AssignmentRunning, Attempt: 1,
	}
	sourceReport, err := PrepareReviewReport(ReviewReport{
		RoundID: sourceAssignment.RoundID, AssignmentID: sourceAssignment.ID,
		ReviewerID: sourceAssignment.ReviewerID, SubjectHash: subject.ContentHash,
		Coverage: []CoverageItem{{
			Category: reviewer.Categories[0], Covered: true,
		}},
		Summary:     "The subject satisfies the review category.",
		CompletedAt: time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
	}, sourceAssignment, subject.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	sourceAssignment.Status = AssignmentSucceeded
	sourceAssignment.CompletedAt = &sourceReport.CompletedAt
	source := ReviewReportReuseSource{
		Report: sourceReport, Assignment: sourceAssignment,
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
	}
	return &reviewServiceStore{
		feature:  FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: artifact, policy: policy,
		reuseSources: []ReviewReportReuseSource{source},
	}, source
}

func TestCreateSubjectReviewRoundUsesConfiguredDefaultPolicy(t *testing.T) {
	policy := testReviewPolicy(t)
	store := &reviewServiceStore{
		feature: FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: Artifact{
			ID: "artifact-1", RequestID: "feat-1", Kind: KindSystemDesign,
			Version: 2, ContentHash: "artifact-hash",
		},
		policy: policy,
	}
	service := NewService(store, nil, time.Second)
	service.SetReviewConfiguration(nil, map[SubjectKind]ReviewPolicyRef{
		SubjectSystemDesign: {ID: policy.ID, Version: policy.Version},
	})

	round, _, err := service.CreateSubjectReviewRound(
		context.Background(), SubjectSystemDesign, "artifact-1",
		ReviewPolicyRef{}, 7, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if round.PolicyID != policy.ID || round.PolicyVersion != policy.Version {
		t.Fatalf("round policy = %s@%d", round.PolicyID, round.PolicyVersion)
	}
}

func TestCreateSubjectReviewRoundExplicitPolicyOverridesDefault(t *testing.T) {
	policy := testReviewPolicy(t)
	store := &reviewServiceStore{
		feature: FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: Artifact{
			ID: "artifact-1", RequestID: "feat-1", Kind: KindSystemDesign,
			Version: 2, ContentHash: "artifact-hash",
		},
		policy: policy,
	}
	service := NewService(store, nil, time.Second)
	service.SetReviewConfiguration(nil, map[SubjectKind]ReviewPolicyRef{
		SubjectSystemDesign: {ID: "unavailable-default", Version: 99},
	})

	round, _, err := service.CreateSubjectReviewRound(
		context.Background(), SubjectSystemDesign, "artifact-1",
		ReviewPolicyRef{ID: policy.ID, Version: policy.Version}, 7, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if round.PolicyID != policy.ID || round.PolicyVersion != policy.Version {
		t.Fatalf("round policy = %s@%d", round.PolicyID, round.PolicyVersion)
	}
}

func TestCreateSubjectReviewRoundFixesCandidateRolloutSelection(t *testing.T) {
	store, defaultPolicy, candidatePolicy := reviewRolloutServiceFixture(t)
	store.rollout = preparedReviewPolicyRolloutRule(t, ReviewPolicyRolloutRule{
		SubjectKind: SubjectSystemDesign, RuleVersion: 4,
		CandidatePolicyID: candidatePolicy.ID, CandidatePolicyVersion: candidatePolicy.Version,
		PercentageBPS: reviewPolicyRolloutBucketCount, Salt: "candidate-all", Active: true,
	})
	store.rolloutFound = true

	round, _, err := NewService(store, nil, time.Second).CreateSubjectReviewRound(
		context.Background(), SubjectSystemDesign, store.artifact.ID,
		ReviewPolicyRef{}, 42, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	selection := round.PolicySelection
	if round.PolicyID != candidatePolicy.ID ||
		round.PolicyVersion != candidatePolicy.Version ||
		selection.RuleVersion != store.rollout.RuleVersion ||
		selection.RuleHash != store.rollout.RuleHash ||
		selection.CandidatePolicyID != candidatePolicy.ID ||
		selection.CandidatePolicyVersion != candidatePolicy.Version ||
		selection.PercentageBasisPoints != reviewPolicyRolloutBucketCount ||
		selection.Reason != "rollout_candidate" ||
		selection.StableKeyHash == "" ||
		strings.Contains(selection.StableKeyHash, "42") ||
		strings.Contains(selection.StableKeyHash, store.artifact.ID) {
		t.Fatalf("round = %+v, default policy = %+v", round, defaultPolicy)
	}
	if store.round.PolicySelection != selection {
		t.Fatalf("persisted selection = %+v, want %+v", store.round.PolicySelection, selection)
	}
}

func TestCreateSubjectReviewRoundRolloutFallsBackToDefault(t *testing.T) {
	for _, test := range []struct {
		name       string
		percentage int
		active     bool
		wantReason string
	}{
		{name: "zero percent", active: true, wantReason: "rollout_default"},
		{name: "inactive", percentage: reviewPolicyRolloutBucketCount, wantReason: "default"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, defaultPolicy, candidatePolicy := reviewRolloutServiceFixture(t)
			store.rollout = preparedReviewPolicyRolloutRule(t, ReviewPolicyRolloutRule{
				SubjectKind: SubjectSystemDesign, RuleVersion: 2,
				CandidatePolicyID:      candidatePolicy.ID,
				CandidatePolicyVersion: candidatePolicy.Version,
				PercentageBPS:          test.percentage, Salt: test.name, Active: test.active,
			})
			store.rolloutFound = true

			round, _, err := NewService(store, nil, time.Second).CreateSubjectReviewRound(
				context.Background(), SubjectSystemDesign, store.artifact.ID,
				ReviewPolicyRef{}, 42, false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if round.PolicyID != defaultPolicy.ID ||
				round.PolicyVersion != defaultPolicy.Version ||
				round.PolicySelection.Reason != test.wantReason {
				t.Fatalf("round = %+v", round)
			}
		})
	}
}

func TestCreateSubjectReviewRoundExplicitPolicyBypassesRollout(t *testing.T) {
	store, defaultPolicy, candidatePolicy := reviewRolloutServiceFixture(t)
	store.rollout = preparedReviewPolicyRolloutRule(t, ReviewPolicyRolloutRule{
		SubjectKind: SubjectSystemDesign, RuleVersion: 2,
		CandidatePolicyID: candidatePolicy.ID, CandidatePolicyVersion: candidatePolicy.Version,
		PercentageBPS: reviewPolicyRolloutBucketCount, Salt: "candidate-all", Active: true,
	})
	store.rolloutFound = true

	round, _, err := NewService(store, nil, time.Second).CreateSubjectReviewRound(
		context.Background(), SubjectSystemDesign, store.artifact.ID,
		ReviewPolicyRef{ID: defaultPolicy.ID, Version: defaultPolicy.Version},
		42, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if round.PolicyID != defaultPolicy.ID ||
		round.PolicyVersion != defaultPolicy.Version ||
		round.PolicySelection != (ReviewPolicySelection{Reason: "explicit_version"}) {
		t.Fatalf("round = %+v", round)
	}
}

func TestCreateSubjectReviewRoundRejectsUnavailableRolloutCandidate(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ReviewPolicyRecord)
	}{
		{name: "inactive", mutate: func(record *ReviewPolicyRecord) { record.Active = false }},
		{name: "subject kind", mutate: func(record *ReviewPolicyRecord) {
			record.SubjectKind = SubjectChangeSet
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, candidatePolicy := reviewRolloutServiceFixture(t)
			candidateRef := ReviewPolicyRef{ID: candidatePolicy.ID, Version: candidatePolicy.Version}
			record := store.policies[candidateRef]
			test.mutate(&record)
			store.policies[candidateRef] = record
			store.rollout = preparedReviewPolicyRolloutRule(t, ReviewPolicyRolloutRule{
				SubjectKind: SubjectSystemDesign, RuleVersion: 2,
				CandidatePolicyID:      candidatePolicy.ID,
				CandidatePolicyVersion: candidatePolicy.Version,
				PercentageBPS:          reviewPolicyRolloutBucketCount,
				Salt:                   "unavailable-candidate", Active: true,
			})
			store.rolloutFound = true

			_, _, err := NewService(store, nil, time.Second).CreateSubjectReviewRound(
				context.Background(), SubjectSystemDesign, store.artifact.ID,
				ReviewPolicyRef{}, 42, false,
			)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v, want ErrConflict", err)
			}
		})
	}
}

func reviewRolloutServiceFixture(
	t *testing.T,
) (*reviewRolloutServiceStore, ReviewPolicy, ReviewPolicy) {
	t.Helper()
	defaultPolicy := testReviewPolicy(t)
	defaultPolicy.ID = "default-system-design-review"
	defaultPolicy.ContentHash = ""
	var err error
	defaultPolicy, err = PrepareReviewPolicy(defaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	candidatePolicy := defaultPolicy
	candidatePolicy.ID = "candidate-system-design-review"
	candidatePolicy.Version = 2
	candidatePolicy.ContentHash = ""
	candidatePolicy, err = PrepareReviewPolicy(candidatePolicy)
	if err != nil {
		t.Fatal(err)
	}
	defaultRef := ReviewPolicyRef{ID: defaultPolicy.ID, Version: defaultPolicy.Version}
	candidateRef := ReviewPolicyRef{ID: candidatePolicy.ID, Version: candidatePolicy.Version}
	base := &reviewServiceStore{
		feature: FeatureRequest{ID: "feat-1", CreatedBy: 42},
		artifact: Artifact{
			ID: "artifact-1", RequestID: "feat-1", Kind: KindSystemDesign,
			Version: 2, ContentHash: "artifact-hash",
		},
	}
	return &reviewRolloutServiceStore{
		reviewServiceStore: base,
		policies: map[ReviewPolicyRef]ReviewPolicyRecord{
			defaultRef:   {ReviewPolicy: defaultPolicy, Active: true, Default: true},
			candidateRef: {ReviewPolicy: candidatePolicy, Active: true},
		},
		defaults: map[SubjectKind]ReviewPolicyRef{SubjectSystemDesign: defaultRef},
	}, defaultPolicy, candidatePolicy
}

func preparedReviewPolicyRolloutRule(
	t *testing.T,
	rule ReviewPolicyRolloutRule,
) ReviewPolicyRolloutRule {
	t.Helper()
	prepared, err := prepareReviewPolicyRolloutRule(rule)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestCreateImplementationReviewRoundsEnforceRunStatus(t *testing.T) {
	for _, test := range []struct {
		name    string
		kind    SubjectKind
		status  RunStatus
		wantErr error
	}{
		{name: "change succeeded", kind: SubjectChangeSet, status: RunSucceeded},
		{name: "change failed", kind: SubjectChangeSet, status: RunFailed},
		{name: "validation succeeded", kind: SubjectValidationBundle, status: RunSucceeded},
		{name: "validation failed", kind: SubjectValidationBundle, status: RunFailed},
		{name: "delivery succeeded", kind: SubjectDeliveryBundle, status: RunSucceeded},
		{name: "delivery failed", kind: SubjectDeliveryBundle, status: RunFailed, wantErr: ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, design, plan := implementationReviewFixture()
			run.Status = test.status
			policy := testReviewPolicyForSubject(t, test.kind)
			store := &executionReviewStore{
				feature:        FeatureRequest{ID: run.RequestID, CreatedBy: run.RequestedBy},
				implementation: run,
				artifacts: map[string]Artifact{
					design.ID: design,
					plan.ID:   plan,
				},
				policy: policy,
			}
			service := NewService(store, nil, time.Second)

			round, assignments, err := service.CreateSubjectReviewRound(
				context.Background(),
				test.kind,
				run.ID,
				ReviewPolicyRef{ID: policy.ID, Version: policy.Version},
				run.RequestedBy,
				false,
			)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if round.Subject.Kind != test.kind ||
				len(assignments) != len(policy.Reviewers) {
				t.Fatalf("round = %+v, assignments = %+v", round, assignments)
			}
		})
	}
}

func TestImplementationReviewAuthorizationRejectsSubjectDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		kind   SubjectKind
		mutate func(*executionReviewStore, Artifact)
	}{
		{
			name: "change set",
			kind: SubjectChangeSet,
			mutate: func(store *executionReviewStore, _ Artifact) {
				store.implementation.ChangeSet.ProviderSummary = "changed provider summary"
			},
		},
		{
			name: "validation",
			kind: SubjectValidationBundle,
			mutate: func(store *executionReviewStore, _ Artifact) {
				store.implementation.ChangeSet.ValidationResults[0].OutputSummary = "changed validation"
			},
		},
		{
			name: "delivery",
			kind: SubjectDeliveryBundle,
			mutate: func(store *executionReviewStore, design Artifact) {
				design.ContentHash = "changed design hash"
				store.artifacts[design.ID] = design
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, design, plan := implementationReviewFixture()
			store := &executionReviewStore{
				feature:        FeatureRequest{ID: run.RequestID, CreatedBy: run.RequestedBy},
				implementation: run,
				artifacts: map[string]Artifact{
					design.ID: design,
					plan.ID:   plan,
				},
			}
			service := NewService(store, nil, time.Second)
			subject, err := service.buildImplementationReviewSubject(
				context.Background(),
				test.kind,
				run,
			)
			if err != nil {
				t.Fatal(err)
			}
			store.round = ReviewRound{ID: "round-1", Subject: subject}
			if _, err := service.GetReviewRound(
				context.Background(),
				store.round.ID,
				run.RequestedBy,
				false,
			); err != nil {
				t.Fatalf("initial authorization: %v", err)
			}

			test.mutate(store, design)
			if _, err := service.GetReviewRound(
				context.Background(),
				store.round.ID,
				run.RequestedBy,
				false,
			); !errors.Is(err, ErrConflict) {
				t.Fatalf("drift error = %v, want conflict", err)
			}
		})
	}
}

func TestValidationFactsDeterministicallyBlockReviewGate(t *testing.T) {
	for _, test := range []struct {
		name         string
		kind         SubjectKind
		status       string
		wantDecision GateDecision
		wantReason   string
		wantBlocking string
		wantGap      string
	}{
		{
			name: "failed validation revises",
			kind: SubjectValidationBundle, status: "failed",
			wantDecision: GateRevise, wantReason: reasonValidationFailed,
			wantBlocking: "validation:1",
		},
		{
			name: "validation not configured is incomplete",
			kind: SubjectDeliveryBundle, status: "validation_not_configured",
			wantDecision: GateIncomplete, wantReason: reasonValidationNotConfigured,
			wantGap: "validation_execution",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			run, design, plan := implementationReviewFixture()
			result := &run.ChangeSet.ValidationResults[0]
			result.Status = test.status
			if test.status == "validation_not_configured" {
				result.OutputRelPath = ""
				result.OutputSHA256 = ""
				result.OutputBytes = 0
			}
			policy := testReviewPolicyForSubject(t, test.kind)
			store := &executionReviewStore{
				implementation: run,
				artifacts: map[string]Artifact{
					design.ID: design,
					plan.ID:   plan,
				},
			}
			service := NewService(store, nil, time.Second)
			subject, err := service.buildImplementationReviewSubject(
				context.Background(),
				test.kind,
				run,
			)
			if err != nil {
				t.Fatal(err)
			}
			round := ReviewRound{
				ID: "round-1", Subject: subject,
				PolicyID: policy.ID, PolicyVersion: policy.Version,
				PolicyHash: policy.ContentHash, Status: RoundEvaluating,
			}
			assignments := make([]ReviewAssignment, 0, len(policy.Reviewers))
			reports := make([]ReviewReport, 0, len(policy.Reviewers))
			for index, reviewer := range policy.Reviewers {
				assignmentID := fmt.Sprintf("assignment-%d", index+1)
				assignments = append(assignments, ReviewAssignment{
					ID: assignmentID, RoundID: round.ID, ReviewerID: reviewer.ID,
					Required: reviewer.Required, Status: AssignmentSucceeded,
				})
				coverage := make([]CoverageItem, 0, len(reviewer.Categories))
				for _, category := range reviewer.Categories {
					coverage = append(coverage, CoverageItem{Category: category, Covered: true})
				}
				reports = append(reports, ReviewReport{
					ID:      fmt.Sprintf("report-%d", index+1),
					RoundID: round.ID, AssignmentID: assignmentID,
					ReviewerID: reviewer.ID, SubjectHash: subject.ContentHash,
					ContentHash: fmt.Sprintf("report-hash-%d", index+1),
					Coverage:    coverage,
				})
			}
			evaluation := ReviewEvaluation{
				Round: round, Policy: policy,
				Assignments: assignments, Reports: reports,
			}
			if err := service.attachReviewSubjectGateFacts(
				context.Background(),
				&evaluation,
			); err != nil {
				t.Fatal(err)
			}
			gate, err := EvaluateReviewGate(
				evaluation,
				time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
			)
			if err != nil {
				t.Fatal(err)
			}
			if gate.Decision != test.wantDecision ||
				!slices.Contains(gate.ReasonCodes, test.wantReason) ||
				(test.wantBlocking != "" && !slices.Contains(gate.BlockingIDs, test.wantBlocking)) ||
				(test.wantGap != "" && !slices.Contains(gate.CoverageGaps, test.wantGap)) {
				t.Fatalf("gate = %+v", gate)
			}
		})
	}
}

func TestReviewPolicyControlPlaneRequiresAdministrator(t *testing.T) {
	store := &reviewServiceStore{}
	service := NewService(store, nil, time.Second)
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	policy := testReviewPolicy(t)
	policy.ContentHash = ""
	policy.CreatedAt = time.Time{}

	if _, err := service.PublishReviewPolicy(
		context.Background(), policy, false,
	); !errors.Is(err, ErrForbidden) {
		t.Fatalf("regular publish error = %v, want forbidden", err)
	}
	published, err := service.PublishReviewPolicy(context.Background(), policy, true)
	if err != nil {
		t.Fatal(err)
	}
	if published.CreatedAt != now || published.ContentHash == "" ||
		store.savedPolicy.ContentHash != published.ContentHash {
		t.Fatalf("published = %+v, saved = %+v", published, store.savedPolicy)
	}
	if _, err := service.GetReviewPolicy(
		context.Background(), published.ID, published.Version, false,
	); !errors.Is(err, ErrForbidden) {
		t.Fatalf("regular read error = %v, want forbidden", err)
	}
	got, err := service.GetReviewPolicy(
		context.Background(), published.ID, published.Version, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentHash != published.ContentHash {
		t.Fatalf("policy hash = %q, want %q", got.ContentHash, published.ContentHash)
	}
}

func TestRuntimeReviewRunnerRejectsTrailingJSON(t *testing.T) {
	runner := NewRuntimeReviewRunner(agentRuntimeFunc(func(
		context.Context,
		agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		return agentapi.RunResult{
			Status: agentapi.RunSucceeded,
			Output: []byte(`{}{"unexpected":true}`),
		}, nil
	}))

	_, err := runner.Run(context.Background(), ReviewAssignmentRequest{
		Round: ReviewRound{
			ID:      "round-1",
			Subject: ReviewSubject{ContentHash: "subject-hash"},
		},
		Policy: ReviewPolicy{ContentHash: "policy-hash"},
		Assignment: ReviewAssignment{
			ID: "assignment-1", ReviewerID: "security",
			Agent: agentapi.DefinitionRef{ID: "review.security", Version: 1},
		},
	})
	if err == nil {
		t.Fatal("runner accepted trailing JSON content")
	}
}

func TestRuntimeReviewRunnerForwardsPinnedDefinitionAndContext(t *testing.T) {
	contextBlock := agentapi.ContextBlock{
		Source: "feature_delivery.artifact", Title: "System design",
		Content: "review material", Complete: true,
		ContentHash: strings.Repeat("c", 64),
	}
	var captured agentapi.RunRequest
	runner := NewRuntimeReviewRunner(agentRuntimeFunc(func(
		_ context.Context,
		request agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		captured = request
		return agentapi.RunResult{
			RunID: request.RunID, Status: agentapi.RunSucceeded, Output: []byte(`{}`),
		}, nil
	}))
	request := ReviewAssignmentRequest{
		Round: ReviewRound{
			ID: "round-1", WorkflowRunID: "review.round-1",
			Subject: ReviewSubject{
				Kind: SubjectSystemDesign, ID: "artifact-1", ContentHash: "subject-hash",
			},
		},
		Policy: ReviewPolicy{ContentHash: "policy-hash"},
		Assignment: ReviewAssignment{
			ID: "assignment-1", ReviewerID: "architecture",
			AgentRunID:     "reviewagent.assignment-1",
			Agent:          agentapi.DefinitionRef{ID: "review.architecture", Version: 3},
			DefinitionHash: strings.Repeat("d", 64),
			Categories:     []string{"architecture", "maintainability"},
		},
		Context: contextBlock,
		Actor:   agentapi.Actor{UserID: 7, TenantID: "tenant-1"},
	}

	report, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if captured.RunID != request.Assignment.AgentRunID ||
		captured.Agent != request.Assignment.Agent ||
		captured.DefinitionHash != request.Assignment.DefinitionHash ||
		captured.Actor != request.Actor ||
		!captured.Policy.RedactSensitive {
		t.Fatalf("captured request = %+v", captured)
	}
	if len(captured.Context) != 1 ||
		captured.Context[0].Source != contextBlock.Source ||
		captured.Context[0].Title != contextBlock.Title ||
		captured.Context[0].Content != contextBlock.Content ||
		captured.Context[0].Complete != contextBlock.Complete ||
		captured.Context[0].ContentHash != contextBlock.ContentHash {
		t.Fatalf("captured context = %+v", captured.Context)
	}
	if captured.Correlation.WorkflowRunID != request.Round.WorkflowRunID ||
		captured.Correlation.NodeID != request.Assignment.ReviewerID {
		t.Fatalf("captured correlation = %+v", captured.Correlation)
	}
	if report.RoundID != request.Round.ID ||
		report.AssignmentID != request.Assignment.ID ||
		report.ReviewerID != request.Assignment.ReviewerID ||
		report.SubjectHash != request.Round.Subject.ContentHash {
		t.Fatalf("report identity = %+v", report)
	}
	var input struct {
		Subject    ReviewSubject `json:"subject"`
		Categories []string      `json:"categories"`
		PolicyHash string        `json:"policy_hash"`
	}
	if err := json.Unmarshal(captured.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.Subject != request.Round.Subject ||
		!slices.Equal(input.Categories, request.Assignment.Categories) ||
		input.PolicyHash != request.Policy.ContentHash {
		t.Fatalf("captured input = %+v", input)
	}
}

func TestRuntimeAdjudicationRunnerRejectsNonStrictOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
	}{
		{
			name:   "unknown field",
			output: `{"decision":"confirmed","rationale":"supported","unexpected":true}`,
		},
		{
			name:   "trailing json",
			output: `{"decision":"confirmed","rationale":"supported"}{"unexpected":true}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := NewRuntimeAdjudicationRunner(agentRuntimeFunc(func(
				context.Context,
				agentapi.RunRequest,
			) (agentapi.RunResult, error) {
				return agentapi.RunResult{
					Status: agentapi.RunSucceeded,
					Output: json.RawMessage(test.output),
				}, nil
			}))
			if _, err := runner.Run(
				context.Background(),
				adjudicationRuntimeRequest(t),
			); err == nil {
				t.Fatal("adjudicator accepted non-strict output")
			}
		})
	}
}

func TestRuntimeAdjudicationRunnerForwardsPinnedSnapshotWithoutReviewerIdentity(t *testing.T) {
	request := adjudicationRuntimeRequest(t)
	request.Round.WorkflowRunID = "review.round-1"
	var captured agentapi.RunRequest
	runner := NewRuntimeAdjudicationRunner(agentRuntimeFunc(func(
		_ context.Context,
		runRequest agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		captured = runRequest
		return agentapi.RunResult{
			RunID: runRequest.RunID, Status: agentapi.RunSucceeded,
			Output: json.RawMessage(`{"decision":"confirmed","rationale":"The blocking evidence is confirmed."}`),
		}, nil
	}))

	outcome, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision != AdjudicationConfirmed ||
		outcome.Rationale != "The blocking evidence is confirmed." {
		t.Fatalf("outcome = %+v", outcome)
	}
	if captured.Agent != request.Policy.Adjudicator.Agent ||
		captured.DefinitionHash != request.Policy.Adjudicator.DefinitionHash ||
		captured.Actor != request.Actor ||
		!captured.Policy.RedactSensitive {
		t.Fatalf("captured request = %+v", captured)
	}
	if captured.Correlation.WorkflowRunID != request.Round.WorkflowRunID ||
		captured.Correlation.NodeID != "adjudicator" ||
		len(captured.Context) != 1 ||
		captured.Context[0].ContentHash != request.Context.ContentHash {
		t.Fatalf("captured context or correlation = %+v / %+v", captured.Context, captured.Correlation)
	}

	var input struct {
		Subject     ReviewSubject                `json:"subject"`
		PolicyHash  string                       `json:"policy_hash"`
		Fingerprint string                       `json:"fingerprint"`
		Findings    []map[string]json.RawMessage `json:"findings"`
	}
	if err := json.Unmarshal(captured.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.Subject != request.Round.Subject ||
		input.PolicyHash != request.Policy.ContentHash ||
		input.Fingerprint != request.Findings[0].Fingerprint ||
		len(input.Findings) != len(request.Findings) {
		t.Fatalf("captured input = %+v", input)
	}
	for _, finding := range input.Findings {
		for _, forbidden := range []string{
			"reviewer_id", "assignment_id", "report_id", "votes", "majority",
		} {
			if _, exists := finding[forbidden]; exists {
				t.Fatalf("adjudicator input exposed %q: %s", forbidden, captured.Input)
			}
		}
	}
}

func TestRuntimeAdjudicationRunnerRejectsMixedFingerprints(t *testing.T) {
	request := adjudicationRuntimeRequest(t)
	request.Findings[1].Fingerprint = "different-fingerprint"
	called := false
	runner := NewRuntimeAdjudicationRunner(agentRuntimeFunc(func(
		context.Context,
		agentapi.RunRequest,
	) (agentapi.RunResult, error) {
		called = true
		return agentapi.RunResult{}, nil
	}))

	if _, err := runner.Run(context.Background(), request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v, want invalid", err)
	}
	if called {
		t.Fatal("runtime called for a mixed conflict group")
	}
}

func TestReviewRoundAuthorizationUsesFeatureOwnership(t *testing.T) {
	store := &reviewServiceStore{
		feature:  FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: Artifact{ID: "artifact-1", RequestID: "feat-1", ContentHash: "artifact-hash"},
		round: ReviewRound{
			ID: "round-1",
			Subject: ReviewSubject{
				Kind: SubjectSystemDesign, ID: "artifact-1",
				SourceContentHash: "artifact-hash",
			},
		},
	}
	service := NewService(store, nil, time.Second)

	if _, err := service.GetReviewRound(context.Background(), "round-1", 7, false); err != nil {
		t.Fatalf("owner read: %v", err)
	}
	if _, err := service.GetReviewRound(context.Background(), "round-1", 8, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user error = %v, want not found", err)
	}
	if _, err := service.GetReviewRound(context.Background(), "round-1", 8, true); err != nil {
		t.Fatalf("administrator read: %v", err)
	}
}

func TestListReviewAdjudicationsUsesRoundAuthorization(t *testing.T) {
	cursor := ReviewAdjudicationCursor{
		Fingerprint: "fingerprint-0",
		ID:          "adjudication-0",
	}
	store := &reviewServiceStore{
		feature:  FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: Artifact{ID: "artifact-1", RequestID: "feat-1", ContentHash: "artifact-hash"},
		round: ReviewRound{
			ID: "round-1",
			Subject: ReviewSubject{
				Kind: SubjectSystemDesign, ID: "artifact-1",
				SourceContentHash: "artifact-hash",
			},
		},
		adjudications: []ReviewAdjudication{{
			ID: "adjudication-1", RoundID: "round-1", Fingerprint: "fingerprint-1",
		}},
	}
	service := NewService(store, nil, time.Second)

	items, err := service.ListReviewAdjudications(
		context.Background(), "round-1", cursor, 25, 7, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "adjudication-1" ||
		store.adjudicationCursor != cursor || store.adjudicationLimit != 25 {
		t.Fatalf(
			"adjudications = %+v, cursor = %+v, limit = %d",
			items,
			store.adjudicationCursor,
			store.adjudicationLimit,
		)
	}
	if _, err := service.ListReviewAdjudications(
		context.Background(), "round-1", ReviewAdjudicationCursor{}, 25, 8, false,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user error = %v, want not found", err)
	}
	if _, err := service.ListReviewAdjudications(
		context.Background(), "round-1", ReviewAdjudicationCursor{}, 25, 8, true,
	); err != nil {
		t.Fatalf("administrator read: %v", err)
	}
}

func TestGetReviewReportUsesRoundAuthorization(t *testing.T) {
	store := &reviewServiceStore{
		feature: FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: Artifact{
			ID: "artifact-1", RequestID: "feat-1", ContentHash: "artifact-hash",
		},
		round: ReviewRound{
			ID: "round-1",
			Subject: ReviewSubject{
				Kind: SubjectSystemDesign, ID: "artifact-1",
				SourceContentHash: "artifact-hash",
			},
		},
		report: ReviewReport{
			ID: "report-1", RoundID: "round-1", AssignmentID: "assignment-1",
			ReviewerID: "architecture", SubjectHash: "subject-hash",
			ContentHash: "report-hash",
		},
	}
	service := NewService(store, nil, time.Second)

	report, err := service.GetReviewReport(
		context.Background(), "round-1", "assignment-1", 7, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.ID != "report-1" || report.AssignmentID != "assignment-1" {
		t.Fatalf("report = %+v", report)
	}
	if _, err := service.GetReviewReport(
		context.Background(), "round-1", "assignment-1", 8, false,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user error = %v, want not found", err)
	}
	if _, err := service.GetReviewReport(
		context.Background(), "round-1", "assignment-2", 7, false,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other assignment error = %v, want not found", err)
	}
	if _, err := service.GetReviewReport(
		context.Background(), "round-1", "assignment-1", 8, true,
	); err != nil {
		t.Fatalf("administrator read: %v", err)
	}
}

func adjudicationRuntimeRequest(t *testing.T) ReviewAdjudicationRequest {
	t.Helper()
	policy := testReviewPolicy(t)
	return ReviewAdjudicationRequest{
		Round: ReviewRound{
			ID: "round-1",
			Subject: ReviewSubject{
				Kind: SubjectSystemDesign, ID: "artifact-1", Version: 1,
				SourceContentHash: "artifact-hash", ContentHash: "subject-hash",
			},
			PolicyID: policy.ID, PolicyVersion: policy.Version,
			PolicyHash: policy.ContentHash, Status: RoundEvaluating,
		},
		Policy: policy,
		Findings: []Finding{
			{
				ID: "finding-high", ReportID: "report-architecture",
				Category: "architecture", Severity: SeverityHigh,
				Claim: "The contract is inconsistent.", Impact: "Requests can fail.",
				Evidence: []FindingEvidence{{
					Kind: "subject", Ref: "interfaces[0]", Hash: "evidence-hash",
					Summary: "The interface evidence is immutable.",
				}},
				Recommendation: "Align the contract.", Confidence: 0.95,
				Fingerprint: "same-fingerprint", ContentHash: "finding-high-hash",
			},
			{
				ID: "finding-medium", ReportID: "report-security",
				Category: "architecture", Severity: SeverityMedium,
				Claim: "The contract is inconsistent.", Impact: "Requests can fail.",
				Evidence: []FindingEvidence{{
					Kind: "subject", Ref: "interfaces[0]", Hash: "evidence-hash",
					Summary: "The interface evidence is immutable.",
				}},
				Recommendation: "Align the contract.", Confidence: 0.8,
				Fingerprint: "same-fingerprint", ContentHash: "finding-medium-hash",
			},
		},
		Context: agentapi.ContextBlock{
			Source: "feature_delivery.artifact", Title: "System design",
			Content: "review material", Complete: true,
			ContentHash: strings.Repeat("c", 64),
		},
		Actor: agentapi.Actor{UserID: 7, TenantID: "tenant-1"},
	}
}

func TestListReviewEventsUsesRoundAuthorization(t *testing.T) {
	store := &reviewServiceStore{
		feature:  FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: Artifact{ID: "artifact-1", RequestID: "feat-1", ContentHash: "artifact-hash"},
		round: ReviewRound{
			ID: "round-1",
			Subject: ReviewSubject{
				Kind: SubjectSystemDesign, ID: "artifact-1",
				SourceContentHash: "artifact-hash",
			},
		},
		events: []ReviewEvent{{RoundID: "round-1", Seq: 8, Kind: ReviewEventRoundCompleted}},
	}
	service := NewService(store, nil, time.Second)

	events, err := service.ListReviewEvents(context.Background(), "round-1", 7, 25, 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 8 || store.eventAfter != 7 || store.eventLimit != 25 {
		t.Fatalf("events = %+v, after = %d, limit = %d", events, store.eventAfter, store.eventLimit)
	}
	if _, err := service.ListReviewEvents(
		context.Background(), "round-1", 0, 25, 8, false,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user error = %v, want not found", err)
	}
	if _, err := service.ListReviewEvents(
		context.Background(), "round-1", -1, 25, 7, false,
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("negative sequence error = %v, want invalid", err)
	}
	if _, err := service.ListReviewEvents(
		context.Background(), "round-1", 0, 25, 8, true,
	); err != nil {
		t.Fatalf("administrator read: %v", err)
	}
}

func TestCreateFindingWaiverPinsSubjectAndHumanActor(t *testing.T) {
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	store := &reviewServiceStore{
		round: ReviewRound{
			ID: "round-1",
			Subject: ReviewSubject{
				Kind: SubjectSystemDesign, ID: "artifact-1", ContentHash: "subject-hash",
			},
		},
		finding: FindingDetail{
			FindingSummary: FindingSummary{ID: "finding-1", RoundID: "round-1"},
		},
	}
	service := NewService(store, nil, time.Second)
	service.now = func() time.Time { return now }
	expiresAt := now.Add(24 * time.Hour)

	resolution, err := service.CreateFindingWaiver(
		context.Background(), "finding-1", "subject-hash", "Accepted for this release.",
		&expiresAt, 9, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Resolution != ResolutionWaived || resolution.ActorID != 9 ||
		resolution.SubjectHash != "subject-hash" || store.resolution.ID != resolution.ID {
		t.Fatalf("resolution = %+v, stored = %+v", resolution, store.resolution)
	}
	if _, err := service.CreateFindingWaiver(
		context.Background(), "finding-1", "other-hash", "Mismatch", nil, 9, true,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("subject mismatch error = %v, want not found", err)
	}
	if _, err := service.CreateFindingWaiver(
		context.Background(), "finding-1", "subject-hash", "No permission", nil, 9, false,
	); !errors.Is(err, ErrForbidden) {
		t.Fatalf("regular user error = %v, want forbidden", err)
	}
}

func TestCreateFindingResolutionSupportsCompleteLifecycle(t *testing.T) {
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		kind        FindingResolutionKind
		replacement bool
		expires     bool
	}{
		{kind: ResolutionWaived, expires: true},
		{kind: ResolutionInvalidated},
		{kind: ResolutionFixed, replacement: true},
		{kind: ResolutionSuperseded, replacement: true},
	} {
		t.Run(string(test.kind), func(t *testing.T) {
			store, source, replacement := findingResolutionServiceFixture(t)
			service := NewService(store, nil, time.Second)
			service.now = func() time.Time { return now }
			request := FindingResolutionRequest{
				Resolution:  test.kind,
				SubjectHash: source.ContentHash,
				Rationale:   "Reviewed lifecycle disposition.",
			}
			if test.replacement {
				request.ReplacementHash = replacement.ContentHash
			}
			if test.expires {
				expiresAt := now.Add(24 * time.Hour)
				request.ExpiresAt = &expiresAt
			}

			resolution, err := service.CreateFindingResolution(
				context.Background(), "finding-1", request, 9, true,
			)
			if err != nil {
				t.Fatal(err)
			}
			if resolution.Resolution != test.kind ||
				resolution.SubjectHash != source.ContentHash ||
				resolution.ReplacementHash != request.ReplacementHash ||
				resolution.ActorID != 9 ||
				resolution.ID == "" ||
				store.resolution.ID != resolution.ID {
				t.Fatalf("resolution = %+v, stored = %+v", resolution, store.resolution)
			}
		})
	}
}

func TestCreateFindingResolutionRejectsInvalidDispositionFields(t *testing.T) {
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	store, source, replacement := findingResolutionServiceFixture(t)
	service := NewService(store, nil, time.Second)
	service.now = func() time.Time { return now }
	base := FindingResolutionRequest{
		SubjectHash: source.ContentHash,
		Rationale:   "Disposition rationale.",
	}
	tests := []struct {
		name    string
		request FindingResolutionRequest
		admin   bool
		want    error
	}{
		{
			name: "regular user",
			request: FindingResolutionRequest{
				Resolution:  ResolutionInvalidated,
				SubjectHash: source.ContentHash,
				Rationale:   base.Rationale,
			},
			want: ErrForbidden,
		},
		{
			name: "waiver replacement",
			request: FindingResolutionRequest{
				Resolution:      ResolutionWaived,
				SubjectHash:     source.ContentHash,
				ReplacementHash: replacement.ContentHash,
				Rationale:       base.Rationale,
			},
			admin: true, want: ErrInvalid,
		},
		{
			name: "expired waiver",
			request: FindingResolutionRequest{
				Resolution:  ResolutionWaived,
				SubjectHash: source.ContentHash,
				Rationale:   base.Rationale, ExpiresAt: &expired,
			},
			admin: true, want: ErrInvalid,
		},
		{
			name: "invalidated expiry",
			request: FindingResolutionRequest{
				Resolution:  ResolutionInvalidated,
				SubjectHash: source.ContentHash,
				Rationale:   base.Rationale, ExpiresAt: &future,
			},
			admin: true, want: ErrInvalid,
		},
		{
			name: "fixed missing replacement",
			request: FindingResolutionRequest{
				Resolution:  ResolutionFixed,
				SubjectHash: source.ContentHash,
				Rationale:   base.Rationale,
			},
			admin: true, want: ErrInvalid,
		},
		{
			name: "same replacement",
			request: FindingResolutionRequest{
				Resolution:      ResolutionFixed,
				SubjectHash:     source.ContentHash,
				ReplacementHash: source.ContentHash,
				Rationale:       base.Rationale,
			},
			admin: true, want: ErrInvalid,
		},
		{
			name: "superseded expiry",
			request: FindingResolutionRequest{
				Resolution:      ResolutionSuperseded,
				SubjectHash:     source.ContentHash,
				ReplacementHash: replacement.ContentHash,
				Rationale:       base.Rationale, ExpiresAt: &future,
			},
			admin: true, want: ErrInvalid,
		},
		{
			name: "missing rationale",
			request: FindingResolutionRequest{
				Resolution:  ResolutionInvalidated,
				SubjectHash: source.ContentHash,
			},
			admin: true, want: ErrInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.CreateFindingResolution(
				context.Background(), "finding-1", test.request, 9, test.admin,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCreateFindingResolutionValidatesReplacementReview(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*reviewServiceStore, *ReviewSubject)
	}{
		{
			name: "missing completed review",
			mutate: func(store *reviewServiceStore, _ *ReviewSubject) {
				store.replacementRound = ReviewRound{}
			},
		},
		{
			name: "unfinished review",
			mutate: func(store *reviewServiceStore, _ *ReviewSubject) {
				store.replacementRound.Status = RoundRunning
			},
		},
		{
			name: "cross feature artifact",
			mutate: func(store *reviewServiceStore, replacement *ReviewSubject) {
				artifact := store.artifacts[replacement.ID]
				artifact.RequestID = "feat-2"
				store.artifacts[replacement.ID] = artifact
			},
		},
		{
			name: "old artifact version",
			mutate: func(store *reviewServiceStore, replacement *ReviewSubject) {
				artifact := store.artifacts[replacement.ID]
				artifact.Version = 1
				subject, err := BuildArtifactReviewSubject(artifact)
				if err != nil {
					t.Fatal(err)
				}
				store.artifacts[replacement.ID] = artifact
				store.replacementRound.Subject = subject
				store.gate.SubjectHash = subject.ContentHash
				*replacement = subject
			},
		},
		{
			name: "non passing fixed review",
			mutate: func(store *reviewServiceStore, _ *ReviewSubject) {
				store.gate.Decision = GateRevise
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, source, replacement := findingResolutionServiceFixture(t)
			test.mutate(store, &replacement)
			service := NewService(store, nil, time.Second)
			_, err := service.CreateFindingResolution(
				context.Background(),
				"finding-1",
				FindingResolutionRequest{
					Resolution:      ResolutionFixed,
					SubjectHash:     source.ContentHash,
					ReplacementHash: replacement.ContentHash,
					Rationale:       "Replacement review checked.",
				},
				9,
				true,
			)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v, want conflict", err)
			}
		})
	}
}

func TestCreateFindingResolutionAllowsSupersededNonPassingReview(t *testing.T) {
	store, source, replacement := findingResolutionServiceFixture(t)
	store.gate.Decision = GateRevise
	service := NewService(store, nil, time.Second)

	resolution, err := service.CreateFindingResolution(
		context.Background(),
		"finding-1",
		FindingResolutionRequest{
			Resolution:      ResolutionSuperseded,
			SubjectHash:     source.ContentHash,
			ReplacementHash: replacement.ContentHash,
			Rationale:       "A newer reviewed version replaces this finding.",
		},
		9,
		true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Resolution != ResolutionSuperseded {
		t.Fatalf("resolution = %+v", resolution)
	}
}

func TestListFindingResolutionsUsesFindingAuthorizationAndCursor(t *testing.T) {
	store, source, _ := findingResolutionServiceFixture(t)
	cursor := FindingResolutionCursor{
		CreatedAt: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
		ID:        "resolution-before",
	}
	store.resolutions = []FindingResolution{{
		ID: "resolution-1", FindingID: "finding-1",
		Resolution: ResolutionInvalidated, SubjectHash: source.ContentHash,
		Rationale: "Not applicable.", ActorID: 9,
		CreatedAt: time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC),
	}}
	service := NewService(store, nil, time.Second)

	resolutions, err := service.ListFindingResolutions(
		context.Background(), "finding-1", cursor, 25, 7, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolutions) != 1 ||
		store.resolutionSubject != source.ContentHash ||
		store.resolutionCursor != cursor ||
		store.resolutionLimit != 25 {
		t.Fatalf(
			"resolutions = %+v, subject = %q, cursor = %+v, limit = %d",
			resolutions, store.resolutionSubject, store.resolutionCursor, store.resolutionLimit,
		)
	}
	if _, err := service.ListFindingResolutions(
		context.Background(), "finding-1", FindingResolutionCursor{}, 20, 8, false,
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user error = %v, want not found", err)
	}
	if _, err := service.ListFindingResolutions(
		context.Background(), "finding-1", FindingResolutionCursor{}, 20, 8, true,
	); err != nil {
		t.Fatalf("administrator read: %v", err)
	}
}

func TestApprovalUsesLatestWaiverOrInvalidationOnly(t *testing.T) {
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	store, source, replacement := findingResolutionServiceFixture(t)
	store.round.Status = RoundCompleted
	store.gate = ReviewGateResult{
		ID: "gate-source", RoundID: store.round.ID,
		SubjectHash: source.ContentHash, Decision: GateRevise,
		BlockingIDs: []string{"finding-1"},
	}
	service := NewService(store, nil, time.Second)
	service.now = func() time.Time { return now }
	binding := ReviewApprovalBinding{
		SubjectHash:   source.ContentHash,
		ReviewRoundID: store.round.ID,
		GateResultID:  store.gate.ID,
	}
	future := now.Add(time.Hour)
	expired := now.Add(-time.Minute)
	tests := []struct {
		name        string
		resolutions []FindingResolution
		wantErr     bool
	}{
		{
			name: "active waiver",
			resolutions: []FindingResolution{{
				ID: "resolution-1", FindingID: "finding-1",
				Resolution: ResolutionWaived, SubjectHash: source.ContentHash,
				ExpiresAt: &future, CreatedAt: now,
			}},
		},
		{
			name: "invalidated",
			resolutions: []FindingResolution{{
				ID: "resolution-1", FindingID: "finding-1",
				Resolution: ResolutionInvalidated, SubjectHash: source.ContentHash,
				CreatedAt: now,
			}},
		},
		{
			name: "fixed belongs to replacement",
			resolutions: []FindingResolution{{
				ID: "resolution-1", FindingID: "finding-1",
				Resolution: ResolutionFixed, SubjectHash: source.ContentHash,
				ReplacementHash: replacement.ContentHash, CreatedAt: now,
			}},
			wantErr: true,
		},
		{
			name: "expired waiver",
			resolutions: []FindingResolution{{
				ID: "resolution-1", FindingID: "finding-1",
				Resolution: ResolutionWaived, SubjectHash: source.ContentHash,
				ExpiresAt: &expired, CreatedAt: now,
			}},
			wantErr: true,
		},
		{
			name: "latest fact wins",
			resolutions: []FindingResolution{
				{
					ID: "resolution-1", FindingID: "finding-1",
					Resolution: ResolutionInvalidated, SubjectHash: source.ContentHash,
					CreatedAt: now.Add(-time.Minute),
				},
				{
					ID: "resolution-2", FindingID: "finding-1",
					Resolution: ResolutionFixed, SubjectHash: source.ContentHash,
					ReplacementHash: replacement.ContentHash, CreatedAt: now,
				},
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store.resolutions = test.resolutions
			err := service.validateApprovalBinding(
				context.Background(), source, binding, DecisionApproved, "",
			)
			if test.wantErr != errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v, want conflict=%t", err, test.wantErr)
			}
		})
	}
}

func findingResolutionServiceFixture(
	t *testing.T,
) (*reviewServiceStore, ReviewSubject, ReviewSubject) {
	t.Helper()
	sourceArtifact := Artifact{
		ID: "artifact-1", RequestID: "feat-1", Kind: KindSystemDesign,
		Version: 1, ContentHash: strings.Repeat("a", 64),
	}
	replacementArtifact := Artifact{
		ID: "artifact-2", RequestID: "feat-1", Kind: KindSystemDesign,
		Version: 2, ParentArtifactID: sourceArtifact.ID,
		ContentHash: strings.Repeat("b", 64),
	}
	source, err := BuildArtifactReviewSubject(sourceArtifact)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := BuildArtifactReviewSubject(replacementArtifact)
	if err != nil {
		t.Fatal(err)
	}
	store := &reviewServiceStore{
		feature:  FeatureRequest{ID: "feat-1", CreatedBy: 7},
		artifact: sourceArtifact,
		artifacts: map[string]Artifact{
			sourceArtifact.ID:      sourceArtifact,
			replacementArtifact.ID: replacementArtifact,
		},
		round: ReviewRound{
			ID: "round-1", Subject: source, Status: RoundCompleted,
		},
		replacementRound: ReviewRound{
			ID: "round-2", Subject: replacement, Status: RoundCompleted,
		},
		finding: FindingDetail{
			FindingSummary: FindingSummary{
				ID: "finding-1", RoundID: "round-1",
			},
		},
		gate: ReviewGateResult{
			ID: "gate-2", RoundID: "round-2",
			SubjectHash: replacement.ContentHash, Decision: GatePass,
		},
	}
	return store, source, replacement
}

func TestCancelReviewRoundHandlesActiveAndTerminalStates(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	for _, status := range []ReviewRoundStatus{RoundCreated, RoundRunning, RoundEvaluating} {
		t.Run(string(status), func(t *testing.T) {
			store := &executionReviewStore{
				round: ReviewRound{ID: "round-1", Status: status},
				assignments: []ReviewAssignment{
					{ID: "queued", RoundID: "round-1", Status: AssignmentQueued},
					{ID: "running", RoundID: "round-1", Status: AssignmentRunning},
					{ID: "succeeded", RoundID: "round-1", Status: AssignmentSucceeded},
				},
			}
			service := NewService(store, nil, time.Second)
			service.now = func() time.Time { return now }

			if err := service.CancelReviewRound(context.Background(), "round-1", true); err != nil {
				t.Fatal(err)
			}
			if err := service.CancelReviewRound(context.Background(), "round-1", true); err != nil {
				t.Fatalf("duplicate cancellation: %v", err)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if store.round.Status != RoundCancelled ||
				store.round.CompletedAt == nil || !store.round.CompletedAt.Equal(now) {
				t.Fatalf("round = %+v", store.round)
			}
			if store.assignments[0].Status != AssignmentCancelled ||
				store.assignments[1].Status != AssignmentCancelled ||
				store.assignments[2].Status != AssignmentSucceeded {
				t.Fatalf("assignments = %+v", store.assignments)
			}
			if len(store.events) != 1 || store.events[0].Kind != ReviewEventRoundCancelled {
				t.Fatalf("events = %+v", store.events)
			}
		})
	}
	for _, status := range []ReviewRoundStatus{RoundCompleted, RoundFailed} {
		t.Run(string(status), func(t *testing.T) {
			store := &executionReviewStore{round: ReviewRound{ID: "round-1", Status: status}}
			service := NewService(store, nil, time.Second)
			if err := service.CancelReviewRound(
				context.Background(), "round-1", true,
			); !errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v, want conflict", err)
			}
		})
	}
	service := NewService(&executionReviewStore{}, nil, time.Second)
	if err := service.CancelReviewRound(
		context.Background(), "round-1", false,
	); !errors.Is(err, ErrForbidden) {
		t.Fatalf("regular user error = %v, want forbidden", err)
	}
}

func TestCancelReviewRoundStopsActiveReviewers(t *testing.T) {
	store := executionReviewFixture(t)
	service := NewService(store, nil, time.Second)
	started := make(chan struct{}, len(store.assignments))
	service.SetReviewRunner(reviewRunnerFunc(func(
		ctx context.Context,
		_ ReviewAssignmentRequest,
	) (ReviewReport, error) {
		started <- struct{}{}
		<-ctx.Done()
		return ReviewReport{}, context.Cause(ctx)
	}))
	result := make(chan error, 1)
	go func() {
		_, err := service.ExecuteReviewRound(
			context.Background(), store.round.ID, agentapi.Actor{UserID: 7}, true,
		)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("reviewer did not start")
	}

	if err := service.CancelReviewRound(context.Background(), store.round.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, errReviewRoundCancelled) {
			t.Fatalf("execution error = %v, want cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("review execution did not stop")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.round.Status != RoundCancelled {
		t.Fatalf("round status = %s, want cancelled", store.round.Status)
	}
	for _, assignment := range store.assignments {
		if assignment.Status != AssignmentCancelled {
			t.Fatalf("assignment = %+v", assignment)
		}
	}
	for index, event := range store.events {
		if event.Kind == ReviewEventRoundCancelled && index != len(store.events)-1 {
			t.Fatalf("event written after cancellation: %+v", store.events[index+1:])
		}
	}
}

func TestExecuteReviewRoundPinsRunnerBeforeLaunchingAssignments(t *testing.T) {
	store := executionReviewFixture(t)
	policy := store.policy
	policy.ContentHash = ""
	policy.MaxParallelism = 1
	prepared, err := PrepareReviewPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	store.policy = prepared
	store.round.PolicyHash = prepared.ContentHash

	service := NewService(store, nil, time.Second)
	firstStarted := make(chan struct{})
	release := make(chan struct{})
	var oldCalls atomic.Int32
	var newCalls atomic.Int32
	report := func(request ReviewAssignmentRequest) ReviewReport {
		return ReviewReport{
			Coverage: []CoverageItem{{
				Category: request.Assignment.Categories[0], Covered: true,
			}},
		}
	}
	service.SetReviewRunner(reviewRunnerFunc(func(
		_ context.Context,
		request ReviewAssignmentRequest,
	) (ReviewReport, error) {
		if oldCalls.Add(1) == 1 {
			close(firstStarted)
			<-release
		}
		return report(request), nil
	}))
	result := make(chan error, 1)
	go func() {
		_, runErr := service.ExecuteReviewRound(
			context.Background(), store.round.ID, agentapi.Actor{UserID: 7}, true,
		)
		result <- runErr
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first reviewer did not start")
	}
	service.SetReviewRunner(reviewRunnerFunc(func(
		_ context.Context,
		request ReviewAssignmentRequest,
	) (ReviewReport, error) {
		newCalls.Add(1)
		return report(request), nil
	}))
	close(release)
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("review execution did not finish")
	}
	if oldCalls.Load() != int32(len(store.assignments)) || newCalls.Load() != 0 {
		t.Fatalf("runner calls = old:%d new:%d", oldCalls.Load(), newCalls.Load())
	}
}

func TestReviewEventsPublishOnlyAfterPersistence(t *testing.T) {
	store := &executionReviewStore{round: ReviewRound{ID: "round-1", Status: RoundRunning}}
	service := NewService(store, nil, time.Second)
	channel, cancel := service.reviewHub.Subscribe(store.round.ID)
	defer cancel()

	persisted, err := service.appendReviewEvent(
		context.Background(), store.round.ID,
		ReviewEventAssignmentStarted, "started", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case published := <-channel:
		store.mu.Lock()
		eventCount := len(store.events)
		store.mu.Unlock()
		if eventCount != 1 || published.Seq != persisted.Seq {
			t.Fatalf("persisted = %+v, published = %+v, count = %d", persisted, published, eventCount)
		}
	default:
		t.Fatal("persisted event was not published")
	}

	store.mu.Lock()
	store.eventError = errors.New("event store unavailable")
	store.mu.Unlock()
	if _, err := service.appendReviewEvent(
		context.Background(), store.round.ID,
		ReviewEventAssignmentFailed, "failed", nil,
	); err == nil {
		t.Fatal("event persistence failure was hidden")
	}
	select {
	case event := <-channel:
		t.Fatalf("unpersisted event was published: %+v", event)
	default:
	}
}

func TestReviewEventsRedactSensitivePayloadBeforePersistence(t *testing.T) {
	store := &executionReviewStore{round: ReviewRound{ID: "round-1", Status: RoundRunning}}
	service := NewService(store, nil, time.Second)
	detail := json.RawMessage(
		`{"authorization":"Bearer detail-secret","dsn":"mysql://app:database-secret@db/service","note":"api_key=note-secret"}`,
	)

	event, err := service.appendReviewEvent(
		context.Background(),
		store.round.ID,
		ReviewEventAssignmentStarted,
		"Authorization: Bearer summary-secret",
		detail,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(event.Detail) {
		t.Fatalf("redacted event detail is not JSON: %s", event.Detail)
	}
	assertReviewSecretsAbsent(t, event, []string{
		"summary-secret", "detail-secret", "database-secret", "note-secret",
	})
	store.mu.Lock()
	persisted := store.events[0]
	store.mu.Unlock()
	assertReviewSecretsAbsent(t, persisted, []string{
		"summary-secret", "detail-secret", "database-secret", "note-secret",
	})
}

func TestExecuteReviewRoundDoesNotEvaluateAfterReportPersistenceFailure(t *testing.T) {
	policy := testReviewPolicy(t)
	artifact := executionReviewArtifact()
	subject, err := BuildArtifactReviewSubject(artifact)
	if err != nil {
		t.Fatal(err)
	}
	round := ReviewRound{
		ID:       "round-1",
		Subject:  subject,
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
		Status: RoundCreated,
	}
	completeArtifactReviewRoundSnapshot(t, policy, artifact, &round)
	assignments := make([]ReviewAssignment, 0, len(round.Reviewers))
	for index, reviewer := range round.Reviewers {
		assignments = append(assignments, ReviewAssignment{
			ID: "assignment-" + reviewer.ID, RoundID: round.ID, ReviewerID: reviewer.ID,
			Agent: reviewer.Agent, DefinitionHash: reviewer.DefinitionHash,
			Categories: append([]string(nil), reviewer.Categories...), Required: reviewer.Required,
			Status: AssignmentQueued, Attempt: 1,
			CreatedAt: time.Date(2026, 8, 5, 10, index, 0, 0, time.UTC),
		})
	}
	persistenceErr := errors.New("report store unavailable")
	store := &executionReviewStore{
		artifacts: map[string]Artifact{artifact.ID: artifact}, round: round, policy: policy,
		assignments: assignments, reportError: persistenceErr,
	}
	service := NewService(store, nil, time.Second)
	now := time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	service.SetReviewRunner(reviewRunnerFunc(func(
		_ context.Context,
		request ReviewAssignmentRequest,
	) (ReviewReport, error) {
		return ReviewReport{
			RoundID: request.Round.ID, AssignmentID: request.Assignment.ID,
			ReviewerID:  request.Assignment.ReviewerID,
			SubjectHash: request.Round.Subject.ContentHash,
			Coverage: []CoverageItem{{
				Category: request.Assignment.Categories[0], Covered: true,
			}},
		}, nil
	}))

	result, err := service.ExecuteReviewRound(
		context.Background(), round.ID, agentapi.Actor{UserID: 7}, true,
	)
	if result != nil || !errors.Is(err, persistenceErr) {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.round.Status != RoundFailed {
		t.Fatalf("round status = %s, want %s", store.round.Status, RoundFailed)
	}
	if store.evaluationLoads != 0 || store.gateCompletions != 0 {
		t.Fatalf(
			"evaluation loads = %d, gate completions = %d",
			store.evaluationLoads, store.gateCompletions,
		)
	}
	for _, assignment := range store.assignments {
		if assignment.Status != AssignmentFailed || assignment.ErrorCode != "report_persistence_failed" {
			t.Fatalf("assignment = %+v", assignment)
		}
	}
}

func TestExecuteReviewRoundSkipsAdjudicatorWithoutConflict(t *testing.T) {
	store := executionReviewFixture(t)
	service := NewService(store, nil, time.Second)
	service.SetReviewRunner(reviewRunnerFunc(func(
		_ context.Context,
		request ReviewAssignmentRequest,
	) (ReviewReport, error) {
		return executionReviewReport(request, false), nil
	}))
	var adjudicationCalls atomic.Int32
	service.SetAdjudicationRunner(adjudicationRunnerFunc(func(
		context.Context,
		ReviewAdjudicationRequest,
	) (AdjudicationOutcome, error) {
		adjudicationCalls.Add(1)
		return AdjudicationOutcome{
			Decision:  AdjudicationConfirmed,
			Rationale: "This path must not execute.",
		}, nil
	}))

	result, err := service.ExecuteReviewRound(
		context.Background(), store.round.ID, agentapi.Actor{UserID: 7}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GatePass || adjudicationCalls.Load() != 0 {
		t.Fatalf("result = %+v, adjudication calls = %d", result, adjudicationCalls.Load())
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.adjudications) != 0 || store.evaluationLoads != 1 ||
		store.gateCompletions != 1 {
		t.Fatalf(
			"adjudications = %d, evaluation loads = %d, gate completions = %d",
			len(store.adjudications), store.evaluationLoads, store.gateCompletions,
		)
	}
}

func TestExecuteReviewRoundPersistsConfirmedAdjudicationAndRevises(t *testing.T) {
	store := executionReviewFixture(t)
	service := NewService(store, nil, time.Second)
	service.SetReviewRunner(reviewRunnerFunc(func(
		_ context.Context,
		request ReviewAssignmentRequest,
	) (ReviewReport, error) {
		return executionReviewReport(request, true), nil
	}))
	var captured ReviewAdjudicationRequest
	service.SetAdjudicationRunner(adjudicationRunnerFunc(func(
		_ context.Context,
		request ReviewAdjudicationRequest,
	) (AdjudicationOutcome, error) {
		captured = request
		return AdjudicationOutcome{
			Decision:  AdjudicationConfirmed,
			Rationale: "The high-severity evidence is confirmed.",
		}, nil
	}))

	result, err := service.ExecuteReviewRound(
		context.Background(), store.round.ID,
		agentapi.Actor{UserID: 7, TenantID: "tenant-1"}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision != GateRevise || len(result.ConflictIDs) != 0 ||
		len(result.BlockingIDs) != 1 || len(result.AdjudicationHashes) != 1 {
		t.Fatalf("gate result = %+v", result)
	}
	if captured.Round.ID != store.round.ID ||
		captured.Policy.ContentHash != store.policy.ContentHash ||
		len(captured.Findings) != 2 ||
		captured.Findings[0].Fingerprint != captured.Findings[1].Fingerprint ||
		captured.Actor.TenantID != "tenant-1" {
		t.Fatalf("adjudication request = %+v", captured)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.adjudications) != 1 ||
		store.adjudications[0].Decision != AdjudicationConfirmed ||
		store.adjudications[0].Agent != store.policy.Adjudicator.Agent ||
		store.adjudications[0].DefinitionHash != store.policy.Adjudicator.DefinitionHash ||
		store.evaluationLoads != 2 || store.gateCompletions != 1 {
		t.Fatalf(
			"adjudications = %+v, evaluation loads = %d, gate completions = %d",
			store.adjudications, store.evaluationLoads, store.gateCompletions,
		)
	}
}

func TestExecuteReviewRoundKeepsAdjudicatorFailureHumanRequired(t *testing.T) {
	for _, test := range []struct {
		name          string
		configure     func(*Service)
		wantErrorCode string
	}{
		{
			name: "runner failure",
			configure: func(service *Service) {
				service.SetAdjudicationRunner(adjudicationRunnerFunc(func(
					context.Context,
					ReviewAdjudicationRequest,
				) (AdjudicationOutcome, error) {
					return AdjudicationOutcome{}, errors.New("adjudicator backend failed")
				}))
			},
			wantErrorCode: "adjudicator_failed",
		},
		{
			name:          "runner unavailable",
			configure:     func(*Service) {},
			wantErrorCode: "adjudicator_unavailable",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := executionReviewFixture(t)
			service := NewService(store, nil, time.Second)
			service.SetReviewRunner(reviewRunnerFunc(func(
				_ context.Context,
				request ReviewAssignmentRequest,
			) (ReviewReport, error) {
				return executionReviewReport(request, true), nil
			}))
			test.configure(service)

			result, err := service.ExecuteReviewRound(
				context.Background(), store.round.ID,
				agentapi.Actor{UserID: 7}, true,
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decision != GateHumanRequired ||
				len(result.ConflictIDs) != 2 ||
				len(result.AdjudicationHashes) != 1 {
				t.Fatalf("gate result = %+v", result)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if len(store.adjudications) != 1 ||
				store.adjudications[0].Decision != AdjudicationNeedsHuman ||
				store.adjudications[0].ErrorCode != test.wantErrorCode ||
				store.gateCompletions != 1 {
				t.Fatalf("adjudications = %+v, gate completions = %d", store.adjudications, store.gateCompletions)
			}
		})
	}
}

func TestExecuteReviewRoundFailsWhenAdjudicationPersistenceFails(t *testing.T) {
	persistenceErr := errors.New("adjudication store unavailable")
	store := executionReviewFixture(t)
	store.adjudicationError = persistenceErr
	service := NewService(store, nil, time.Second)
	service.SetReviewRunner(reviewRunnerFunc(func(
		_ context.Context,
		request ReviewAssignmentRequest,
	) (ReviewReport, error) {
		return executionReviewReport(request, true), nil
	}))
	service.SetAdjudicationRunner(adjudicationRunnerFunc(func(
		context.Context,
		ReviewAdjudicationRequest,
	) (AdjudicationOutcome, error) {
		return AdjudicationOutcome{
			Decision:  AdjudicationConfirmed,
			Rationale: "The high-severity evidence is confirmed.",
		}, nil
	}))

	result, err := service.ExecuteReviewRound(
		context.Background(), store.round.ID, agentapi.Actor{UserID: 7}, true,
	)
	if result != nil || !errors.Is(err, persistenceErr) {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.round.Status != RoundFailed || len(store.adjudications) != 0 ||
		store.gateCompletions != 0 {
		t.Fatalf(
			"round = %+v, adjudications = %+v, gate completions = %d",
			store.round, store.adjudications, store.gateCompletions,
		)
	}
	for _, event := range store.events {
		if event.Kind == ReviewEventAdjudicationFinished ||
			event.Kind == ReviewEventRoundCompleted {
			t.Fatalf("event published after adjudication persistence failure: %+v", event)
		}
	}
}

func executionReviewFixture(t *testing.T) *executionReviewStore {
	t.Helper()
	policy := testReviewPolicy(t)
	artifact := executionReviewArtifact()
	subject, err := BuildArtifactReviewSubject(artifact)
	if err != nil {
		t.Fatal(err)
	}
	round := ReviewRound{
		ID:       "round-1",
		Subject:  subject,
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: policy.ContentHash,
		Status: RoundCreated,
	}
	completeArtifactReviewRoundSnapshot(t, policy, artifact, &round)
	assignments := make([]ReviewAssignment, 0, len(round.Reviewers))
	for index, reviewer := range round.Reviewers {
		assignments = append(assignments, ReviewAssignment{
			ID: "assignment-" + reviewer.ID, RoundID: round.ID, ReviewerID: reviewer.ID,
			Agent: reviewer.Agent, DefinitionHash: reviewer.DefinitionHash,
			Categories: append([]string(nil), reviewer.Categories...), Required: reviewer.Required,
			Status: AssignmentQueued, Attempt: 1,
			CreatedAt: time.Date(2026, 8, 5, 10, index, 0, 0, time.UTC),
		})
	}
	return &executionReviewStore{
		artifacts: map[string]Artifact{artifact.ID: artifact},
		round:     round, policy: policy, assignments: assignments,
	}
}

func executionReviewReport(
	request ReviewAssignmentRequest,
	withConflict bool,
) ReviewReport {
	report := ReviewReport{
		RoundID: request.Round.ID, AssignmentID: request.Assignment.ID,
		ReviewerID:  request.Assignment.ReviewerID,
		SubjectHash: request.Round.Subject.ContentHash,
		Coverage: []CoverageItem{{
			Category: request.Assignment.Categories[0], Covered: true,
		}},
		Summary: "Review completed.",
	}
	if !withConflict {
		return report
	}
	severity := SeverityMedium
	if request.Assignment.ReviewerID == "architecture" {
		severity = SeverityHigh
	}
	report.Findings = []Finding{{
		Category: "architecture", Severity: severity,
		Claim:  "The service contract is inconsistent.",
		Impact: "Requests can fail across the boundary.",
		Evidence: []FindingEvidence{{
			Kind: "subject", Ref: "architecture_decision_record",
			Hash:    request.Round.Subject.ContentHash,
			Summary: "The immutable subject contains the inconsistent contract.",
		}},
		Recommendation: "Align the contract before delivery.",
		Confidence:     0.9,
	}}
	return report
}

func executionReviewArtifact() Artifact {
	return Artifact{
		ID: "artifact-1", RequestID: "feature-1", Kind: KindSystemDesign,
		Version: 1, Origin: OriginAgent,
		DocumentJSON:     json.RawMessage(`{"architecture_decision_record":{"status":"accepted"}}`),
		RenderedMarkdown: "# System Design\n\nAccepted architecture.",
		Evidence: []EvidenceRef{{
			Kind: "code", Repo: "nasuta", Path: "internal/feature/delivery/review_service.go",
			Summary: "Review orchestration", Hash: "evidence-hash",
		}},
		ContentHash: "artifact-hash",
	}
}

func completeArtifactReviewRoundSnapshot(
	t *testing.T,
	policy ReviewPolicy,
	artifact Artifact,
	round *ReviewRound,
) {
	t.Helper()
	facts, err := BuildArtifactReviewRiskFacts(artifact)
	if err != nil {
		t.Fatal(err)
	}
	facts, riskHash, reviewers, panelHash, err := PrepareReviewPanel(policy, facts)
	if err != nil {
		t.Fatal(err)
	}
	round.RiskFacts = facts
	round.RiskHash = riskHash
	round.RuleVersion = policy.RiskRuleVersion
	round.Reviewers = reviewers
	round.PanelHash = panelHash
}

func TestExecuteReviewRoundFailsPolicySnapshotMismatch(t *testing.T) {
	policy := testReviewPolicy(t)
	round := ReviewRound{
		ID: "round-1",
		Subject: ReviewSubject{
			Kind: SubjectSystemDesign, ID: "artifact-1", Version: 1,
			SourceContentHash: "artifact-hash", ContentHash: "subject-hash",
		},
		PolicyID: policy.ID, PolicyVersion: policy.Version, PolicyHash: "original-policy-hash",
		Status: RoundCreated,
	}
	store := &executionReviewStore{round: round, policy: policy}
	service := NewService(store, nil, time.Second)
	now := time.Date(2026, 8, 5, 11, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	reviewerCalled := false
	service.SetReviewRunner(reviewRunnerFunc(func(
		context.Context,
		ReviewAssignmentRequest,
	) (ReviewReport, error) {
		reviewerCalled = true
		return ReviewReport{}, nil
	}))

	result, err := service.ExecuteReviewRound(
		context.Background(), round.ID, agentapi.Actor{UserID: 7}, true,
	)
	if result != nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("result = %+v, error = %v", result, err)
	}
	if reviewerCalled {
		t.Fatal("reviewer ran for a mismatched policy snapshot")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.round.Status != RoundFailed {
		t.Fatalf("round status = %s, want %s", store.round.Status, RoundFailed)
	}
	if store.round.CompletedAt == nil || !store.round.CompletedAt.Equal(now) {
		t.Fatalf("round completed_at = %v, want %v", store.round.CompletedAt, now)
	}
}
