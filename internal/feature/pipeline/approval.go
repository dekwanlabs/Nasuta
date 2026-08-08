package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
)

const maxPipelineHandoffs = 11

type artifactApprovalService interface {
	ReviewArtifact(
		context.Context,
		string,
		string,
		delivery.ReviewDecision,
		string,
		delivery.ReviewApprovalBinding,
		int64,
	) error
	GetArtifact(context.Context, string, string, int64, bool) (*delivery.Artifact, error)
	GetGenerationForArtifact(context.Context, string, string, int64, bool) (*delivery.GenerationRun, error)
}

type workflowApprovalService interface {
	GetRun(context.Context, string, int64, bool) (*workflow.WorkflowRunRecord, error)
	ListHandoffs(
		context.Context,
		string,
		workflow.HandoffCursor,
		int,
		int64,
		bool,
	) ([]workflow.Handoff, error)
	DecideHumanApproval(
		context.Context,
		workflow.ApprovalRequest,
	) (workflow.ApprovalResult, error)
}

// ApprovalCoordinator keeps the domain review and workflow checkpoint convergent.
type ApprovalCoordinator struct {
	artifacts artifactApprovalService
	workflows workflowApprovalService
}

func NewApprovalCoordinator(
	artifacts artifactApprovalService,
	workflows workflowApprovalService,
) *ApprovalCoordinator {
	return &ApprovalCoordinator{artifacts: artifacts, workflows: workflows}
}

func (coordinator *ApprovalCoordinator) ReviewArtifact(
	ctx context.Context,
	featureID string,
	artifactID string,
	decision delivery.ReviewDecision,
	comment string,
	reviewBinding delivery.ReviewApprovalBinding,
	reviewerID int64,
) error {
	if coordinator == nil || coordinator.artifacts == nil {
		return delivery.ErrUnavailable
	}
	binding, linked, err := coordinator.bindingForArtifact(
		ctx,
		featureID,
		artifactID,
		reviewerID,
		true,
	)
	if err != nil {
		return err
	}
	if err := coordinator.artifacts.ReviewArtifact(
		ctx,
		featureID,
		artifactID,
		decision,
		comment,
		reviewBinding,
		reviewerID,
	); err != nil {
		return err
	}
	if !linked {
		return nil
	}
	_, err = coordinator.DecideHumanApproval(ctx, workflow.ApprovalRequest{
		WorkflowRunID: binding.run.ID,
		NodeID:        binding.stage.approvalNode,
		Decision:      workflowDecision(decision),
		Approver:      agentapi.Actor{UserID: reviewerID},
		Admin:         true,
		Comment:       comment,
	})
	if err != nil {
		return fmt.Errorf(
			"advance feature pipeline approval %q/%q: %w",
			binding.run.ID,
			binding.stage.approvalNode,
			err,
		)
	}
	return nil
}

func (coordinator *ApprovalCoordinator) DecideHumanApproval(
	ctx context.Context,
	request workflow.ApprovalRequest,
) (workflow.ApprovalResult, error) {
	result, err := coordinator.decideHumanApproval(ctx, request)
	if err != nil {
		return workflow.ApprovalResult{}, workflowApprovalError(err)
	}
	return result, nil
}

