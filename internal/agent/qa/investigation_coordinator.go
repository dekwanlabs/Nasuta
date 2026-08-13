package qa

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

type sessionTurnStore interface {
	EnsureSession(string, int64, string) error
	AppendTurn(string, string, int64, []llm.Message) (int, error)
}

// InvestigationCoordinator converges durable Workflow facts into QA Parents.
type InvestigationCoordinator struct {
	investigation InvestigationRunner
	scenarios     ScenarioLifecycle
	parentRuns    ParentRunReader
	sessions      sessionTurnStore
}

// NewInvestigationCoordinator binds the durable stores used by recovery and control.
func NewInvestigationCoordinator(
	investigation InvestigationRunner,
	scenarios ScenarioLifecycle,
	parentRuns ParentRunReader,
	sessions *memory.SessionStore,
) *InvestigationCoordinator {
	var sessionTurns sessionTurnStore
	if sessions != nil {
		sessionTurns = sessions
	}
	return &InvestigationCoordinator{
		investigation: investigation,
		scenarios:     scenarios,
		parentRuns:    parentRuns,
		sessions:      sessionTurns,
	}
}

// Await joins local execution when present before converging durable terminal facts.
func (coordinator *InvestigationCoordinator) Await(
	ctx context.Context,
	parentRunID string,
	workflowRunID string,
) error {
	parent, err := coordinator.loadParent(parentRunID)
	if err != nil {
		return err
	}
	if parent.WorkflowRunID != workflowRunID {
		return fmt.Errorf(
			"QA parent run %q references workflow %q, not %q",
			parentRunID,
			parent.WorkflowRunID,
			workflowRunID,
		)
	}
	investigation, err := coordinator.investigationRunner()
	if err != nil {
		return err
	}
	terminal, err := investigation.AwaitTerminal(ctx, workflowRunID)
	if err != nil {
		return fmt.Errorf("await QA investigation workflow %q: %w", workflowRunID, err)
	}
	return coordinator.converge(ctx, parent, terminal)
}

// ReconcileInvestigation converges one Parent from durable Workflow facts.
func (coordinator *InvestigationCoordinator) ReconcileInvestigation(
	ctx context.Context,
	parentRunID string,
) error {
	parent, err := coordinator.loadParent(parentRunID)
	if err != nil {
		return err
	}
	investigation, err := coordinator.investigationRunner()
	if err != nil {
		return err
	}
	terminal, err := investigation.LoadTerminal(ctx, parent.WorkflowRunID)
	if err != nil {
		return fmt.Errorf(
			"load QA investigation workflow %q terminal result: %w",
			parent.WorkflowRunID,
			err,
		)
	}
	return coordinator.converge(ctx, parent, terminal)
}

// CancelInvestigation propagates an owned Parent cancellation to its Workflow.
func (coordinator *InvestigationCoordinator) CancelInvestigation(
	ctx context.Context,
	parentRunID string,
	userID int64,
) error {
	parent, err := coordinator.loadParent(parentRunID)
	if err != nil {
		return err
	}
	if parent.UserID != userID {
		return fmt.Errorf("QA parent run %q is not owned by user %d", parentRunID, userID)
	}
	investigation, err := coordinator.investigationRunner()
	if err != nil {
		return err
	}
	if err := investigation.Cancel(ctx, parent.WorkflowRunID, userID); err != nil {
		return fmt.Errorf(
			"cancel QA investigation workflow %q: %w",
			parent.WorkflowRunID,
			err,
		)
	}
	return coordinator.ReconcileInvestigation(context.WithoutCancel(ctx), parentRunID)
}

func (coordinator *InvestigationCoordinator) investigationRunner() (InvestigationRunner, error) {
	if coordinator == nil || coordinator.investigation == nil {
		return nil, fmt.Errorf("QA investigation workflow service is unavailable")
	}
	return coordinator.investigation, nil
}

func (coordinator *InvestigationCoordinator) loadParent(
	parentRunID string,
) (QAParentRecord, error) {
	if coordinator == nil || coordinator.parentRuns == nil {
		return QAParentRecord{}, fmt.Errorf("QA parent run store is unavailable")
	}
	parent, err := coordinator.parentRuns.GetQAParent(parentRunID)
	if err != nil {
		return QAParentRecord{}, fmt.Errorf("load QA parent run %q: %w", parentRunID, err)
	}
	if parent.WorkflowRunID == "" {
		return QAParentRecord{}, fmt.Errorf(
			"QA parent run %q has no workflow run",
			parentRunID,
		)
	}
	return parent, nil
}

func (coordinator *InvestigationCoordinator) converge(
	ctx context.Context,
	parent QAParentRecord,
	terminal InvestigationTerminal,
) error {
	if terminal.WorkflowRunID != parent.WorkflowRunID {
		return fmt.Errorf(
			"QA parent run %q references workflow %q but terminal belongs to %q",
			parent.ID,
			parent.WorkflowRunID,
			terminal.WorkflowRunID,
		)
	}
	outcome, err := investigationOutcome(terminal)
	if err != nil {
		return err
	}
	if coordinator.scenarios == nil {
		return fmt.Errorf("QA parent lifecycle is unavailable")
	}
	if outcome.Status == RunStatusDone {
		if err := persistSessionTurn(
			ctx,
			coordinator.sessions,
			parent.ID,
			parent.SessionID,
			parent.UserID,
			parent.Question,
			outcome,
		); err != nil {
			return fmt.Errorf(
				"persist completed QA parent run %q session turn: %w",
				parent.ID,
				err,
			)
		}
	}
	if err := coordinator.scenarios.CompleteScenario(ctx, parent.ID, outcome); err != nil {
		return err
	}
	return nil
}
