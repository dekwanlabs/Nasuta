package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (service *Service) Cancel(
	ctx context.Context,
	runID string,
	userID int64,
	admin bool,
) (CancelTransition, error) {
	if _, err := service.GetRun(ctx, runID, userID, admin); err != nil {
		return CancelTransition{}, err
	}
	transition, err := service.store.CancelRun(ctx, runID, time.Now().UTC())
	if err != nil {
		return CancelTransition{}, err
	}
	if transition.Applied {
		service.cancelActive(runID)
	}
	return transition, nil
}

// DecideHumanApproval resumes only from durable facts fixed by the original Run.
func (service *Service) DecideHumanApproval(
	ctx context.Context,
	request ApprovalRequest,
) (ApprovalResult, error) {
	if service == nil || service.catalog == nil || service.store == nil {
		return ApprovalResult{}, ErrUnavailable
	}
	service.mu.RLock()
	orchestrator := service.orchestrator
	service.mu.RUnlock()
	if orchestrator == nil {
		return ApprovalResult{}, ErrUnavailable
	}
	prepared, err := prepareApproval(request)
	if err != nil {
		return ApprovalResult{}, err
	}
	state, definition, node, metadata, err := service.loadApprovalTarget(
		ctx, orchestrator, prepared,
	)
	if err != nil {
		return ApprovalResult{}, err
	}
	approval := Approval{
		WorkflowRunID:    state.Run.ID,
		NodeID:           node.ID,
		Decision:         prepared.Decision,
		ApproverUserID:   prepared.Approver.UserID,
		ApproverTenantID: prepared.Approver.TenantID,
		Comment:          prepared.Comment,
		DecidedAt:        time.Now().UTC(),
	}
	var approvedHandoff *Handoff
	if approval.Decision == ApprovalApproved {
		handoff, err := orchestrator.approvalHandoff(
			definition,
			state.Run.ID,
			node,
			predecessorHandoffs(
				node.ID,
				metadata.predecessors,
				state.NodeOutputs,
				state.Input,
			),
			approval.DecidedAt,
		)
		if err != nil {
			return ApprovalResult{}, fmt.Errorf(
				"approve workflow node %q/%q: %w",
				state.Run.ID, node.ID, err,
			)
		}
		approvedHandoff = &handoff
	}
	transition, err := service.store.DecideApproval(ctx, approval, approvedHandoff)
	if err != nil {
		return ApprovalResult{}, err
	}
	approvalResult := ApprovalResult{
		Approval: transition.Approval,
		Applied:  transition.Applied,
		Status:   transition.RunStatus,
	}
	return service.finalizeApprovalResume(ctx, approvalResult, state.Run.ID)
}

// loadApprovalTarget loads and validates the durable run, definition and
// approval node targeted by an approval decision.
func validateApprovalIdentity(prepared ApprovalRequest, state *RunState) error {
	if prepared.Admin {
		return nil
	}
	if prepared.Approver.TenantID != state.Run.ActorTenantID {
		return fmt.Errorf(
			"workflow approval tenant %q does not match run tenant %q: %w",
			prepared.Approver.TenantID, state.Run.ActorTenantID, ErrForbidden,
		)
	}
	if prepared.Approver.UserID != state.Run.ActorUserID {
		return ErrForbidden
	}
	return nil
}