func (coordinator *ApprovalCoordinator) decideHumanApproval(
	ctx context.Context,
	request workflow.ApprovalRequest,
) (workflow.ApprovalResult, error) {
	if coordinator == nil || coordinator.workflows == nil {
		return workflow.ApprovalResult{}, workflow.ErrUnavailable
	}
	run, err := coordinator.workflows.GetRun(
		ctx,
		request.WorkflowRunID,
		request.Approver.UserID,
		request.Admin,
	)
	if err != nil {
		return workflow.ApprovalResult{}, err
	}
	if run.WorkflowID != WorkflowID {
		return coordinator.workflows.DecideHumanApproval(ctx, request)
	}
	if run.WorkflowVersion != WorkflowVersion {
		return workflow.ApprovalResult{}, fmt.Errorf(
			"feature pipeline run %q uses unsupported version %d: %w",
			run.ID,
			run.WorkflowVersion,
			delivery.ErrConflict,
		)
	}
	if coordinator.artifacts == nil {
		return workflow.ApprovalResult{}, delivery.ErrUnavailable
	}
	if !request.Admin {
		return workflow.ApprovalResult{}, delivery.ErrForbidden
	}
	stage, err := approvalStageForNode(request.NodeID)
	if err != nil {
		return workflow.ApprovalResult{}, err
	}
	binding, err := coordinator.loadBinding(
		ctx,
		run,
		stage,
		request.Approver.UserID,
		true,
	)
	if err != nil {
		return workflow.ApprovalResult{}, err
	}
	if binding.artifact.Review == nil {
		return workflow.ApprovalResult{}, fmt.Errorf(
			"artifact %q must be reviewed before pipeline approval: %w",
			binding.artifact.ID,
			delivery.ErrConflict,
		)
	}
	decision := workflowDecision(binding.artifact.Review.Decision)
	if decision != request.Decision {
		return workflow.ApprovalResult{}, fmt.Errorf(
			"workflow decision %q does not match artifact review %q: %w",
			request.Decision,
			binding.artifact.Review.Decision,
			delivery.ErrConflict,
		)
	}
	request.Approver = agentapi.Actor{
		UserID:   binding.artifact.Review.Reviewer,
		TenantID: run.ActorTenantID,
	}
	request.Comment = binding.artifact.Review.Comment
	return coordinator.workflows.DecideHumanApproval(ctx, request)
}

type pipelineApprovalBinding struct {
	run        *workflow.WorkflowRunRecord
	stage      pipelineApprovalStage
	artifact   *delivery.Artifact
	generation *delivery.GenerationRun
}

type pipelineApprovalStage struct {
	kind           delivery.ArtifactKind
	generationNode string
	approvalNode   string
}

func (coordinator *ApprovalCoordinator) bindingForArtifact(
	ctx context.Context,
	featureID string,
	artifactID string,
	userID int64,
	admin bool,
) (pipelineApprovalBinding, bool, error) {
	generation, err := coordinator.artifacts.GetGenerationForArtifact(
		ctx,
		featureID,
		artifactID,
		userID,
		admin,
	)
	if errors.Is(err, delivery.ErrNotFound) {
		return pipelineApprovalBinding{}, false, nil
	}
	if err != nil {
		return pipelineApprovalBinding{}, false, err
	}
	if generation.WorkflowRunID == "" {
		return pipelineApprovalBinding{}, false, nil
	}
	if coordinator.workflows == nil {
		return pipelineApprovalBinding{}, false, delivery.ErrUnavailable
	}
	stage, err := approvalStageForKind(generation.ArtifactKind)
	if err != nil {
		return pipelineApprovalBinding{}, false, err
	}
	if generation.WorkflowNodeID != stage.generationNode ||
		generation.RequestID != featureID ||
		generation.ArtifactID != artifactID {
		return pipelineApprovalBinding{}, false, fmt.Errorf(
			"artifact %q pipeline generation binding is inconsistent: %w",
			artifactID,
			delivery.ErrConflict,
		)
	}
	run, err := coordinator.workflows.GetRun(
		ctx,
		generation.WorkflowRunID,
		userID,
		admin,
	)
	if err != nil {
		return pipelineApprovalBinding{}, false, err
	}
	if run.WorkflowID != WorkflowID || run.WorkflowVersion != WorkflowVersion {
		return pipelineApprovalBinding{}, false, fmt.Errorf(
			"artifact %q is bound to workflow %q version %d: %w",
			artifactID,
			run.WorkflowID,
			run.WorkflowVersion,
			delivery.ErrConflict,
		)
	}
	binding, err := coordinator.loadBinding(ctx, run, stage, userID, admin)
	if err != nil {
		return pipelineApprovalBinding{}, false, err
	}
	if binding.artifact.ID != artifactID ||
		binding.generation.ID != generation.ID {
		return pipelineApprovalBinding{}, false, fmt.Errorf(
			"artifact %q does not match pipeline checkpoint: %w",
			artifactID,
			delivery.ErrConflict,
		)
	}
	return binding, true, nil
}

