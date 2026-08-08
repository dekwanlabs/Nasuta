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
	transition, err := service.store.CancelWorkflow(ctx, runID, time.Now().UTC())
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
	prepared, err := prepareApprovalRequest(request)
	if err != nil {
		return ApprovalResult{}, err
	}
	state, err := service.store.LoadFullRunState(ctx, prepared.WorkflowRunID)
	if err != nil {
		return ApprovalResult{}, err
	}
	if !prepared.Admin {
		if prepared.Approver.TenantID != state.Run.ActorTenantID {
			return ApprovalResult{}, fmt.Errorf(
				"workflow approval tenant %q does not match run tenant %q: %w",
				prepared.Approver.TenantID, state.Run.ActorTenantID, ErrForbidden,
			)
		}
		if prepared.Approver.UserID != state.Run.ActorUserID {
			return ApprovalResult{}, ErrForbidden
		}
	}
	definition, err := service.catalog.Resolve(DefinitionRef{
		ID: state.Run.WorkflowID, Version: state.Run.WorkflowVersion,
	})
	if err != nil {
		return ApprovalResult{}, err
	}
	if definition.ContentHash != state.Run.WorkflowHash {
		return ApprovalResult{}, fmt.Errorf(
			"workflow run %q definition hash mismatch",
			state.Run.ID,
		)
	}
	metadata, err := graph(definition, orchestrator.schemas)
	if err != nil {
		return ApprovalResult{}, err
	}
	node, ok := metadata.nodes[prepared.NodeID]
	if !ok {
		return ApprovalResult{}, fmt.Errorf(
			"workflow run %q node %q not found: %w",
			state.Run.ID, prepared.NodeID, ErrNotFound,
		)
	}
	if node.Kind != NodeHumanApproval {
		return ApprovalResult{}, fmt.Errorf(
			"workflow run %q node %q does not require human approval: %w",
			state.Run.ID, node.ID, ErrConflict,
		)
	}
	if _, decided := state.Approvals[node.ID]; !decided {
		if state.Run.Status != RunWaitingHuman {
			return ApprovalResult{}, fmt.Errorf(
				"workflow run %q is %q, expected %q: %w",
				state.Run.ID, state.Run.Status, RunWaitingHuman, ErrConflict,
			)
		}
		nodeRun, exists := state.Nodes[node.ID]
		if !exists {
			return ApprovalResult{}, fmt.Errorf(
				"workflow run %q node %q has not started: %w",
				state.Run.ID, node.ID, ErrNotFound,
			)
		}
		if nodeRun.Kind != NodeHumanApproval {
			return ApprovalResult{}, fmt.Errorf(
				"workflow run %q node %q persisted kind %q does not require human approval: %w",
				state.Run.ID, node.ID, nodeRun.Kind, ErrConflict,
			)
		}
		if nodeRun.Status != RunWaitingHuman {
			return ApprovalResult{}, fmt.Errorf(
				"workflow run %q node %q is %q, expected %q: %w",
				state.Run.ID, node.ID, nodeRun.Status, RunWaitingHuman, ErrConflict,
			)
		}
	}
	approval := WorkflowApproval{
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
	transition, err := service.store.DecideHumanApproval(ctx, approval, approvedHandoff)
	if err != nil {
		return ApprovalResult{}, err
	}
	approvalResult := ApprovalResult{
		Approval: transition.Approval,
		Applied:  transition.Applied,
		Status:   transition.RunStatus,
	}
	if transition.RunStatus != RunRunning {
		return approvalResult, nil
	}
	resumed, resumeErr := service.Resume(ctx, state.Run.ID)
	if resumed.Status != "" {
		approvalResult.Status = resumed.Status
	}
	approvalResult.Result = resumed.Result
	if resumeErr != nil && !errors.Is(resumeErr, ErrHumanApprovalRequired) {
		return approvalResult, resumeErr
	}
	return approvalResult, nil
}

func prepareApprovalRequest(request ApprovalRequest) (ApprovalRequest, error) {
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
