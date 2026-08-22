package qa

import (
	"context"
	"errors"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

type sessionTurnStore interface {
	EnsureSession(string, int64, string) error
	AppendTurn(string, string, int64, []llm.Message) (int, error)
}

// Coordinator converges durable Workflow facts into QA Parents.
type Coordinator struct {
	investigation InvestigationRunner
	planner       InvestigationPlanner
	scenarios     ScenarioLifecycle
	parentRuns    ParentRunReader
	sessions      sessionTurnStore
}

// NewCoordinator binds the durable stores used by recovery and control.
func NewCoordinator(
	investigation InvestigationRunner,
	scenarios ScenarioLifecycle,
	parentRuns ParentRunReader,
	sessions *memory.SessionStore,
) *Coordinator {
	var sessionTurns sessionTurnStore
	if sessions != nil {
		sessionTurns = sessions
	}
	return &Coordinator{
		investigation: investigation,
		scenarios:     scenarios,
		parentRuns:    parentRuns,
		sessions:      sessionTurns,
	}
}

// SetInvestigationPlanner enables gap-aware planning for continuation rounds.
func (coordinator *Coordinator) SetInvestigationPlanner(
	planner InvestigationPlanner,
) {
	if coordinator == nil {
		return
	}
	coordinator.planner = planner
}

// Await joins local execution when present before converging durable terminal facts.
func (coordinator *Coordinator) Await(
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
	investigation, err := coordinator.runner()
	if err != nil {
		return err
	}
	terminal, err := investigation.AwaitTerminal(ctx, workflowRunID)
	if err != nil {
		return fmt.Errorf("await QA investigation workflow %q: %w", workflowRunID, err)
	}
	return coordinator.converge(ctx, parent, terminal)
}

// Reconcile converges one Parent from durable Workflow facts.
func (coordinator *Coordinator) Reconcile(
	ctx context.Context,
	parentRunID string,
) error {
	parent, err := coordinator.loadParent(parentRunID)
	if err != nil {
		return err
	}
	investigation, err := coordinator.runner()
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

// Cancel propagates an owned Parent cancellation to its Workflow.
func (coordinator *Coordinator) Cancel(
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
	investigation, err := coordinator.runner()
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
	return coordinator.Reconcile(context.WithoutCancel(ctx), parentRunID)
}

func (coordinator *Coordinator) runner() (InvestigationRunner, error) {
	if coordinator == nil || coordinator.investigation == nil {
		return nil, fmt.Errorf("QA investigation workflow service is unavailable")
	}
	return coordinator.investigation, nil
}

func (coordinator *Coordinator) loadParent(
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

func (coordinator *Coordinator) converge(
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
	if continuation, ok := coordinator.investigation.(InvestigationContinuationRunner); ok {
		return coordinator.convergeContinuation(ctx, parent, terminal, continuation)
	}
	return coordinator.complete(ctx, parent, terminal)
}

func (coordinator *Coordinator) convergeContinuation(
	ctx context.Context,
	parent QAParentRecord,
	terminal InvestigationTerminal,
	continuation InvestigationContinuationRunner,
) error {
	snapshot, err := continuation.LoadRound(ctx, terminal.WorkflowRunID)
	if err != nil {
		return fmt.Errorf(
			"load QA investigation round %q: %w",
			terminal.WorkflowRunID,
			err,
		)
	}
	if snapshot.Terminal.WorkflowRunID != terminal.WorkflowRunID {
		return fmt.Errorf(
			"QA investigation round snapshot belongs to %q, not %q",
			snapshot.Terminal.WorkflowRunID,
			terminal.WorkflowRunID,
		)
	}
	terminal = snapshot.Terminal
	if terminal.Output == nil {
		return coordinator.complete(ctx, parent, terminal)
	}

	merged := MergeRoundResult(
		InvestigationResult{EvidenceUnits: snapshot.SeedEvidence},
		*terminal.Output,
	)
	currentRound := terminal.Round
	if currentRound <= 0 {
		currentRound = 1
	}
	roundContext := InvestigationRoundContext{
		ParentRunID:           parent.ID,
		Objective:             snapshot.Contract.Objective,
		Round:                 currentRound,
		MaxRounds:             defaultInvestigationMaxRounds,
		PreviousWorkflowRunID: terminal.WorkflowRunID,
		RemainingBudget:       0,
	}

	for {
		current := cloneInvestigationResult(*terminal.Output)
		if current.Round <= 0 {
			current.Round = currentRound
		}
		merged = MergeRoundResult(merged, current)
		nextContext := NextRound(roundContext, current)
		if !ShouldContinue(nextContext) {
			return coordinator.completeResult(ctx, parent, terminal, merged)
		}
		if NewEvidenceRatio(snapshot.SeedEvidence, current.EvidenceUnits) <= 0 {
			merged.StopReason = string(workflow.StopNoNewEvidence)
			return coordinator.completeResult(ctx, parent, terminal, merged)
		}
		contract, ok := continuationContract(snapshot.Contract, merged)
		if !ok {
			return coordinator.completeResult(ctx, parent, terminal, merged)
		}
		proposal, err := coordinator.planNextRound(
			ctx,
			contract,
			merged,
			merged.EvidenceUnits,
		)
		if err != nil {
			return fmt.Errorf(
				"plan QA continuation round %d: %w",
				nextContext.Round,
				err,
			)
		}

		nextID := StableRoundWorkflowID(parent.ID, nextContext.Round)
		actor := snapshot.Actor
		if actor.UserID <= 0 {
			actor.UserID = parent.UserID
		}
		nextSnapshot, err := coordinator.loadOrStartContinuation(
			ctx,
			parent,
			continuation,
			snapshot,
			InvestigationContinuationRequest{
				ParentRunID:           parent.ID,
				PreviousWorkflowRunID: terminal.WorkflowRunID,
				WorkflowRunID:         nextID,
				Contract:              contract,
				Proposal:              proposal,
				SeedEvidence:          cloneEvidenceUnits(merged.EvidenceUnits),
				Actor:                 actor,
				Round:                 nextContext.Round,
				BaseDepth:             terminal.BaseDepth,
			},
		)
		if err != nil {
			return err
		}
		if nextSnapshot.Terminal.WorkflowRunID != nextID {
			return fmt.Errorf(
				"QA continuation round snapshot belongs to %q, not %q",
				nextSnapshot.Terminal.WorkflowRunID,
				nextID,
			)
		}
		if nextSnapshot.Terminal.Output == nil {
			terminal = nextSnapshot.Terminal
			return coordinator.completeResult(ctx, parent, terminal, merged)
		}
		snapshot = nextSnapshot
		terminal = snapshot.Terminal
		currentRound = nextContext.Round
		roundContext = nextContext
	}
}

func (coordinator *Coordinator) planNextRound(
	ctx context.Context,
	contract TaskContract,
	result InvestigationResult,
	seedEvidence []tool.EvidenceUnit,
) (*agentapi.TaskGraphProposal, error) {
	if coordinator.planner != nil {
		proposal, err := coordinator.planner.PlanInvestigation(
			ctx,
			cloneTaskContract(contract),
			cloneInvestigationResult(result),
			cloneEvidenceUnits(seedEvidence),
		)
		if err == nil && proposal == nil {
			err = fmt.Errorf("investigation planner returned no proposal")
		}
		if err == nil {
			prepared, prepareErr := prepareInvestigationProposal(
				proposal,
				contract,
				seedEvidence,
			)
			if prepareErr == nil {
				return prepared, nil
			}
			err = prepareErr
		}
		log.WarnfCtx(
			ctx,
			"[qa] continuation task graph planner degraded; using deterministic gap cover: %v",
			err,
		)
	} else {
		log.WarnfCtx(
			ctx,
			"[qa] continuation task graph planner unavailable; using deterministic gap cover",
		)
	}

	fallback, err := buildTaskGraphFallback(contract)
	if err != nil {
		return nil, fmt.Errorf("build deterministic continuation task graph: %w", err)
	}
	prepared, err := prepareInvestigationProposal(
		&fallback,
		contract,
		seedEvidence,
	)
	if err != nil {
		return nil, fmt.Errorf("prepare deterministic continuation task graph: %w", err)
	}
	return prepared, nil
}

func (coordinator *Coordinator) loadOrStartContinuation(
	ctx context.Context,
	parent QAParentRecord,
	continuation InvestigationContinuationRunner,
	previous InvestigationRoundSnapshot,
	request InvestigationContinuationRequest,
) (InvestigationRoundSnapshot, error) {
	existing, err := continuation.LoadRound(ctx, request.WorkflowRunID)
	if err == nil {
		return existing, nil
	}
	if errors.Is(err, workflow.ErrConflict) {
		if _, awaitErr := continuation.AwaitTerminal(ctx, request.WorkflowRunID); awaitErr != nil {
			return InvestigationRoundSnapshot{}, fmt.Errorf(
				"await existing QA continuation workflow %q: %w",
				request.WorkflowRunID,
				awaitErr,
			)
		}
		return continuation.LoadRound(ctx, request.WorkflowRunID)
	}
	if !errors.Is(err, workflow.ErrNotFound) {
		return InvestigationRoundSnapshot{}, fmt.Errorf(
			"load QA continuation workflow %q: %w",
			request.WorkflowRunID,
			err,
		)
	}
	if err := continuation.StartNextRound(ctx, request); err != nil {
		if !errors.Is(err, workflow.ErrConflict) {
			return InvestigationRoundSnapshot{}, fmt.Errorf(
				"start QA continuation workflow %q after %q: %w",
				request.WorkflowRunID,
				previous.Terminal.WorkflowRunID,
				err,
			)
		}
	}
	if _, err := continuation.AwaitTerminal(ctx, request.WorkflowRunID); err != nil {
		return InvestigationRoundSnapshot{}, fmt.Errorf(
			"await QA continuation workflow %q: %w",
			request.WorkflowRunID,
			err,
		)
	}
	return continuation.LoadRound(ctx, request.WorkflowRunID)
}

func (coordinator *Coordinator) complete(
	ctx context.Context,
	parent QAParentRecord,
	terminal InvestigationTerminal,
) error {
	outcome, err := investigationOutcome(terminal)
	if err != nil {
		return err
	}
	return coordinator.completeOutcome(ctx, parent, outcome)
}

func (coordinator *Coordinator) completeResult(
	ctx context.Context,
	parent QAParentRecord,
	terminal InvestigationTerminal,
	result InvestigationResult,
) error {
	terminal.Output = &result
	return coordinator.complete(ctx, parent, terminal)
}

func (coordinator *Coordinator) completeOutcome(
	ctx context.Context,
	parent QAParentRecord,
	outcome RunOutcome,
) error {
	if coordinator.scenarios == nil {
		return fmt.Errorf("QA parent lifecycle is unavailable")
	}
	if outcome.Status == RunStatusDone {
		if err := persistTurn(
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
	if err := coordinator.scenarios.Complete(ctx, parent.ID, outcome); err != nil {
		return err
	}
	return nil
}