func (coordinator *ApprovalCoordinator) loadBinding(
	ctx context.Context,
	run *workflow.WorkflowRunRecord,
	stage pipelineApprovalStage,
	userID int64,
	admin bool,
) (pipelineApprovalBinding, error) {
	if coordinator.artifacts == nil || coordinator.workflows == nil {
		return pipelineApprovalBinding{}, delivery.ErrUnavailable
	}
	handoffs, err := coordinator.workflows.ListHandoffs(
		ctx,
		run.ID,
		workflow.HandoffCursor{},
		maxPipelineHandoffs,
		userID,
		admin,
	)
	if err != nil {
		return pipelineApprovalBinding{}, err
	}
	var input *workflow.Handoff
	var generated *workflow.Handoff
	for index := range handoffs {
		handoff := &handoffs[index]
		switch handoff.ProducerNodeID {
		case "workflow.input":
			if input != nil {
				return pipelineApprovalBinding{}, fmt.Errorf(
					"feature pipeline run %q has duplicate input handoffs: %w",
					run.ID,
					delivery.ErrConflict,
				)
			}
			input = handoff
		case stage.generationNode:
			if generated != nil {
				return pipelineApprovalBinding{}, fmt.Errorf(
					"feature pipeline run %q has duplicate handoffs for %q: %w",
					run.ID,
					stage.generationNode,
					delivery.ErrConflict,
				)
			}
			generated = handoff
		}
	}
	if input == nil || generated == nil {
		return pipelineApprovalBinding{}, fmt.Errorf(
			"feature pipeline run %q is missing approval handoffs: %w",
			run.ID,
			delivery.ErrConflict,
		)
	}
	var pipelineRequest Request
	if err := json.Unmarshal(input.Payload, &pipelineRequest); err != nil {
		return pipelineApprovalBinding{}, fmt.Errorf(
			"decode feature pipeline input for run %q: %w",
			run.ID,
			err,
		)
	}
	state, err := decodeState(generated.Payload)
	if err != nil {
		return pipelineApprovalBinding{}, err
	}
	if state.FeatureID != pipelineRequest.FeatureID ||
		state.CurrentArtifact.Kind != stage.kind {
		return pipelineApprovalBinding{}, fmt.Errorf(
			"feature pipeline run %q approval handoff is inconsistent: %w",
			run.ID,
			delivery.ErrConflict,
		)
	}
	artifact, err := coordinator.artifacts.GetArtifact(
		ctx,
		pipelineRequest.FeatureID,
		state.CurrentArtifact.ID,
		userID,
		admin,
	)
	if err != nil {
		return pipelineApprovalBinding{}, err
	}
	if artifact.Kind != stage.kind ||
		artifact.Version != state.CurrentArtifact.Version ||
		artifact.ParentArtifactID != state.CurrentArtifact.ParentArtifactID ||
		artifact.ContentHash != state.CurrentArtifact.ContentHash {
		return pipelineApprovalBinding{}, fmt.Errorf(
			"artifact %q does not match pipeline handoff: %w",
			artifact.ID,
			delivery.ErrConflict,
		)
	}
	generation, err := coordinator.artifacts.GetGenerationForArtifact(
		ctx,
		pipelineRequest.FeatureID,
		artifact.ID,
		userID,
		admin,
	)
	if err != nil {
		return pipelineApprovalBinding{}, err
	}
	if generation.WorkflowRunID != run.ID ||
		generation.WorkflowNodeID != stage.generationNode ||
		generation.ArtifactKind != stage.kind {
		return pipelineApprovalBinding{}, fmt.Errorf(
			"artifact %q generation does not match pipeline run %q: %w",
			artifact.ID,
			run.ID,
			delivery.ErrConflict,
		)
	}
	return pipelineApprovalBinding{
		run: run, stage: stage, artifact: artifact, generation: generation,
	}, nil
}

