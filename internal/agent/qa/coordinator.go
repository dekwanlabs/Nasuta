package qa

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
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
	maxRounds     int
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

// SetInvestigationMaxRounds applies the platform-owned continuation bound.
func (coordinator *Coordinator) SetInvestigationMaxRounds(maxRounds int) {
	if coordinator == nil {
		return
	}
	coordinator.maxRounds = maxRounds
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
	log.InfofCtx(ctx, "[qa] investigation await requested parent=%s workflow=%s", parentRunID, workflowRunID)
	parent, err := coordinator.loadParent(parentRunID)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation await parent load failed parent=%s workflow=%s: %v", parentRunID, workflowRunID, err)
		return err
	}
	if parent.WorkflowRunID != workflowRunID {
		err := fmt.Errorf(
			"QA parent run %q references workflow %q, not %q",
			parentRunID,
			parent.WorkflowRunID,
			workflowRunID,
		)
		log.ErrorfCtx(ctx, "[qa] investigation await identity mismatch parent=%s workflow=%s: %v", parentRunID, workflowRunID, err)
		return err
	}
	investigator, err := coordinator.runner()
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation await runner unavailable parent=%s workflow=%s: %v", parentRunID, workflowRunID, err)
		return err
	}
	if parent.Status.Terminal() {
		log.InfofCtx(ctx, "[qa] investigation await parent already terminal parent=%s workflow=%s status=%s", parentRunID, workflowRunID, parent.Status)
		return nil
	}
	terminal, err := loadInvestigationTerminalWithRetry(ctx, investigator, workflowRunID, "await")
	if err != nil {
		if errors.Is(err, investigation.ErrNotFound) {
			log.WarnfCtx(ctx, "[qa] investigation workflow missing while converging parent=%s workflow=%s phase=await: %v", parentRunID, workflowRunID, err)
			completeErr := coordinator.complete(ctx, parent, missingInvestigationTerminal(workflowRunID))
			if completeErr != nil {
				log.ErrorfCtx(ctx, "[qa] investigation missing terminal persistence failed parent=%s workflow=%s: %v", parentRunID, workflowRunID, completeErr)
			}
			return completeErr
		}
		log.ErrorfCtx(ctx, "[qa] investigation await failed parent=%s workflow=%s: %v", parentRunID, workflowRunID, err)
		return fmt.Errorf("await QA investigation workflow %q: %w", workflowRunID, err)
	}
	log.InfofCtx(ctx, "[qa] investigation await terminal received parent=%s workflow=%s status=%s", parentRunID, workflowRunID, terminal.Status)
	if err := coordinator.converge(ctx, parent, terminal); err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation terminal convergence failed parent=%s workflow=%s: %v", parentRunID, workflowRunID, err)
		return err
	}
	return nil
}

// Reconcile converges one Parent from durable Workflow facts.
func (coordinator *Coordinator) Reconcile(
	ctx context.Context,
	parentRunID string,
) error {
	log.InfofCtx(ctx, "[qa] investigation reconcile requested parent=%s", parentRunID)
	parent, err := coordinator.loadParent(parentRunID)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation reconcile parent load failed parent=%s: %v", parentRunID, err)
		return err
	}
	if parent.Status.Terminal() {
		log.InfofCtx(ctx, "[qa] investigation reconcile parent already terminal parent=%s workflow=%s status=%s", parentRunID, parent.WorkflowRunID, parent.Status)
		return nil
	}
	investigator, err := coordinator.runner()
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation reconcile runner unavailable parent=%s workflow=%s: %v", parentRunID, parent.WorkflowRunID, err)
		return err
	}
	terminal, err := loadInvestigationTerminalWithRetry(ctx, investigator, parent.WorkflowRunID, "reconcile")
	if err != nil {
		if errors.Is(err, investigation.ErrNotFound) {
			log.WarnfCtx(ctx, "[qa] investigation workflow missing while converging parent=%s workflow=%s phase=reconcile: %v", parentRunID, parent.WorkflowRunID, err)
			completeErr := coordinator.complete(ctx, parent, missingInvestigationTerminal(parent.WorkflowRunID))
			if completeErr != nil {
				log.ErrorfCtx(ctx, "[qa] investigation missing terminal persistence failed parent=%s workflow=%s: %v", parentRunID, parent.WorkflowRunID, completeErr)
			}
			return completeErr
		}
		log.ErrorfCtx(ctx, "[qa] investigation reconcile load failed parent=%s workflow=%s: %v", parentRunID, parent.WorkflowRunID, err)
		return fmt.Errorf(
			"load QA investigation workflow %q terminal result: %w",
			parent.WorkflowRunID,
			err,
		)
	}
	log.InfofCtx(ctx, "[qa] investigation reconcile terminal received parent=%s workflow=%s status=%s", parentRunID, parent.WorkflowRunID, terminal.Status)
	if err := coordinator.converge(ctx, parent, terminal); err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation reconcile convergence failed parent=%s workflow=%s: %v", parentRunID, parent.WorkflowRunID, err)
		return err
	}
	return nil
}

