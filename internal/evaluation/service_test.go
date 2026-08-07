package evaluation

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recordingRepository struct {
	agentMetrics    map[int64]AgentVersionMetrics
	workflowMetrics map[int64]WorkflowVersionMetrics
	reviewMetrics   map[int64]ReviewPolicyVersionMetrics
	windows         []Window
	createdLabels   []ReviewLabelInput
	createdAt       time.Time
}

func (repository *recordingRepository) WorkflowTrace(
	context.Context,
	string,
	int64,
	bool,
) (*WorkflowTrace, error) {
	return &WorkflowTrace{}, nil
}

func (repository *recordingRepository) AgentVersionMetrics(
	_ context.Context,
	_ string,
	version int64,
	window Window,
) (AgentVersionMetrics, error) {
	repository.windows = append(repository.windows, window)
	return repository.agentMetrics[version], nil
}

func (repository *recordingRepository) WorkflowVersionMetrics(
	_ context.Context,
	_ string,
	version int64,
	window Window,
) (WorkflowVersionMetrics, error) {
	repository.windows = append(repository.windows, window)
	return repository.workflowMetrics[version], nil
}

func (repository *recordingRepository) ReviewPolicyVersionMetrics(
	_ context.Context,
	_ string,
	version int64,
	window Window,
) (ReviewPolicyVersionMetrics, error) {
	repository.windows = append(repository.windows, window)
	return repository.reviewMetrics[version], nil
}

func (repository *recordingRepository) CreateReviewLabels(
	_ context.Context,
	roundID string,
	inputs []ReviewLabelInput,
	actorUserID int64,
	createdAt time.Time,
) ([]ReviewLabel, error) {
	repository.createdLabels = append([]ReviewLabelInput(nil), inputs...)
	repository.createdAt = createdAt
	return []ReviewLabel{{
		Seq: 1, RoundID: roundID, Label: inputs[0].Label,
		CreatedBy: actorUserID, CreatedAt: createdAt,
	}}, nil
}

func (repository *recordingRepository) ListReviewLabels(
	context.Context,
	string,
	int64,
	int,
) ([]ReviewLabel, error) {
	return nil, nil
}

func TestCompareAgentVersionsUsesDefaultWindowAndStableDenominators(t *testing.T) {
	now := time.Date(2026, 8, 7, 10, 30, 0, 0, time.UTC)
	repository := &recordingRepository{agentMetrics: map[int64]AgentVersionMetrics{
		1: {
			Version: 1, RunCount: 4, SuccessCount: 3,
			EvidenceRequiredRunCount: 2, EvidenceCompleteCount: 1,
			TotalTokens: 100, ToolCalls: 5, ToolFailures: 1,
		},
		2: {
			Version: 2, RunCount: 2, SuccessCount: 2,
			EvidenceRequiredRunCount: 0, EvidenceCompleteCount: 0,
			TotalTokens: 60, ToolCalls: 0, ToolFailures: 0,
		},
	}}
	service, err := NewService(repository)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.now = func() time.Time { return now }

	comparison, err := service.CompareAgentVersions(
		t.Context(), "qa.answerer", 1, 2, time.Time{}, time.Time{}, true,
	)
	if err != nil {
		t.Fatalf("CompareAgentVersions: %v", err)
	}
	if comparison.Window.To != now ||
		comparison.Window.From != now.Add(-30*24*time.Hour) ||
		len(repository.windows) != 2 ||
		repository.windows[0] != comparison.Window ||
		repository.windows[1] != comparison.Window {
		t.Fatalf("comparison window = %+v calls=%+v", comparison.Window, repository.windows)
	}
	if comparison.Base.SuccessRate != 0.75 ||
		comparison.Base.EvidenceCompletenessRate != 0.5 ||
		comparison.Base.AverageTotalTokens != 25 ||
		comparison.Base.ToolFailureRate != 0.2 {
		t.Fatalf("base metrics = %+v", comparison.Base)
	}
	if comparison.Candidate.EvidenceCompletenessRate != 0 ||
		comparison.Candidate.ToolFailureRate != 0 {
		t.Fatalf("candidate zero denominators = %+v", comparison.Candidate)
	}
}