func approvalStageForKind(
	kind delivery.ArtifactKind,
) (pipelineApprovalStage, error) {
	switch kind {
	case delivery.KindRequirementAnalysis:
		return pipelineApprovalStage{
			kind: kind, generationNode: NodeGenerateRequirementAnalysis,
			approvalNode: NodeApproveRequirementAnalysis,
		}, nil
	case delivery.KindTechnicalProposal:
		return pipelineApprovalStage{
			kind: kind, generationNode: NodeGenerateTechnicalProposal,
			approvalNode: NodeApproveTechnicalProposal,
		}, nil
	case delivery.KindSystemDesign:
		return pipelineApprovalStage{
			kind: kind, generationNode: NodeGenerateSystemDesign,
			approvalNode: NodeApproveSystemDesign,
		}, nil
	case delivery.KindImplementationPlan:
		return pipelineApprovalStage{
			kind: kind, generationNode: NodeGenerateImplementationPlan,
			approvalNode: NodeApproveImplementationPlan,
		}, nil
	default:
		return pipelineApprovalStage{}, fmt.Errorf(
			"artifact kind %q is not a pipeline approval stage: %w",
			kind,
			delivery.ErrConflict,
		)
	}
}

func approvalStageForNode(nodeID string) (pipelineApprovalStage, error) {
	for _, kind := range []delivery.ArtifactKind{
		delivery.KindRequirementAnalysis,
		delivery.KindTechnicalProposal,
		delivery.KindSystemDesign,
		delivery.KindImplementationPlan,
	} {
		stage, _ := approvalStageForKind(kind)
		if stage.approvalNode == nodeID {
			return stage, nil
		}
	}
	return pipelineApprovalStage{}, fmt.Errorf(
		"node %q is not a feature pipeline approval checkpoint: %w",
		nodeID,
		delivery.ErrConflict,
	)
}

func workflowApprovalError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, workflow.ErrInvalid),
		errors.Is(err, workflow.ErrNotFound),
		errors.Is(err, workflow.ErrForbidden),
		errors.Is(err, workflow.ErrConflict),
		errors.Is(err, workflow.ErrUnavailable):
		return err
	case errors.Is(err, delivery.ErrInvalid):
		return fmt.Errorf("%v: %w", err, workflow.ErrInvalid)
	case errors.Is(err, delivery.ErrNotFound):
		return fmt.Errorf("%v: %w", err, workflow.ErrNotFound)
	case errors.Is(err, delivery.ErrForbidden):
		return fmt.Errorf("%v: %w", err, workflow.ErrForbidden)
	case errors.Is(err, delivery.ErrConflict):
		return fmt.Errorf("%v: %w", err, workflow.ErrConflict)
	case errors.Is(err, delivery.ErrUnavailable):
		return fmt.Errorf("%v: %w", err, workflow.ErrUnavailable)
	default:
		return err
	}
}

func workflowDecision(
	decision delivery.ReviewDecision,
) workflow.ApprovalDecision {
	if decision == delivery.DecisionApproved {
		return workflow.ApprovalApproved
	}
	return workflow.ApprovalRejected
}

var _ interface {
	ReviewArtifact(
		context.Context,
		string,
		string,
		delivery.ReviewDecision,
		string,
		delivery.ReviewApprovalBinding,
		int64,
	) error
	DecideHumanApproval(
		context.Context,
		workflow.ApprovalRequest,
	) (workflow.ApprovalResult, error)
} = (*ApprovalCoordinator)(nil)
