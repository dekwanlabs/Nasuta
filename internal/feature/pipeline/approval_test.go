package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
)

type approvalArtifactService struct {
	artifact    delivery.Artifact
	generation  delivery.GenerationRun
	reviewCalls int
}

func (service *approvalArtifactService) ReviewArtifact(
	_ context.Context,
	featureID string,
	artifactID string,
	decision delivery.ReviewDecision,
	comment string,
	binding delivery.ReviewApprovalBinding,
	reviewerID int64,
) error {
	if featureID != service.artifact.RequestID || artifactID != service.artifact.ID {
		return delivery.ErrNotFound
	}
	service.reviewCalls++
	review := &delivery.ArtifactReview{
		ArtifactID: artifactID, SubjectHash: binding.SubjectHash,
		ReviewRoundID: binding.ReviewRoundID, GateResultID: binding.GateResultID,
		Decision: decision, Comment: comment, Reviewer: reviewerID,
	}
	if service.artifact.Review != nil {
		existing := service.artifact.Review
		if existing.Decision != review.Decision ||
			existing.Comment != review.Comment ||
			existing.Reviewer != review.Reviewer {
			return delivery.ErrConflict
		}
		return nil
	}
	service.artifact.Review = review
	return nil
}

func (service *approvalArtifactService) GetArtifact(
	_ context.Context,
	featureID string,
	artifactID string,
	_ int64,
	_ bool,
) (*delivery.Artifact, error) {
	if featureID != service.artifact.RequestID || artifactID != service.artifact.ID {
		return nil, delivery.ErrNotFound
	}
	artifact := service.artifact
	if service.artifact.Review != nil {
		review := *service.artifact.Review
		artifact.Review = &review
	}
	return &artifact, nil
}

func (service *approvalArtifactService) GetGenerationForArtifact(
	_ context.Context,
	featureID string,
	artifactID string,
	_ int64,
	_ bool,
) (*delivery.GenerationRun, error) {
	if featureID != service.generation.RequestID ||
		artifactID != service.generation.ArtifactID {
		return nil, delivery.ErrNotFound
	}
	run := service.generation
	return &run, nil
}

type approvalWorkflowService struct {
	run          workflow.RunRecord
	handoffs     []workflow.Handoff
	artifacts    *approvalArtifactService
	decideCalls  int
	decideErrors []error
	lastRequest  workflow.ApprovalRequest
}

func (service *approvalWorkflowService) GetRun(
	_ context.Context,
	runID string,
	_ int64,
	_ bool,
) (*workflow.RunRecord, error) {
	if runID != service.run.ID {
		return nil, workflow.ErrNotFound
	}
	run := service.run
	return &run, nil
}

func (service *approvalWorkflowService) ListHandoffs(
	_ context.Context,
	runID string,
	_ workflow.HandoffCursor,
	limit int,
	_ int64,
	_ bool,
) ([]workflow.Handoff, error) {
	if runID != service.run.ID {
		return nil, workflow.ErrNotFound
	}
	if limit != maxPipelineHandoffs {
		return nil, errors.New("pipeline handoff read was not bounded")
	}
	return append([]workflow.Handoff(nil), service.handoffs...), nil
}

func (service *approvalWorkflowService) DecideHumanApproval(
	_ context.Context,
	request workflow.ApprovalRequest,
) (workflow.ApprovalResult, error) {
	service.decideCalls++
	service.lastRequest = request
	if service.artifacts != nil && service.artifacts.artifact.Review == nil {
		return workflow.ApprovalResult{}, errors.New("workflow approval preceded artifact review")
	}
	if len(service.decideErrors) > 0 {
		err := service.decideErrors[0]
		service.decideErrors = service.decideErrors[1:]
		if err != nil {
			return workflow.ApprovalResult{}, err
		}
	}
	return workflow.ApprovalResult{
		Applied: true,
		Status:  workflow.RunRunning,
	}, nil
}