const investigationMissingRetryAttempts = 5

func loadInvestigationTerminalWithRetry(
	ctx context.Context,
	runner InvestigationRunner,
	workflowRunID string,
	phase string,
) (InvestigationTerminal, error) {
	var terminal InvestigationTerminal
	var err error
	for attempt := 1; attempt <= investigationMissingRetryAttempts; attempt++ {
		if phase == "await" {
			terminal, err = runner.AwaitTerminal(ctx, workflowRunID)
		} else {
			terminal, err = runner.LoadTerminal(ctx, workflowRunID)
		}
		if err == nil {
			return terminal, nil
		}
		if !errors.Is(err, investigation.ErrNotFound) {
			return InvestigationTerminal{}, err
		}
		retryable := attempt < investigationMissingRetryAttempts
		log.WarnfCtx(ctx, "[qa] investigation terminal not found parent_phase=%s workflow=%s attempt=%d/%d retryable=%t: %v", phase, workflowRunID, attempt, investigationMissingRetryAttempts, retryable, err)
		if !retryable {
			break
		}
		delay := time.Duration(25*(1<<(attempt-1))) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return InvestigationTerminal{}, ctx.Err()
		}
	}
	return InvestigationTerminal{}, err
}

// Cancel propagates an owned Parent cancellation to its Workflow.
func missingInvestigationTerminal(workflowRunID string) InvestigationTerminal {
	return InvestigationTerminal{
		WorkflowRunID: workflowRunID,
		Status:        InvestigationFailed,
		ErrorCode:     "investigation_run_missing",
		Completeness:  InvestigationUnavailable,
	}
}

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

func (coordinator *Coordinator) investigationMaxRounds() int {
	if coordinator.maxRounds > 0 {
		return coordinator.maxRounds
	}
	return defaultInvestigationMaxRounds
}

func terminalBudgetExhausted(terminal InvestigationTerminal) bool {
	return terminal.ErrorCode == "budget_exhausted" ||
		terminal.ErrorCode == "workflow_budget_exhausted" ||
		terminal.StopReason == string(workflow.StopBudgetExhausted) ||
		terminal.StopReason == "workflow_budget_exhausted"
}

func continuationEligible(terminal InvestigationTerminal) bool {
	// Budget exhaustion is a hard terminal boundary. A child may still carry
	// useful partial evidence, but spending another round cannot make the
	// exhausted ledger affordable.
	if terminalBudgetExhausted(terminal) {
		return false
	}
	return terminal.Status == InvestigationSucceeded ||
		(terminal.Status == InvestigationFailed && terminal.ErrorCode == "evidence_insufficient")
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
	if terminal.Output == nil || !continuationEligible(terminal) {
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
		MaxRounds:             coordinator.investigationMaxRounds(),
		PreviousWorkflowRunID: terminal.WorkflowRunID,
		BudgetLimit:           snapshot.BudgetLimit,
	}

	for {
		roundContext.BudgetUsed = addInvestigationUsage(roundContext.BudgetUsed, terminal.Usage)
		current := cloneInvestigationResult(*terminal.Output)
		if current.Round <= 0 {
			current.Round = currentRound
		}
		merged = MergeRoundResult(merged, current)
		nextContext := NextRound(roundContext, current)
		nextContext.HasValidReport = resultHasValidReport(merged)
		nextContext.PendingEntitySelection = pendingEntitySelection(snapshot.Contract, merged)
		if !ShouldContinue(nextContext) {
			if !investigationBudgetAvailable(nextContext.BudgetLimit, nextContext.BudgetUsed) {
				merged.StopReason = string(workflow.StopBudgetExhausted)
			}
			return coordinator.completeResult(ctx, parent, terminal, merged)
		}
		if !nextContext.PendingEntitySelection &&
			NewEvidenceRatio(snapshot.SeedEvidence, current.EvidenceUnits) <= 0 {
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
		if !tightenContinuationBudget(proposal, nextContext.BudgetLimit, nextContext.BudgetUsed) {
			merged.StopReason = string(workflow.StopBudgetExhausted)
			return coordinator.completeResult(ctx, parent, terminal, merged)
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
	if parent.Status.Terminal() {
		log.InfofCtx(ctx, "[qa] complete skipped because parent is already terminal parent=%s status=%s", parent.ID, parent.Status)
		return nil
	}
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
		if errors.Is(err, agentrun.ErrNotActive) {
			log.InfofCtx(ctx, "[qa] complete replay observed already terminal parent=%s", parent.ID)
			return nil
		}
		return err
	}
	return nil
}
