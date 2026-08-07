package evaluation

import (
	"context"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	defaultWindow    = 30 * 24 * time.Hour
	maxWindow        = 90 * 24 * time.Hour
	defaultLabelPage = 20
	maxLabelPage     = 100
)

var canonicalCategory = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

type repository interface {
	WorkflowTrace(context.Context, string, int64, bool) (*WorkflowTrace, error)
	AgentVersionMetrics(context.Context, string, int64, Window) (AgentVersionMetrics, error)
	WorkflowVersionMetrics(context.Context, string, int64, Window) (WorkflowVersionMetrics, error)
	ReviewPolicyVersionMetrics(context.Context, string, int64, Window) (ReviewPolicyVersionMetrics, error)
	CreateReviewLabels(context.Context, string, []ReviewLabelInput, int64, time.Time) ([]ReviewLabel, error)
	ListReviewLabels(context.Context, string, int64, int) ([]ReviewLabel, error)
}

type Service struct {
	store repository
	now   func() time.Time
}

func NewService(store repository) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("evaluation repository is required")
	}
	return &Service{store: store, now: time.Now}, nil
}

func (service *Service) WorkflowTrace(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (*WorkflowTrace, error) {
	if runID == "" || userID <= 0 {
		return nil, fmt.Errorf("workflow run and actor are required: %w", ErrInvalid)
	}
	return service.store.WorkflowTrace(ctx, runID, userID, admin)
}

func (service *Service) CompareAgentVersions(
	ctx context.Context,
	agentID string,
	baseVersion int64,
	candidateVersion int64,
	from time.Time,
	to time.Time,
	admin bool,
) (Comparison[AgentVersionMetrics], error) {
	var comparison Comparison[AgentVersionMetrics]
	window, err := service.comparisonWindow(
		agentID, baseVersion, candidateVersion, from, to, admin,
	)
	if err != nil {
		return comparison, err
	}
	base, err := service.store.AgentVersionMetrics(
		ctx, agentID, baseVersion, window,
	)
	if err != nil {
		return comparison, err
	}
	candidate, err := service.store.AgentVersionMetrics(
		ctx, agentID, candidateVersion, window,
	)
	if err != nil {
		return comparison, err
	}
	finalizeAgentMetrics(&base)
	finalizeAgentMetrics(&candidate)
	return Comparison[AgentVersionMetrics]{
		ID: agentID, Window: window, Base: base, Candidate: candidate,
	}, nil
}

func (service *Service) CompareWorkflowVersions(
	ctx context.Context,
	workflowID string,
	baseVersion int64,
	candidateVersion int64,
	from time.Time,
	to time.Time,
	admin bool,
) (Comparison[WorkflowVersionMetrics], error) {
	var comparison Comparison[WorkflowVersionMetrics]
	window, err := service.comparisonWindow(
		workflowID, baseVersion, candidateVersion, from, to, admin,
	)
	if err != nil {
		return comparison, err
	}
	base, err := service.store.WorkflowVersionMetrics(
		ctx, workflowID, baseVersion, window,
	)
	if err != nil {
		return comparison, err
	}
	candidate, err := service.store.WorkflowVersionMetrics(
		ctx, workflowID, candidateVersion, window,
	)
	if err != nil {
		return comparison, err
	}
	finalizeWorkflowMetrics(&base)
	finalizeWorkflowMetrics(&candidate)
	return Comparison[WorkflowVersionMetrics]{
		ID: workflowID, Window: window, Base: base, Candidate: candidate,
	}, nil
}

func (service *Service) CompareReviewPolicyVersions(
	ctx context.Context,
	policyID string,
	baseVersion int64,
	candidateVersion int64,
	from time.Time,
	to time.Time,
	admin bool,
) (Comparison[ReviewPolicyVersionMetrics], error) {
	var comparison Comparison[ReviewPolicyVersionMetrics]
	window, err := service.comparisonWindow(
		policyID, baseVersion, candidateVersion, from, to, admin,
	)
	if err != nil {
		return comparison, err
	}
	base, err := service.store.ReviewPolicyVersionMetrics(
		ctx, policyID, baseVersion, window,
	)
	if err != nil {
		return comparison, err
	}
	candidate, err := service.store.ReviewPolicyVersionMetrics(
		ctx, policyID, candidateVersion, window,
	)
	if err != nil {
		return comparison, err
	}
	finalizeReviewMetrics(&base)
	finalizeReviewMetrics(&candidate)
	return Comparison[ReviewPolicyVersionMetrics]{
		ID: policyID, Window: window, Base: base, Candidate: candidate,
	}, nil
}

func (service *Service) CreateReviewLabels(
	ctx context.Context,
	roundID string,
	inputs []ReviewLabelInput,
	actorUserID int64,
	admin bool,
) ([]ReviewLabel, error) {
	if !admin {
		return nil, ErrForbidden
	}
	if roundID == "" || actorUserID <= 0 || len(inputs) == 0 ||
		len(inputs) > maxReviewLabels {
		return nil, fmt.Errorf("review round and 1-%d labels are required: %w", maxReviewLabels, ErrInvalid)
	}
	for index, input := range inputs {
		switch input.Label {
		case LabelTruePositive, LabelFalsePositive:
			if input.FindingID == "" || input.TargetHash != "" || input.Category != "" {
				return nil, fmt.Errorf(
					"review label %d must identify only a persisted finding: %w",
					index, ErrInvalid,
				)
			}
		case LabelFalseNegative:
			if input.FindingID != "" || !validSHA256(input.TargetHash) ||
				!canonicalCategory.MatchString(input.Category) {
				return nil, fmt.Errorf(
					"review label %d must identify a canonical missed finding: %w",
					index, ErrInvalid,
				)
			}
		default:
			return nil, fmt.Errorf(
				"review label %d kind %q is invalid: %w",
				index, input.Label, ErrInvalid,
			)
		}
	}
	return service.store.CreateReviewLabels(
		ctx, roundID, inputs, actorUserID, service.now().UTC(),
	)
}

func (service *Service) ListReviewLabels(
	ctx context.Context,
	roundID string,
	afterSeq int64,
	limit int,
	admin bool,
) ([]ReviewLabel, error) {
	if !admin {
		return nil, ErrForbidden
	}
	if roundID == "" || afterSeq < 0 {
		return nil, fmt.Errorf("review round or cursor is invalid: %w", ErrInvalid)
	}
	if limit <= 0 {
		limit = defaultLabelPage
	}
	if limit > maxLabelPage {
		return nil, fmt.Errorf("review label limit exceeds %d: %w", maxLabelPage, ErrInvalid)
	}
	return service.store.ListReviewLabels(ctx, roundID, afterSeq, limit)
}

func (service *Service) comparisonWindow(
	id string,
	baseVersion int64,
	candidateVersion int64,
	from time.Time,
	to time.Time,
	admin bool,
) (Window, error) {
	if !admin {
		return Window{}, ErrForbidden
	}
	if id == "" || baseVersion <= 0 || candidateVersion <= 0 ||
		baseVersion == candidateVersion {
		return Window{}, fmt.Errorf("resource and two distinct positive versions are required: %w", ErrInvalid)
	}
	now := service.now().UTC()
	if from.IsZero() && to.IsZero() {
		return Window{From: now.Add(-defaultWindow), To: now}, nil
	}
	if from.IsZero() || to.IsZero() {
		return Window{}, fmt.Errorf("from and to must be provided together: %w", ErrInvalid)
	}
	from = from.UTC()
	to = to.UTC()
	if !from.Before(to) || to.Sub(from) > maxWindow {
		return Window{}, fmt.Errorf("evaluation window must be positive and at most 90 days: %w", ErrInvalid)
	}
	return Window{From: from, To: to}, nil
}

func finalizeAgentMetrics(metrics *AgentVersionMetrics) {
	metrics.SuccessRate = ratio(metrics.SuccessCount, metrics.RunCount)
	metrics.EvidenceCompletenessRate = ratio(
		metrics.EvidenceCompleteCount,
		metrics.EvidenceRequiredRunCount,
	)
	metrics.AverageTotalTokens = ratio(metrics.TotalTokens, metrics.RunCount)
	metrics.ToolFailureRate = ratio(metrics.ToolFailures, metrics.ToolCalls)
}

func finalizeWorkflowMetrics(metrics *WorkflowVersionMetrics) {
	metrics.SuccessRate = ratio(metrics.SuccessCount, metrics.RunCount)
	metrics.RecoveryRate = ratio(metrics.RecoveredRunCount, metrics.RunCount)
	metrics.AverageTotalTokens = ratio(metrics.TotalTokens, metrics.RunCount)
}

func finalizeReviewMetrics(metrics *ReviewPolicyVersionMetrics) {
	metrics.CompletionRate = ratio(metrics.CompletedRoundCount, metrics.RoundCount)
	metrics.PassRate = ratio(metrics.PassedRoundCount, metrics.CompletedRoundCount)
	metrics.UniqueYield = ratio(metrics.UniqueFindingCount, metrics.ReportCount)
	metrics.DuplicateRate = ratio(
		metrics.FindingCount-metrics.UniqueFindingCount,
		metrics.FindingCount,
	)
	metrics.ConflictRate = ratio(
		metrics.ConflictRoundCount,
		metrics.CompletedRoundCount,
	)
	metrics.AdoptionRate = ratio(
		metrics.AdoptedFindingCount,
		metrics.LabeledResolutionCount,
	)
	precisionDenominator := metrics.TruePositiveCount + metrics.FalsePositiveCount
	metrics.PrecisionAvailable = precisionDenominator > 0
	metrics.Precision = ratio(metrics.TruePositiveCount, precisionDenominator)
	recallDenominator := metrics.TruePositiveCount + metrics.FalseNegativeCount
	metrics.RecallAvailable = recallDenominator > 0
	metrics.Recall = ratio(metrics.TruePositiveCount, recallDenominator)
}

func validSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
