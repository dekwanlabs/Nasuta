package featurepipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
)

type approvalArtifactService struct {
	artifact    featuredelivery.Artifact
	generation  featuredelivery.GenerationRun
	reviewCalls int
}

func (service *approvalArtifactService) ReviewArtifact(
	_ context.Context,
	featureID string,
	artifactID string,
	decision featuredelivery.ReviewDecision,
	comment string,
	binding featuredelivery.ReviewApprovalBinding,
	reviewerID int64,
) error {
	if featureID != service.artifact.RequestID || artifactID != service.artifact.ID {
		return featuredelivery.ErrNotFound
	}
	service.reviewCalls++
	review := &featuredelivery.ArtifactReview{
		ArtifactID: artifactID, SubjectHash: binding.SubjectHash,
		ReviewRoundID: binding.ReviewRoundID, GateResultID: binding.GateResultID,
		Decision: decision, Comment: comment, Reviewer: reviewerID,
	}
	if service.artifact.Review != nil {
		existing := service.artifact.Review
		if existing.Decision != review.Decision ||
			existing.Comment != review.Comment ||
			existing.Reviewer != review.Reviewer {
			return featuredelivery.ErrConflict
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
) (*featuredelivery.Artifact, error) {
	if featureID != service.artifact.RequestID || artifactID != service.artifact.ID {
		return nil, featuredelivery.ErrNotFound
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
) (*featuredelivery.GenerationRun, error) {
	if featureID != service.generation.RequestID ||
		artifactID != service.generation.ArtifactID {
		return nil, featuredelivery.ErrNotFound
	}
	run := service.generation
	return &run, nil
}

type approvalWorkflowService struct {
	run          agentworkflow.WorkflowRunRecord
	handoffs     []agentworkflow.Handoff
	artifacts    *approvalArtifactService
	decideCalls  int
	decideErrors []error
	lastRequest  agentworkflow.ApprovalRequest
}

func (service *approvalWorkflowService) GetRun(
	_ context.Context,
	runID string,
	_ int64,
	_ bool,
) (*agentworkflow.WorkflowRunRecord, error) {
	if runID != service.run.ID {
		return nil, agentworkflow.ErrNotFound
	}
	run := service.run
	return &run, nil
}

func (service *approvalWorkflowService) ListHandoffs(
	_ context.Context,
	runID string,
	_ agentworkflow.HandoffCursor,
	limit int,
	_ int64,
	_ bool,
) ([]agentworkflow.Handoff, error) {
	if runID != service.run.ID {
		return nil, agentworkflow.ErrNotFound
	}
	if limit != maxPipelineHandoffs {
		return nil, errors.New("pipeline handoff read was not bounded")
	}
	return append([]agentworkflow.Handoff(nil), service.handoffs...), nil
}

func (service *approvalWorkflowService) DecideHumanApproval(
	_ context.Context,
	request agentworkflow.ApprovalRequest,
) (agentworkflow.ApprovalResult, error) {
	service.decideCalls++
	service.lastRequest = request
	if service.artifacts != nil && service.artifacts.artifact.Review == nil {
		return agentworkflow.ApprovalResult{}, errors.New("workflow approval preceded artifact review")
	}
	if len(service.decideErrors) > 0 {
		err := service.decideErrors[0]
		service.decideErrors = service.decideErrors[1:]
		if err != nil {
			return agentworkflow.ApprovalResult{}, err
		}
	}
	return agentworkflow.ApprovalResult{
		Applied: true,
		Status:  agentworkflow.RunRunning,
	}, nil
}

func TestArtifactReviewAdvancesPipelineAfterDurableReview(t *testing.T) {
	artifacts, workflows := pipelineApprovalFixture(t)
	coordinator := NewApprovalCoordinator(artifacts, workflows)

	err := coordinator.ReviewArtifact(
		t.Context(),
		"feat-1",
		"analysis-1",
		featuredelivery.DecisionApproved,
		"ship it",
		featuredelivery.ReviewApprovalBinding{
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
		workflows.lastRequest.Decision != agentworkflow.ApprovalApproved ||
		workflows.lastRequest.Approver.UserID != 42 ||
		workflows.lastRequest.Comment != "ship it" {
		t.Fatalf("workflow approval = %+v", workflows.lastRequest)
	}
}

func TestArtifactReviewRetryConvergesAfterWorkflowFailure(t *testing.T) {
	artifacts, workflows := pipelineApprovalFixture(t)
	workflows.decideErrors = []error{errors.New("workflow store unavailable"), nil}
	coordinator := NewApprovalCoordinator(artifacts, workflows)
	binding := featuredelivery.ReviewApprovalBinding{
		SubjectHash: "subject-1", ReviewRoundID: "round-1", GateResultID: "gate-1",
	}

	err := coordinator.ReviewArtifact(
		t.Context(), "feat-1", "analysis-1",
		featuredelivery.DecisionApproved, "ship it", binding, 42,
	)
	if err == nil {
		t.Fatal("first review unexpectedly succeeded")
	}
	err = coordinator.ReviewArtifact(
		t.Context(), "feat-1", "analysis-1",
		featuredelivery.DecisionApproved, "ship it", binding, 42,
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
	request := agentworkflow.ApprovalRequest{
		WorkflowRunID: workflows.run.ID,
		NodeID:        NodeApproveRequirementAnalysis,
		Decision:      agentworkflow.ApprovalApproved,
		Approver:      agentapi.Actor{UserID: 42},
		Admin:         true,
	}
	if _, err := coordinator.DecideHumanApproval(t.Context(), request); !errors.Is(err, agentworkflow.ErrConflict) {
		t.Fatalf("approval error = %v, want conflict", err)
	}
	artifacts.artifact.Review = &featuredelivery.ArtifactReview{
		ArtifactID: artifacts.artifact.ID, Decision: featuredelivery.DecisionRejected,
		Comment: "not ready", Reviewer: 77,
	}
	if _, err := coordinator.DecideHumanApproval(t.Context(), request); !errors.Is(err, agentworkflow.ErrConflict) {
		t.Fatalf("mismatched decision error = %v, want conflict", err)
	}
	request.Decision = agentworkflow.ApprovalRejected
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
		agentworkflow.ApprovalRequest{
			WorkflowRunID: workflows.run.ID,
			NodeID:        NodeApproveRequirementAnalysis,
			Decision:      agentworkflow.ApprovalApproved,
			Approver:      agentapi.Actor{UserID: 42},
			Admin:         true,
		},
	)
	if !errors.Is(err, agentworkflow.ErrUnavailable) {
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
		featuredelivery.DecisionApproved,
		"ship it",
		featuredelivery.ReviewApprovalBinding{},
		42,
	)
	if !errors.Is(err, featuredelivery.ErrUnavailable) {
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
		featuredelivery.DecisionApproved, "ship it",
		featuredelivery.ReviewApprovalBinding{
			SubjectHash: "subject-1", ReviewRoundID: "round-1", GateResultID: "gate-1",
		},
		42,
	)
	if !errors.Is(err, featuredelivery.ErrConflict) {
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
	artifact := featuredelivery.Artifact{
		ID: "analysis-1", RequestID: "feat-1",
		Kind: featuredelivery.KindRequirementAnalysis, Version: 1,
		ParentArtifactID: "requirement-1", ContentHash: "artifact-hash",
	}
	generation := featuredelivery.GenerationRun{
		ID: "generation-1", RequestID: "feat-1",
		ArtifactKind:    featuredelivery.KindRequirementAnalysis,
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
		run: agentworkflow.WorkflowRunRecord{
			ID: generation.WorkflowRunID, WorkflowID: WorkflowID,
			WorkflowVersion: WorkflowVersion, Status: agentworkflow.RunWaitingHuman,
		},
		handoffs: []agentworkflow.Handoff{
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