func TestArtifactReviewAdvancesPipelineAfterDurableReview(t *testing.T) {
	artifacts, workflows := pipelineApprovalFixture(t)
	coordinator := NewApprovalCoordinator(artifacts, workflows)

	err := coordinator.ReviewArtifact(
		t.Context(),
		"feat-1",
		"analysis-1",
		delivery.DecisionApproved,
		"ship it",
		delivery.ReviewApprovalBinding{
			SubjectHash: "subject-1", ReviewRoundID: "round-1", GateResultID: "gate-1",
		},
		42,
	)
	if err != nil {
		t.Fatal(err)
	}
	if artifacts.reviewCalls != 1 || workflows.decideCalls != 1 {
		t.Fatalf("review calls=%d workflow calls=%d", artifacts.reviewCalls, workflows.decideCalls)
	}
	if workflows.lastRequest.NodeID != NodeApproveRequirementAnalysis ||
		workflows.lastRequest.Decision != workflow.ApprovalApproved ||
		workflows.lastRequest.Approver.UserID != 42 ||
		workflows.lastRequest.Comment != "ship it" {
		t.Fatalf("workflow approval = %+v", workflows.lastRequest)
	}
}

func TestArtifactReviewRetryConvergesAfterWorkflowFailure(t *testing.T) {
	artifacts, workflows := pipelineApprovalFixture(t)
	workflows.decideErrors = []error{errors.New("workflow store unavailable"), nil}
	coordinator := NewApprovalCoordinator(artifacts, workflows)
	binding := delivery.ReviewApprovalBinding{
		SubjectHash: "subject-1", ReviewRoundID: "round-1", GateResultID: "gate-1",
	}

	err := coordinator.ReviewArtifact(
		t.Context(), "feat-1", "analysis-1",
		delivery.DecisionApproved, "ship it", binding, 42,
	)
	if err == nil {
		t.Fatal("first review unexpectedly succeeded")
	}
	err = coordinator.ReviewArtifact(
		t.Context(), "feat-1", "analysis-1",
		delivery.DecisionApproved, "ship it", binding, 42,
	)
	if err != nil {
		t.Fatal(err)
	}
	if artifacts.reviewCalls != 2 || workflows.decideCalls != 2 {
		t.Fatalf("review calls=%d workflow calls=%d", artifacts.reviewCalls, workflows.decideCalls)
	}
}

func TestDirectPipelineApprovalRequiresMatchingArtifactReview(t *testing.T) {
	artifacts, workflows := pipelineApprovalFixture(t)
	coordinator := NewApprovalCoordinator(artifacts, workflows)
	request := workflow.ApprovalRequest{
		WorkflowRunID: workflows.run.ID,
		NodeID:        NodeApproveRequirementAnalysis,
		Decision:      workflow.ApprovalApproved,
		Approver:      agentapi.Actor{UserID: 42},
		Admin:         true,
	}
	if _, err := coordinator.DecideHumanApproval(t.Context(), request); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("approval error = %v, want conflict", err)
	}
	artifacts.artifact.Review = &delivery.ArtifactReview{
		ArtifactID: artifacts.artifact.ID, Decision: delivery.DecisionRejected,
		Comment: "not ready", Reviewer: 77,
	}
	if _, err := coordinator.DecideHumanApproval(t.Context(), request); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("mismatched decision error = %v, want conflict", err)
	}
	request.Decision = workflow.ApprovalRejected
	if _, err := coordinator.DecideHumanApproval(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if workflows.lastRequest.Approver.UserID != 77 ||
		workflows.lastRequest.Comment != "not ready" {
		t.Fatalf("canonical workflow approval = %+v", workflows.lastRequest)
	}
}

func TestPipelineApprovalRequiresArtifactCapability(t *testing.T) {
	_, workflows := pipelineApprovalFixture(t)
	coordinator := NewApprovalCoordinator(nil, workflows)
	_, err := coordinator.DecideHumanApproval(
		t.Context(),
		workflow.ApprovalRequest{
			WorkflowRunID: workflows.run.ID,
			NodeID:        NodeApproveRequirementAnalysis,
			Decision:      workflow.ApprovalApproved,
			Approver:      agentapi.Actor{UserID: 42},
			Admin:         true,
		},
	)
	if !errors.Is(err, workflow.ErrUnavailable) {
		t.Fatalf("approval error = %v, want unavailable", err)
	}
}

