package approval

import (
	"context"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/incident"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/writeaction"
	"github.com/dekwanlabs/nasuta/tool"
)

type Status = ActionStatus

const (
	StatusPending  = ActionPending
	StatusApproved = ActionApproved
	StatusRejected = ActionRejected
	StatusDone     = ActionDone
	StatusFailed   = ActionFailed
	StatusExpired  = ActionExpired
)

// IncidentFixer is the narrow incident mutation port used after approval.
type IncidentFixer interface {
	StartFix(context.Context, string, incident.FixRequest) (*incident.Incident, error)
	CommitFix(context.Context, string, incident.ConfirmRequest) (*incident.Incident, error)
}

// Propose persists a request produced by the platform-owned write catalog.
func (svc *Service) Propose(ctx context.Context, proposal writeaction.Proposal) (tool.Result, error) {
	user := auth.UserFromContext(ctx)
	if user == nil || !user.IsAdmin {
		return tool.Result{}, fmt.Errorf("write action requires an authenticated administrator")
	}
	if proposal.IncidentID == "" {
		return tool.Result{}, fmt.Errorf("incident_id is required")
	}
	action, err := svc.Create(PendingAction{
		Tool: string(proposal.ToolID), IncidentID: proposal.IncidentID,
		Args: map[string]any(proposal.Arguments), Rationale: proposal.Rationale,
		Impact: proposal.Impact, RequestedBy: user.ID,
	})
	if err != nil {
		return tool.Result{}, err
	}
	return tool.Result{Content: fmt.Sprintf(
		"Pending approval request #%s(%s) created. Awaiting human approval.",
		action.ID, proposal.ToolID,
	)}, nil
}

// Approve marks a pending action approved by approver and executes it.
func (svc *Service) Approve(ctx context.Context, id string, approver int64) (*PendingAction, error) {
	if approver <= 0 {
		return nil, fmt.Errorf("approver is required")
	}
	action, err := svc.Get(id)
	if err != nil {
		return nil, err
	}
	if action.Status != ActionPending {
		return nil, fmt.Errorf("action %s is not pending (status=%s)", id, action.Status)
	}
	if svc.incidents == nil {
		return nil, fmt.Errorf("incident manager not configured; cannot execute write")
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result, execErr := svc.execute(ctx, action)
	status := ActionDone
	if execErr != nil {
		status = ActionFailed
		result = map[string]any{"error": execErr.Error()}
	}
	_, err = svc.db.Exec(
		`UPDATE pending_actions SET status=?, approver=?, result_json=?, decided_at=? WHERE id=?`,
		status, approver, mustJSON(result), store.DatabaseTime(now), id)
	if err != nil {
		return nil, err
	}
	action.Status = status
	action.Approver = approver
	action.Result = result
	action.DecidedAt = now
	return action, execErr
}

// Reject marks a pending action rejected with a reason.
func (svc *Service) Reject(id string, approver int64, reason string) error {
	if approver <= 0 {
		return fmt.Errorf("approver is required")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := svc.db.Exec(
		`UPDATE pending_actions SET status=?, approver=?, result_json=?, decided_at=? WHERE id=?`,
		ActionRejected, approver, mustJSON(map[string]any{"reason": reason}), store.DatabaseTime(now), id)
	return err
}

func (svc *Service) execute(ctx context.Context, action *PendingAction) (any, error) {
	switch action.Tool {
	case "propose_branch":
		assignee, _ := action.Args["assignee"].(string)
		branchName, _ := action.Args["branch_name"].(string)
		out, err := svc.incidents.StartFix(ctx, action.IncidentID, incident.FixRequest{
			Assignee: assignee, BranchName: branchName,
		})
		if err != nil {
			return nil, err
		}
		return map[string]any{"incident_id": out.ID, "status": out.Status, "branches": out.FixBranches}, nil
	case "propose_commit":
		branchName, _ := action.Args["branch_name"].(string)
		out, err := svc.incidents.CommitFix(ctx, action.IncidentID, incident.ConfirmRequest{BranchName: branchName})
		if err != nil {
			return nil, err
		}
		return map[string]any{"incident_id": out.ID, "status": out.Status, "branches": out.FixBranches}, nil
	default:
		return nil, fmt.Errorf("unknown write tool %q", action.Tool)
	}
}