func TestCompareReviewPolicyVersionsExposesPrecisionOnlyWithLabels(t *testing.T) {
	repository := &recordingRepository{reviewMetrics: map[int64]ReviewPolicyVersionMetrics{
		1: {
			Version: 1, RoundCount: 10, CompletedRoundCount: 8,
			PassedRoundCount: 6, ReportCount: 16, FindingCount: 20,
			UniqueFindingCount: 15, ConflictRoundCount: 2,
			LabeledResolutionCount: 5, AdoptedFindingCount: 4,
		},
		2: {
			Version: 2, RoundCount: 4, CompletedRoundCount: 4,
			PassedRoundCount: 4, ReportCount: 8, FindingCount: 10,
			UniqueFindingCount: 9, TruePositiveCount: 7,
			FalsePositiveCount: 1, FalseNegativeCount: 2,
		},
	}}
	service, err := NewService(repository)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	comparison, err := service.CompareReviewPolicyVersions(
		t.Context(), "review.change", 1, 2,
		time.Now().Add(-time.Hour), time.Now(), true,
	)
	if err != nil {
		t.Fatalf("CompareReviewPolicyVersions: %v", err)
	}
	if comparison.Base.PrecisionAvailable || comparison.Base.RecallAvailable ||
		comparison.Base.DuplicateRate != 0.25 ||
		comparison.Base.UniqueYield != 0.9375 ||
		comparison.Base.AdoptionRate != 0.8 {
		t.Fatalf("base review metrics = %+v", comparison.Base)
	}
	if !comparison.Candidate.PrecisionAvailable ||
		!comparison.Candidate.RecallAvailable ||
		comparison.Candidate.Precision != 0.875 ||
		comparison.Candidate.Recall != 7.0/9.0 {
		t.Fatalf("candidate review metrics = %+v", comparison.Candidate)
	}
}

func TestComparisonRequiresAdministratorAndBoundedWindow(t *testing.T) {
	service, err := NewService(&recordingRepository{
		agentMetrics: map[int64]AgentVersionMetrics{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := service.CompareAgentVersions(
		t.Context(), "qa.answerer", 1, 2, time.Time{}, time.Time{}, false,
	); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin error = %v", err)
	}
	from := time.Now().Add(-91 * 24 * time.Hour)
	if _, err := service.CompareAgentVersions(
		t.Context(), "qa.answerer", 1, 2, from, time.Now(), true,
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized window error = %v", err)
	}
}

func TestCreateReviewLabelsRequiresCanonicalImmutableFacts(t *testing.T) {
	now := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	repository := &recordingRepository{}
	service, err := NewService(repository)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	service.now = func() time.Time { return now }
	input := ReviewLabelInput{
		Label: LabelFalseNegative, TargetHash: string(make([]byte, 64)),
		Category: "security",
	}
	if _, err := service.CreateReviewLabels(
		t.Context(), "round-1", []ReviewLabelInput{input}, 7, true,
	); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid hash error = %v", err)
	}
	input.TargetHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := service.CreateReviewLabels(
		t.Context(), "round-1", []ReviewLabelInput{input}, 7, false,
	); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin label error = %v", err)
	}
	labels, err := service.CreateReviewLabels(
		t.Context(), "round-1", []ReviewLabelInput{input}, 7, true,
	)
	if err != nil {
		t.Fatalf("CreateReviewLabels: %v", err)
	}
	if len(labels) != 1 || repository.createdAt != now ||
		len(repository.createdLabels) != 1 ||
		repository.createdLabels[0] != input {
		t.Fatalf(
			"labels=%+v stored=%+v created_at=%s",
			labels, repository.createdLabels, repository.createdAt,
		)
	}
}