func TestLinkedArtifactReviewRequiresWorkflowCapability(t *testing.T) {
	artifacts, _ := pipelineApprovalFixture(t)
	coordinator := NewApprovalCoordinator(artifacts, nil)
	err := coordinator.ReviewArtifact(
		t.Context(),
		"feat-1",
		"analysis-1",
		delivery.DecisionApproved,
		"ship it",
		delivery.ReviewApprovalBinding{},
		42,
	)
	if !errors.Is(err, delivery.ErrUnavailable) {
		t.Fatalf("review error = %v, want unavailable", err)
	}
	if artifacts.reviewCalls != 0 {
		t.Fatalf("review calls=%d, want 0", artifacts.reviewCalls)
	}
}

func TestPipelineApprovalRejectsInconsistentGenerationBinding(t *testing.T) {
	artifacts, workflows := pipelineApprovalFixture(t)
	artifacts.generation.WorkflowNodeID = NodeGenerateTechnicalProposal
	coordinator := NewApprovalCoordinator(artifacts, workflows)

	err := coordinator.ReviewArtifact(
		t.Context(), "feat-1", "analysis-1",
		delivery.DecisionApproved, "ship it",
		delivery.ReviewApprovalBinding{
			SubjectHash: "subject-1", ReviewRoundID: "round-1", GateResultID: "gate-1",
		},
		42,
	)
	if !errors.Is(err, delivery.ErrConflict) {
		t.Fatalf("review error = %v, want conflict", err)
	}
	if artifacts.reviewCalls != 0 || workflows.decideCalls != 0 {
		t.Fatalf("review calls=%d workflow calls=%d", artifacts.reviewCalls, workflows.decideCalls)
	}
}

func pipelineApprovalFixture(
	t *testing.T,
) (*approvalArtifactService, *approvalWorkflowService) {
	t.Helper()
	artifact := delivery.Artifact{
		ID: "analysis-1", RequestID: "feat-1",
		Kind: delivery.KindRequirementAnalysis, Version: 1,
		ParentArtifactID: "requirement-1", ContentHash: "artifact-hash",
	}
	generation := delivery.GenerationRun{
		ID: "generation-1", RequestID: "feat-1",
		ArtifactKind:    delivery.KindRequirementAnalysis,
		WorkflowRunID:   "workflow_1",
		WorkflowNodeID:  NodeGenerateRequirementAnalysis,
		WorkflowAttempt: 1, ArtifactID: artifact.ID, Status: "succeeded",
	}
	requestPayload, err := json.Marshal(Request{
		FeatureID: "feat-1", ClientRequestID: "client-1",
		Repository: "repo", BaseRef: "HEAD", Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	statePayload, err := json.Marshal(State{
		FeatureID: "feat-1",
		CurrentArtifact: &ArtifactSummary{
			ID: artifact.ID, ParentArtifactID: artifact.ParentArtifactID,
			Kind: artifact.Kind, Version: artifact.Version, ContentHash: artifact.ContentHash,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifacts := &approvalArtifactService{artifact: artifact, generation: generation}
	workflows := &approvalWorkflowService{
		run: workflow.RunRecord{
			ID: generation.WorkflowRunID, WorkflowID: WorkflowID,
			WorkflowVersion: WorkflowVersion, Status: workflow.RunWaitingHuman,
		},
		handoffs: []workflow.Handoff{
			{
				WorkflowRunID:  generation.WorkflowRunID,
				ProducerNodeID: "workflow.input",
				Payload:        requestPayload,
			},
			{
				WorkflowRunID:  generation.WorkflowRunID,
				ProducerNodeID: NodeGenerateRequirementAnalysis,
				Payload:        statePayload,
			},
		},
		artifacts: artifacts,
	}
	return artifacts, workflows
}