func (service *Service) loadApprovalTarget(
	ctx context.Context,
	orchestrator *Orchestrator,
	prepared ApprovalRequest,
) (*RunState, Definition, NodeDefinition, graphMetadata, error) {
	state, err := service.store.LoadFullRunState(ctx, prepared.WorkflowRunID)
	if err != nil {
		return nil, Definition{}, NodeDefinition{}, graphMetadata{}, err
	}
	if err := validateApprovalIdentity(prepared, state); err != nil {
		return nil, Definition{}, NodeDefinition{}, graphMetadata{}, err
	}
	definition, err := service.catalog.Resolve(DefinitionRef{
		ID: state.Run.WorkflowID, Version: state.Run.WorkflowVersion,
	})
	if err != nil {
		return nil, Definition{}, NodeDefinition{}, graphMetadata{}, err
	}
	if definition.ContentHash != state.Run.WorkflowHash {
		return nil, Definition{}, NodeDefinition{}, graphMetadata{}, fmt.Errorf(
			"workflow run %q definition hash mismatch",
			state.Run.ID,
		)
	}
	metadata, err := graph(definition, orchestrator.schemas)
	if err != nil {
		return nil, Definition{}, NodeDefinition{}, graphMetadata{}, err
	}
	node, ok := metadata.nodes[prepared.NodeID]
	if !ok {
		return nil, Definition{}, NodeDefinition{}, graphMetadata{}, fmt.Errorf(
			"workflow run %q node %q not found: %w",
			state.Run.ID, prepared.NodeID, ErrNotFound,
		)
	}
	if node.Kind != NodeHumanApproval {
		return nil, Definition{}, NodeDefinition{}, graphMetadata{}, fmt.Errorf(
			"workflow run %q node %q does not require human approval: %w",
			state.Run.ID, node.ID, ErrConflict,
		)
	}
	if _, decided := state.Approvals[node.ID]; !decided {
		if state.Run.Status != RunWaitingHuman {
			return nil, Definition{}, NodeDefinition{}, graphMetadata{}, fmt.Errorf(
				"workflow run %q is %q, expected %q: %w",
				state.Run.ID, state.Run.Status, RunWaitingHuman, ErrConflict,
			)
		}
		nodeRun, exists := state.Nodes[node.ID]
		if !exists {
			return nil, Definition{}, NodeDefinition{}, graphMetadata{}, fmt.Errorf(
				"workflow run %q node %q has not started: %w",
				state.Run.ID, node.ID, ErrNotFound,
			)
		}
		if nodeRun.Kind != NodeHumanApproval {
			return nil, Definition{}, NodeDefinition{}, graphMetadata{}, fmt.Errorf(
				"workflow run %q node %q persisted kind %q does not require human approval: %w",
				state.Run.ID, node.ID, nodeRun.Kind, ErrConflict,
			)
		}
		if nodeRun.Status != RunWaitingHuman {
			return nil, Definition{}, NodeDefinition{}, graphMetadata{}, fmt.Errorf(
				"workflow run %q node %q is %q, expected %q: %w",
				state.Run.ID, node.ID, nodeRun.Status, RunWaitingHuman, ErrConflict,
			)
		}
	}
	return state, definition, node, metadata, nil
}

// finalizeApprovalResume resumes a running workflow after an approval decision
// and folds the resumed result into the returned approval result.
func (service *Service) finalizeApprovalResume(
	ctx context.Context,
	approvalResult ApprovalResult,
	runID string,
) (ApprovalResult, error) {
	if approvalResult.Status != RunRunning {
		return approvalResult, nil
	}
	resumed, resumeErr := service.Resume(ctx, runID)
	if resumed.Status != "" {
		approvalResult.Status = resumed.Status
	}
	approvalResult.Result = resumed.Result
	if resumeErr != nil && !errors.Is(resumeErr, ErrHumanApprovalRequired) {
		return approvalResult, resumeErr
	}
	return approvalResult, nil
}

func prepareApproval(request ApprovalRequest) (ApprovalRequest, error) {
	prepared := request
	prepared.WorkflowRunID = strings.TrimSpace(prepared.WorkflowRunID)
	prepared.NodeID = strings.TrimSpace(prepared.NodeID)
	prepared.Comment = strings.TrimSpace(prepared.Comment)
	prepared.Approver.TenantID = strings.TrimSpace(prepared.Approver.TenantID)
	prepared.Decision = ApprovalDecision(strings.TrimSpace(string(prepared.Decision)))
	if !canonicalID.MatchString(prepared.WorkflowRunID) {
		return ApprovalRequest{}, fmt.Errorf(
			"workflow run id %q is not canonical: %w",
			request.WorkflowRunID, ErrInvalid,
		)
	}
	if !canonicalID.MatchString(prepared.NodeID) {
		return ApprovalRequest{}, fmt.Errorf(
			"workflow node id %q is not canonical: %w",
			request.NodeID, ErrInvalid,
		)
	}
	if prepared.Approver.UserID <= 0 {
		return ApprovalRequest{}, fmt.Errorf("workflow approver identity is required: %w", ErrInvalid)
	}
	if prepared.Decision != ApprovalApproved && prepared.Decision != ApprovalRejected {
		return ApprovalRequest{}, fmt.Errorf(
			"workflow approval decision %q is invalid: %w",
			prepared.Decision, ErrInvalid,
		)
	}
	return prepared, nil
}
