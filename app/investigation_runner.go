package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	agentqa "github.com/dekwanlabs/nasuta/internal/agent/qa"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

type qaInvestigator struct {
	platform *Platform
	coord    *investigation.Coordinator
	events   run.ExecutionEventEmitter

	mu   sync.Mutex
	runs map[string]*investigationState
}

type investigationState struct {
	runID       string
	coordinator *investigation.Coordinator
	done        chan struct{}
	terminal    agent.InvestigationTerminal
	err         error
}

type investigationStartResult struct {
	persisted bool
	err       error
}

type investigationRecoveryReport struct {
	Scanned int
	Resumed int
	Failed  int
	Skipped int
	Errors  int
}

// RecoverActive resumes durable child snapshots that were left non-terminal
// when the process stopped. The storage cursor makes this scan bounded.
func (runner *qaInvestigator) RecoverActive(
	ctx context.Context,
	cutoff time.Time,
	pageSize int,
) error {
	var report investigationRecoveryReport
	if ctx == nil {
		ctx = context.Background()
	}
	if cutoff.IsZero() {
		return fmt.Errorf("investigation recovery cutoff is required")
	}
	if pageSize <= 0 {
		return fmt.Errorf("investigation recovery page size must be positive")
	}
	coordinator, err := runner.coordinator()
	if err != nil {
		return err
	}
	lister, ok := coordinator.Store.(investigation.ActiveRunLister)
	if !ok {
		return fmt.Errorf("investigation run store does not support bounded active-run recovery")
	}
	cursor := investigation.ActiveRunCursor{}
	var firstErr error
	for {
		page, err := lister.ListActiveRuns(cutoff, cursor, pageSize)
		if err != nil {
			return fmt.Errorf("list active investigation runs: %w", err)
		}
		for _, durable := range page.Runs {
			report.Scanned++
			parentID := durable.Contract.ParentRunID
			if parentID == "" {
				parentID = durable.Contract.TaskID
			}
			log.InfofCtx(ctx, "[qa] investigation child recovery requested run=%s parent=%s status=%s", durable.ID, parentID, durable.Status)
			if failure := nonResumableRecoveryFailure(durable, parentID); failure != nil {
				if err := coordinator.Store.Fail(durable.ID, *failure, investigation.RunFailed); err != nil {
					report.Errors++
					if firstErr == nil {
						firstErr = fmt.Errorf("fail created investigation run %q: %w", durable.ID, err)
					}
					log.ErrorfCtx(ctx, "[qa] investigation child recovery failed run=%s parent=%s status=%s error_code=%s retryable=%t: %v", durable.ID, parentID, durable.Status, failure.Code, failure.Retryable, err)
					continue
				}
				report.Failed++
				log.WarnfCtx(ctx, "[qa] investigation child recovery terminalized run=%s parent=%s status=%s error_code=%s retryable=%t", durable.ID, parentID, durable.Status, failure.Code, failure.Retryable)
				continue
			}
			_, resumeErr := coordinator.Resume(ctx, durable.ID)
			if errors.Is(resumeErr, investigation.ErrLeaseHeld) {
				report.Skipped++
				log.InfofCtx(ctx, "[qa] investigation child recovery skipped run=%s parent=%s status=%s reason=lease_held", durable.ID, parentID, durable.Status)
				continue
			}
			if resumeErr != nil {
				report.Errors++
				if firstErr == nil {
					firstErr = fmt.Errorf("resume investigation run %q: %w", durable.ID, resumeErr)
				}
				log.ErrorfCtx(ctx, "[qa] investigation child recovery failed run=%s parent=%s status=%s error_code=%s: %v", durable.ID, parentID, durable.Status, investigationFailureCode(resumeErr), resumeErr)
				continue
			}
			report.Resumed++
			log.InfofCtx(ctx, "[qa] investigation child recovery completed run=%s parent=%s previous_status=%s", durable.ID, parentID, durable.Status)
		}
		if !page.HasMore {
			break
		}
		if page.Next.ID == "" || page.Next.UpdatedAt.IsZero() {
			return fmt.Errorf("active investigation recovery returned an incomplete cursor")
		}
		cursor = page.Next
	}
	log.InfofCtx(ctx, "[qa] investigation child recovery complete scanned=%d resumed=%d terminalized=%d skipped=%d errors=%d", report.Scanned, report.Resumed, report.Failed, report.Skipped, report.Errors)
	return firstErr
}

func nonResumableRecoveryFailure(
	run investigation.InvestigationRun,
	parentID string,
) *investigation.RunFailure {
	if run.Status == investigation.RunCreated {
		return &investigation.RunFailure{
			Code: investigation.FailureExecution, Message: "investigation snapshot was left before execution began",
			Stage: "initialization", TaskID: parentID, Retryable: false,
		}
	}
	if run.Status == investigation.RunAnalyzing || len(run.Plan.Tasks) == 0 {
		return &investigation.RunFailure{
			Code: investigation.FailurePlan, Message: "investigation snapshot has no durable executable plan",
			Stage: string(investigation.StagePlanning), TaskID: parentID, Retryable: false,
		}
	}
	return nil
}

func investigationFailureCode(err error) string {
	var failure *investigation.RunFailureError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &failure):
		return string(failure.Failure.Code)
	case errors.Is(err, investigation.ErrLeaseHeld):
		return "lease_held"
	case errors.Is(err, investigation.ErrLeaseFenced):
		return string(investigation.FailureLease)
	case errors.Is(err, investigation.ErrNotFound):
		return "investigation_run_missing"
	case errors.Is(err, investigation.ErrInvalidTransition):
		return "invalid_transition"
	default:
		return "resume_failed"
	}
}

func (runner *qaInvestigator) Available() bool {
	_, err := runner.coordinator()
	return err == nil
}

func (runner *qaInvestigator) Start(
	ctx context.Context,
	request agent.InvestigationRequest,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	workflowRunID := strings.TrimSpace(request.WorkflowRunID)
	if workflowRunID == "" {
		return fmt.Errorf("investigation workflow run id is required")
	}
	parentRunID := strings.TrimSpace(request.ParentRunID)
	if parentRunID == "" {
		parentRunID = request.Contract.TaskID
	}
	log.InfofCtx(ctx, "[qa] investigation start requested workflow=%s parent=%s", workflowRunID, parentRunID)
	coordinator, err := runner.newCoordinator(request, parentRunID)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation coordinator initialization failed workflow=%s: %v", workflowRunID, err)
		return err
	}
	contract := contractFromTaskContract(request)
	if contract.ID != workflowRunID || investigation.ContractRunID(contract) != workflowRunID {
		return fmt.Errorf("investigation workflow identity mismatch: request=%q contract=%q", workflowRunID, contract.ID)
	}
	state := &investigationState{
		runID:       investigation.ContractRunID(contract),
		coordinator: coordinator,
		done:        make(chan struct{}),
	}
	if existing, loadErr := coordinator.LoadRun(ctx, state.runID); loadErr == nil {
		if existing.Status.Terminal() {
			if !sameInvestigationContract(existing.Contract, contract) {
				return fmt.Errorf("%w: investigation workflow %q already completed with a different contract", workflow.ErrConflict, workflowRunID)
			}
			log.InfofCtx(ctx, "[qa] investigation start replayed durable terminal workflow=%s run=%s status=%s", workflowRunID, existing.ID, existing.Status)
			return nil
		}
		return fmt.Errorf("%w: investigation workflow %q run %q is already active", investigation.ErrInvalidTransition, workflowRunID, existing.ID)
	} else if !errors.Is(loadErr, investigation.ErrNotFound) {
		return fmt.Errorf("load investigation workflow %q before start: %w", workflowRunID, loadErr)
	}
	if err := runner.track(workflowRunID, state); err != nil {
		return err
	}
	ready := make(chan investigationStartResult, 1)
	executionCtx := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			close(state.done)
			runner.remove(workflowRunID, state)
		}()
		started := false
		run, runErr := coordinator.ExecuteWithProposalReady(
			executionCtx,
			contract,
			request.Proposal,
			func(persisted investigation.InvestigationRun) {
				started = true
				log.InfofCtx(executionCtx, "[qa] investigation snapshot persisted workflow=%s run=%s status=%s", workflowRunID, persisted.ID, persisted.Status)
				ready <- investigationStartResult{persisted: true}
			},
		)
		if !started {
			ready <- investigationStartResult{err: runErr}
		}
		if runErr != nil {
			phase := "execution"
			if !started {
				phase = "initialization"
			}
			log.ErrorfCtx(executionCtx, "[qa] investigation %s failed workflow=%s run=%s: %v", phase, workflowRunID, run.ID, runErr)
		} else {
			log.InfofCtx(executionCtx, "[qa] investigation execution completed workflow=%s run=%s status=%s", workflowRunID, run.ID, run.Status)
		}
		terminal, mapErr := investigationTerminal(run, runErr)
		runner.complete(workflowRunID, state, terminal, mapErr)
	}()
	select {
	case startResult := <-ready:
		if startResult.err != nil {
			log.ErrorfCtx(ctx, "[qa] investigation start failed before snapshot became durable workflow=%s persisted=%t: %v", workflowRunID, startResult.persisted, startResult.err)
			return fmt.Errorf("start investigation workflow %q: %w", workflowRunID, startResult.err)
		}
		return nil
	case <-ctx.Done():
		log.ErrorfCtx(ctx, "[qa] investigation snapshot persistence wait canceled workflow=%s: %v", workflowRunID, ctx.Err())
		return fmt.Errorf("wait for investigation workflow %q to start: %w", workflowRunID, ctx.Err())
	}
}

// LoadRun returns the native  snapshot for transport and recovery reads.
func (runner *qaInvestigator) LoadRun(
	ctx context.Context,
	workflowRunID string,
) (investigation.InvestigationRun, error) {
	coordinator, err := runner.coordinator()
	if err != nil {
		return investigation.InvestigationRun{}, err
	}
	return coordinator.LoadRun(ctx, strings.TrimSpace(workflowRunID))
}

// LoadDelivery returns the persisted delivery without the QA terminal projection.
func (runner *qaInvestigator) LoadDelivery(
	ctx context.Context,
	workflowRunID string,
) (investigation.DeliveryResult, error) {
	run, err := runner.LoadRun(ctx, workflowRunID)
	if err != nil {
		return investigation.DeliveryResult{}, err
	}
	if err := investigation.ValidateContractVersion(run.Contract); err != nil {
		return investigation.DeliveryResult{}, fmt.Errorf("load investigation delivery %q: %w", run.ID, err)
	}
	if run.Delivery == nil {
		return investigation.DeliveryResult{}, fmt.Errorf(
			"%w: run %q has no delivery result", investigation.ErrNoDelivery, run.ID,
		)
	}
	return *run.Delivery, nil
}

func (runner *qaInvestigator) LoadRound(
	ctx context.Context,
	workflowRunID string,
) (agent.InvestigationRoundSnapshot, error) {
	run, err := runner.LoadRun(ctx, workflowRunID)
	if err != nil {
		if errors.Is(err, investigation.ErrNotFound) {
			return agent.InvestigationRoundSnapshot{}, fmt.Errorf("%w: investigation round %q", workflow.ErrNotFound, workflowRunID)
		}
		return agent.InvestigationRoundSnapshot{}, err
	}
	if !run.Status.Terminal() {
		return agent.InvestigationRoundSnapshot{}, fmt.Errorf(
			"%w: investigation run %q is still %q",
			workflow.ErrConflict,
			run.ID,
			run.Status,
		)
	}
	terminal, err := investigationTerminal(run, nil)
	if err != nil {
		return agent.InvestigationRoundSnapshot{}, err
	}
	return agent.InvestigationRoundSnapshot{
		Terminal:     terminal,
		Contract:     taskContractFromInvestigationContract(run.Contract),
		SeedEvidence: evidenceUnits(run.Contract.SeedEvidence),
		Actor:        run.Contract.Actor,
		BudgetLimit:  investigationBudget(run.Budget.Run.Limit),
	}, nil
}

func (runner *qaInvestigator) StartNextRound(
	ctx context.Context,
	request agent.InvestigationContinuationRequest,
) error {
	err := runner.Start(ctx, agent.InvestigationRequest{
		WorkflowRunID: request.WorkflowRunID,
		ParentRunID:   request.ParentRunID,
		Round:         request.Round,
		BaseDepth:     request.BaseDepth,
		Contract:      request.Contract,
		Proposal:      request.Proposal,
		SeedEvidence:  request.SeedEvidence,
		Actor:         request.Actor,
	})
	if errors.Is(err, investigation.ErrInvalidTransition) || errors.Is(err, investigation.ErrLeaseHeld) {
		return fmt.Errorf("%w: investigation round %q is already active", workflow.ErrConflict, request.WorkflowRunID)
	}
	return err
}

func (runner *qaInvestigator) AwaitTerminal(
	ctx context.Context,
	workflowRunID string,
) (agent.InvestigationTerminal, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	workflowRunID = strings.TrimSpace(workflowRunID)
	if workflowRunID == "" {
		return agent.InvestigationTerminal{}, fmt.Errorf("investigation run id is required")
	}
	log.InfofCtx(ctx, "[qa] investigation await started workflow=%s", workflowRunID)
	state, tracked := runner.state(workflowRunID)
	coordinator, err := runner.coordinator()
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation await coordinator unavailable workflow=%s: %v", workflowRunID, err)
		return agent.InvestigationTerminal{}, err
	}
	// The process-local channel is only an optimization. A durable poll makes
	// waiting work after a restart or when another process owns execution.
	const pollInterval = 100 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	const creationGrace = 2 * time.Second
	missingSince := time.Time{}
	for {
		run, loadErr := coordinator.LoadRun(ctx, workflowRunID)
		if loadErr != nil {
			if errors.Is(loadErr, investigation.ErrNotFound) {
				if missingSince.IsZero() {
					missingSince = time.Now()
				}
				log.WarnfCtx(ctx, "[qa] investigation await load missing workflow=%s state_tracked=%t age=%s: %v", workflowRunID, tracked, time.Since(missingSince).Round(time.Millisecond), loadErr)
				if !tracked || time.Since(missingSince) >= creationGrace {
					return agent.InvestigationTerminal{}, loadErr
				}
			} else {
				log.ErrorfCtx(ctx, "[qa] investigation await load failed workflow=%s: %v", workflowRunID, loadErr)
				return agent.InvestigationTerminal{}, loadErr
			}
		} else {
			missingSince = time.Time{}
			if run.Status.Terminal() {
				terminal, terminalErr := investigationTerminal(run, nil)
				if terminalErr != nil {
					log.ErrorfCtx(ctx, "[qa] investigation await terminal mapping failed workflow=%s phase=terminal: %v", workflowRunID, terminalErr)
					return agent.InvestigationTerminal{}, terminalErr
				}
				log.InfofCtx(ctx, "[qa] investigation await terminal workflow=%s status=%s", workflowRunID, terminal.Status)
				return terminal, nil
			}
		}
		if loadErr == nil && state != nil {
			select {
			case <-state.done:
				// Durable state was not terminal above, so a completed local
				// callback cannot override it. Keep polling until persistence catches up.
			default:
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			log.WarnfCtx(ctx, "[qa] investigation await canceled workflow=%s: %v", workflowRunID, ctx.Err())
			return agent.InvestigationTerminal{}, ctx.Err()
		}
	}
}

func (runner *qaInvestigator) LoadTerminal(
	ctx context.Context,
	workflowRunID string,
) (agent.InvestigationTerminal, error) {
	workflowRunID = strings.TrimSpace(workflowRunID)
	if workflowRunID == "" {
		return agent.InvestigationTerminal{}, fmt.Errorf("investigation run id is required")
	}
	coordinator, err := runner.coordinator()
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation terminal coordinator unavailable workflow=%s: %v", workflowRunID, err)
		return agent.InvestigationTerminal{}, err
	}
	// Durable state is authoritative. Local state is only a notification
	// optimization and must never resurrect a deleted or incomplete snapshot.
	run, err := coordinator.LoadRun(ctx, workflowRunID)
	if err != nil {
		if errors.Is(err, investigation.ErrNotFound) {
			log.WarnfCtx(ctx, "[qa] investigation terminal load missing workflow=%s: %v", workflowRunID, err)
		} else {
			log.ErrorfCtx(ctx, "[qa] investigation terminal load failed workflow=%s: %v", workflowRunID, err)
		}
		return agent.InvestigationTerminal{}, err
	}
	if !run.Status.Terminal() {
		return agent.InvestigationTerminal{}, fmt.Errorf(
			"%w: investigation run %q is still %q",
			workflow.ErrConflict,
			workflowRunID,
			run.Status,
		)
	}
	terminal, err := investigationTerminal(run, nil)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation terminal mapping failed workflow=%s: %v", workflowRunID, err)
		return agent.InvestigationTerminal{}, err
	}
	return terminal, nil
}

func (runner *qaInvestigator) Cancel(
	ctx context.Context,
	workflowRunID string,
	_ int64,
) error {
	workflowRunID = strings.TrimSpace(workflowRunID)
	if workflowRunID == "" {
		return fmt.Errorf("investigation run id is required")
	}
	log.InfofCtx(ctx, "[qa] investigation cancellation requested workflow=%s", workflowRunID)
	coordinator, err := runner.coordinator()
	if err != nil {
		return err
	}
	if state, ok := runner.state(workflowRunID); ok && state.coordinator != nil {
		coordinator = state.coordinator
		workflowRunID = state.runID
	}
	if err := coordinator.Cancel(ctx, workflowRunID); err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation cancellation failed workflow=%s: %v", workflowRunID, err)
		return err
	}
	_, err = runner.AwaitTerminal(ctx, workflowRunID)
	if err != nil {
		log.ErrorfCtx(ctx, "[qa] investigation cancellation convergence failed workflow=%s: %v", workflowRunID, err)
		return fmt.Errorf("await cancelled investigation workflow %q: %w", workflowRunID, err)
	}
	log.InfofCtx(ctx, "[qa] investigation cancellation completed workflow=%s", workflowRunID)
	return nil
}

func (runner *qaInvestigator) coordinator() (*investigation.Coordinator, error) {
	if runner == nil || runner.platform == nil {
		return nil, fmt.Errorf("investigation platform is unavailable")
	}
	runner.mu.Lock()
	if runner.coord != nil {
		coordinator := runner.coord
		runner.mu.Unlock()
		return coordinator, nil
	}
	runner.mu.Unlock()
	settings := runner.platform.Settings()
	coordinator, err := runner.platform.buildInvestigationCoordinator(&settings)
	if err != nil {
		return nil, err
	}
	runner.mu.Lock()
	runner.coord = coordinator
	runner.mu.Unlock()
	return coordinator, nil
}

func (runner *qaInvestigator) newCoordinator(
	request agent.InvestigationRequest,
	parentRunID string,
) (*investigation.Coordinator, error) {
	base, err := runner.coordinator()
	if err != nil {
		return nil, err
	}
	settings := runner.platform.Settings()
	coordinator, err := runner.platform.buildInvestigationCoordinator(&settings, base.Store)
	if err != nil {
		return nil, err
	}
	coordinator.Lease = base.Lease
	coordinator.Observer = runner.progressObserver(request.WorkflowRunID, parentRunID)
	return coordinator, nil
}

func (runner *qaInvestigator) progressObserver(
	workflowRunID string,
	parentRunID string,
) investigation.ProgressObserver {
	return func(event investigation.ProgressEvent) {
		if runner == nil {
			return
		}
		if event.Kind == investigation.ProgressTaskCompleted && isAgentExecutor(event.Executor) {
			switch event.Status {
			case string(investigation.TaskSucceeded):
			case string(investigation.TaskPartial):
				log.Warnf(
					"[qa] investigation agent completed partially workflow=%s parent=%s node=%s executor=%s reason=%s",
					workflowRunID, parentRunID, event.NodeID, event.Executor, event.Reason,
				)
			default:
				log.Errorf(
					"[qa] investigation agent failed workflow=%s parent=%s node=%s executor=%s status=%s reason=%s",
					workflowRunID, parentRunID, event.NodeID, event.Executor, event.Status, event.Reason,
				)
			}
		}
		if runner.events == nil {
			return
		}
		projected := run.ExecutionEvent{
			RunID: parentRunID, WorkflowRunID: workflowRunID,
			NodeID: event.NodeID, Strategy: "multi_agent",
		}
		switch event.Kind {
		case investigation.ProgressWorkflowStarted:
			projected.Status = "running"
			runner.events.EmitEvent(run.EventWorkflowStarted, projected)
		case investigation.ProgressWorkflowCompleted:
			projected.Status = event.Status
			projected.Reason = event.Reason
			runner.events.EmitEvent(run.EventWorkflowCompleted, projected)
		case investigation.ProgressTaskStarted:
			if isAgentExecutor(event.Executor) {
				projected.Status = "running"
				runner.events.EmitEvent(run.EventAgentStarted, projected)
			} else if event.ToolID != "" {
				runner.events.EmitToolStarted(parentRunID, run.ToolStartedEvent{
					Step: 0, ToolCallID: event.NodeID, Name: event.ToolID,
					WorkflowRunID: workflowRunID, NodeID: event.NodeID,
				})
			}
		case investigation.ProgressTaskCompleted:
			if isAgentExecutor(event.Executor) {
				projected.Status = event.Status
				projected.Reason = event.Reason
				runner.events.EmitEvent(run.EventAgentCompleted, projected)
			} else if event.ToolID != "" {
				runner.events.EmitToolFinished(parentRunID, run.ToolFinishedEvent{
					Step: 0, ToolCallID: event.NodeID, Tool: event.ToolID,
					Summary: event.Reason, Failed: event.Status != string(investigation.TaskSucceeded),
					WorkflowRunID: workflowRunID, NodeID: event.NodeID,
				})
			} else if event.Status == string(investigation.TaskSucceeded) {
				projected.Status = "completed"
				runner.events.EmitEvent(run.EventEvidenceJoined, projected)
			}
		}
	}
}

func isAgentExecutor(executor investigation.ExecutorType) bool {
	switch executor {
	case investigation.ExecutorInvestigator,
		investigation.ExecutorVerifier,
		investigation.ExecutorComposer:
		return true
	default:
		return false
	}
}

func (runner *qaInvestigator) track(runID string, state *investigationState) error {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.runs == nil {
		runner.runs = make(map[string]*investigationState)
	}
	if existing := runner.runs[runID]; existing != nil {
		select {
		case <-existing.done:
			// A completed local entry is safe to replace only after the durable
			// preflight in Start has found no terminal snapshot.
		default:
			return fmt.Errorf("%w: investigation workflow %q is already active", investigation.ErrInvalidTransition, runID)
		}
	}
	runner.runs[runID] = state
	return nil
}

func (runner *qaInvestigator) complete(
	runID string,
	completed *investigationState,
	terminal agent.InvestigationTerminal,
	err error,
) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.runs == nil {
		return
	}
	state := runner.runs[runID]
	if state != completed {
		log.Warnf("[qa] ignored stale investigation completion workflow=%s", runID)
		return
	}
	state.terminal = terminal
	state.err = err
}

func (runner *qaInvestigator) remove(runID string, completed *investigationState) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.runs != nil && runner.runs[runID] == completed {
		delete(runner.runs, runID)
	}
}

func (runner *qaInvestigator) state(runID string) (*investigationState, bool) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	state, ok := runner.runs[runID]
	return state, ok
}

func taskContractFromInvestigationContract(contract investigation.InvestigationContract) agent.TaskContract {
	entities := make([]agent.EntityRef, 0, len(contract.EntityDetails))
	for _, entity := range contract.EntityDetails {
		entities = append(entities, agent.EntityRef{
			ID: entity.ID, Label: entity.Label, Role: entity.Role,
			Aliases: append([]string(nil), entity.Aliases...),
		})
	}
	if len(entities) == 0 {
		entities = make([]agent.EntityRef, 0, len(contract.Entities))
		for _, id := range contract.Entities {
			entities = append(entities, agent.EntityRef{ID: id})
		}
	}
	investigationGoals := make([]agent.InvestigationGoal, 0, len(contract.InvestigationGoals))
	for _, goal := range contract.InvestigationGoals {
		investigationGoals = append(investigationGoals, agent.InvestigationGoal{
			ID: goal.ID, Objective: goal.Objective,
			IndependentlyUseful: goal.IndependentlyUseful,
			DependsOn:           append([]string(nil), goal.DependsOn...),
		})
	}
	evidenceGoals := make([]agent.EvidenceGoal, 0, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		facets := append([]string(nil), goal.Facets...)
		facet := goal.Kind
		evidenceGoals = append(evidenceGoals, agent.EvidenceGoal{
			ID: goal.ID, Facet: facet, Facets: facets, Required: goal.Required,
			Sources:         append([]agentapi.EvidenceSource(nil), goal.Sources...),
			RequiredSources: append([]agentapi.EvidenceSource(nil), goal.RequiredSources...),
			Freshness:       goal.Freshness, MinimumCoverage: goal.MinimumCoverage, HighRisk: goal.HighRisk,
		})
	}
	conversationRefs := make([]agent.ConversationRef, 0, len(contract.Context.ConversationRefs))
	for _, ref := range contract.Context.ConversationRefs {
		conversationRefs = append(conversationRefs, agent.ConversationRef{
			SessionID: ref.SessionID, RunID: ref.RunID, Turn: ref.Turn,
		})
	}
	var timeRange *agent.TaskTimeRange
	if contract.Context.TimeRange != nil {
		timeRange = &agent.TaskTimeRange{
			From: contract.Context.TimeRange.From, To: contract.Context.TimeRange.To,
			ToExclusive: contract.Context.TimeRange.ToExclusive,
		}
	}
	return agent.TaskContract{
		TaskID:             contract.TaskID,
		Objective:          contract.Question,
		Entities:           entities,
		InvestigationGoals: investigationGoals,
		EvidenceGoals:      evidenceGoals,
		Context: agent.TaskContext{
			ConversationRefs: conversationRefs,
			TimeRange:        timeRange,
			SeedMaterial:     append([]agentapi.ContextBlock(nil), contract.Context.SeedMaterial...),
		},
	}
}

func sameInvestigationContract(left, right investigation.InvestigationContract) bool {
	left.CreatedAt = time.Time{}
	right.CreatedAt = time.Time{}
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func contractFromTaskContract(request agent.InvestigationRequest) investigation.InvestigationContract {
	contract := request.Contract
	investigationGoals := make([]investigation.InvestigationGoal, 0, len(contract.InvestigationGoals))
	for _, goal := range contract.InvestigationGoals {
		investigationGoals = append(investigationGoals, investigation.InvestigationGoal{
			ID: goal.ID, Objective: goal.Objective,
			IndependentlyUseful: goal.IndependentlyUseful,
			DependsOn:           append([]string(nil), goal.DependsOn...),
		})
	}
	evidenceGoals := make([]investigation.EvidenceGoal, 0, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		facets := append([]string(nil), goal.Facets...)
		kind := goal.Facet
		evidenceGoals = append(evidenceGoals, investigation.EvidenceGoal{
			ID:              goal.ID,
			Kind:            kind,
			Description:     goal.ID,
			Facets:          facets,
			Sources:         append([]agentapi.EvidenceSource(nil), goal.Sources...),
			RequiredSources: append([]agentapi.EvidenceSource(nil), goal.RequiredSources...),
			Freshness:       goal.Freshness,
			Required:        goal.Required,
			HighRisk:        goal.HighRisk,
			MinimumCoverage: goal.MinimumCoverage,
		})
	}
	entities := make([]string, 0, len(contract.Entities))
	entityDetails := make([]investigation.InvestigationEntity, 0, len(contract.Entities))
	for _, entity := range contract.Entities {
		entities = append(entities, entity.ID)
		entityDetails = append(entityDetails, investigation.InvestigationEntity{
			ID: entity.ID, Label: entity.Label, Role: entity.Role,
			Aliases: append([]string(nil), entity.Aliases...),
		})
	}
	conversationRefs := make([]investigation.InvestigationConversationRef, 0, len(contract.Context.ConversationRefs))
	for _, ref := range contract.Context.ConversationRefs {
		conversationRefs = append(conversationRefs, investigation.InvestigationConversationRef{
			SessionID: ref.SessionID, RunID: ref.RunID, Turn: ref.Turn,
		})
	}
	var timeRange *investigation.InvestigationTimeRange
	if contract.Context.TimeRange != nil {
		timeRange = &investigation.InvestigationTimeRange{
			From: contract.Context.TimeRange.From, To: contract.Context.TimeRange.To,
			ToExclusive: contract.Context.TimeRange.ToExclusive,
		}
	}
	seed := make([]investigation.EvidenceUnit, 0, len(request.SeedEvidence))
	for _, unit := range request.SeedEvidence {
		section := ""
		if len(unit.Sections) > 0 {
			section = unit.Sections[0]
		}
		seed = append(seed, investigation.EvidenceUnit{
			SourceKind:    unit.SourceKind,
			Target:        unit.Target,
			Section:       section,
			ContentHash:   unit.ContentHash,
			Facets:        append([]string(nil), unit.Facets...),
			TrustTier:     unit.TrustTier,
			EvidenceClass: unit.EvidenceClass,
			Version:       unit.Version,
			TimeRange:     unit.TimeRange,
		})
	}
	round := request.Round
	if round <= 0 {
		round = 1
	}
	return investigation.InvestigationContract{
		ID:            request.WorkflowRunID,
		Version:       investigation.InvestigationContractVersion,
		ParentRunID:   request.ParentRunID,
		TaskID:        contract.TaskID,
		Round:         round,
		BaseDepth:     request.BaseDepth,
		Actor:         request.Actor,
		Entities:      entities,
		EntityDetails: entityDetails,
		Context: investigation.InvestigationContext{
			ConversationRefs: conversationRefs,
			TimeRange:        timeRange,
			SeedMaterial:     append([]agentapi.ContextBlock(nil), contract.Context.SeedMaterial...),
		},
		Question:           contract.Objective,
		InvestigationGoals: investigationGoals,
		EvidenceGoals:      evidenceGoals,
		SeedEvidence:       seed,
		CreatedAt:          time.Now().UTC(),
	}
}

func investigationTerminal(
	run investigation.InvestigationRun,
	runErr error,
) (agent.InvestigationTerminal, error) {
	if err := investigation.ValidateContractVersion(run.Contract); err != nil {
		return agent.InvestigationTerminal{}, fmt.Errorf("map investigation run %q: %w", run.ID, err)
	}
	if runErr != nil && run.Delivery == nil {
		return agent.InvestigationTerminal{
			WorkflowRunID: run.ID,
			Status:        agent.InvestigationFailed,
			ErrorCode:     errorCode(run, runErr),
		}, nil
	}
	round := run.Contract.Round
	if round <= 0 {
		round = 1
	}
	terminal := agent.InvestigationTerminal{
		WorkflowRunID: run.ID,
		Round:         round,
		BaseDepth:     run.Contract.BaseDepth,
		StopReason:    stopReason(run),
		Usage:         investigationUsage(run),
	}
	switch run.Status {
	case investigation.RunDelivered:
		if run.Delivery == nil {
			terminal.Status = agent.InvestigationFailed
			terminal.ErrorCode = "missing_delivery"
			break
		}
		terminal.Output = investigationResult(run, run.Delivery)
		terminal.Completeness = investigationCompleteness(run.Delivery.Status)
		switch run.Delivery.Status {
		case investigation.DeliverySucceeded:
			terminal.Status = agent.InvestigationSucceeded
		case investigation.DeliveryPartial:
			// Partial is a valid, user-readable result. Keep the incomplete
			// completeness on the terminal instead of turning it into a
			// transport failure that hides the answer from QA.
			terminal.Status = agent.InvestigationSucceeded
		case investigation.DeliveryEvidenceInsufficient:
			terminal.Status = agent.InvestigationFailed
			terminal.ErrorCode = "evidence_insufficient"
		case investigation.DeliveryFailed:
			terminal.Status = agent.InvestigationFailed
			terminal.ErrorCode = "delivery_failed"
			if run.Delivery.Failure != nil && run.Delivery.Failure.Code != "" {
				terminal.ErrorCode = string(run.Delivery.Failure.Code)
			}
		}
	case investigation.RunCancelled:
		terminal.Status = agent.InvestigationCancelled
	case investigation.RunTimedOut:
		terminal.Status = agent.InvestigationTimedOut
	case investigation.RunBudgetExhausted:
		terminal.Status = agent.InvestigationFailed
		terminal.ErrorCode = "budget_exhausted"
	case investigation.RunFailed:
		terminal.Status = agent.InvestigationFailed
		terminal.ErrorCode = errorCode(run, runErr)
	default:
		return agent.InvestigationTerminal{}, fmt.Errorf(
			"investigation run %q is not terminal",
			run.ID,
		)
	}
	if terminal.Output == nil && terminal.Status == agent.InvestigationSucceeded {
		return agent.InvestigationTerminal{}, fmt.Errorf(
			"investigation run %q succeeded without delivery",
			run.ID,
		)
	}
	return terminal, nil
}

func investigationResult(
	run investigation.InvestigationRun,
	delivery *investigation.DeliveryResult,
) *agent.InvestigationResult {
	report := delivery.Report
	partialEvidenceGoals, unresolvedEvidenceGoals := reportEvidenceGoalStatus(report)
	result := &agent.InvestigationResult{
		Answer:                  delivery.Text,
		EvidenceUnits:           evidenceUnits(report.Evidence),
		EvidenceConflicts:       append([]agentapi.EvidenceConflict(nil), report.EvidenceConflicts...),
		PartialEvidenceGoals:    partialEvidenceGoals,
		UnresolvedEvidenceGoals: unresolvedEvidenceGoals,
		WorkflowCompleteness:    string(investigationCompleteness(delivery.Status)),
		Round:                   run.Contract.Round,
		BaseDepth:               run.Contract.BaseDepth,
	}
	if len(report.Gaps) > 0 {
		result.Limitations = []string{"some requested areas lack verified evidence"}
	}
	if delivery.Failure != nil {
		result.FailureReason = delivery.Failure.Message
	}
	for index, claim := range report.Claims {
		item := agent.InvestigationClaim{
			ProducerNodeID:     claim.VerifierTaskID,
			FindingIndex:       index,
			Claim:              claim.Text,
			EvidenceGoalIDs:    []string{claim.GoalID},
			Evidence:           claimEvidence(claim.EvidenceRefs, report.Evidence),
			EvidenceIdentities: evidenceIdentities(claim.EvidenceRefs, report.Evidence),
			Confidence:         claim.Confidence,
			Support:            string(claim.Status),
		}
		switch claim.Status {
		case investigation.ClaimSupported:
			result.SupportedClaims = append(result.SupportedClaims, item)
		case investigation.ClaimPartial, investigation.ClaimConflicting:
			result.PartialClaims = append(result.PartialClaims, item)
		case investigation.ClaimRejected:
			result.UnsupportedClaims = append(result.UnsupportedClaims, agent.InvestigationUnsupportedClaim{
				ProducerNodeID:  claim.VerifierTaskID,
				FindingIndex:    index,
				EvidenceGoalIDs: []string{claim.GoalID},
				Support:         string(claim.Status),
				ReasonCode:      "verifier_rejected",
			})
		}
	}
	return result
}

func reportEvidenceGoalStatus(report investigation.InvestigationReport) ([]string, []string) {
	partial := make([]string, 0)
	unresolved := make([]string, 0)
	for _, coverage := range report.Coverage {
		switch coverage.Status {
		case investigation.GoalPartial:
			partial = append(partial, coverage.GoalID)
		case investigation.GoalUnresolved:
			unresolved = append(unresolved, coverage.GoalID)
		}
	}
	if len(report.Coverage) == 0 {
		unresolved = gapGoalIDs(report.Gaps)
	}
	return uniqueGoalIDs(partial), uniqueGoalIDs(unresolved)
}

func uniqueGoalIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func investigationCompleteness(status investigation.DeliveryStatus) agent.InvestigationCompleteness {
	switch status {
	case investigation.DeliverySucceeded:
		return agent.InvestigationComplete
	case investigation.DeliveryPartial:
		return agent.InvestigationPartial
	case investigation.DeliveryEvidenceInsufficient:
		return agent.InvestigationPartial
	default:
		return agent.InvestigationUnavailable
	}
}

func evidenceUnits(units []investigation.EvidenceUnit) []tool.EvidenceUnit {
	out := make([]tool.EvidenceUnit, 0, len(units))
	for _, unit := range units {
		sections := make([]string, 0, 1)
		if strings.TrimSpace(unit.Section) != "" {
			sections = append(sections, unit.Section)
		}
		out = append(out, tool.EvidenceUnit{
			SourceKind:    unit.SourceKind,
			Target:        unit.Target,
			Sections:      sections,
			ContentHash:   unit.ContentHash,
			Facets:        append([]string(nil), unit.Facets...),
			TrustTier:     unit.TrustTier,
			EvidenceClass: unit.EvidenceClass,
			Version:       unit.Version,
			TimeRange:     unit.TimeRange,
		})
	}
	return out
}

func claimEvidence(
	refs []investigation.EvidenceRef,
	units []investigation.EvidenceUnit,
) []agentqa.InvestigationEvidence {
	byID := make(map[string]investigation.EvidenceUnit, len(units))
	for _, unit := range units {
		byID[unit.ID] = unit
	}
	out := make([]agentqa.InvestigationEvidence, 0, len(refs))
	for _, ref := range refs {
		summary := ref.Section
		if unit, ok := byID[ref.EvidenceID]; ok {
			summary = unit.Content
		}
		out = append(out, agentqa.InvestigationEvidence{
			Kind: ref.SourceKind, Reference: ref.Target, Summary: summary,
		})
	}
	return out
}

func evidenceIdentities(
	refs []investigation.EvidenceRef,
	units []investigation.EvidenceUnit,
) []agentapi.EvidenceIdentity {
	byID := make(map[string]investigation.EvidenceUnit, len(units))
	for _, unit := range units {
		byID[unit.ID] = unit
	}
	out := make([]agentapi.EvidenceIdentity, 0, len(refs))
	for _, ref := range refs {
		unit, ok := byID[ref.EvidenceID]
		if !ok {
			continue
		}
		out = append(out, agentapi.EvidenceIdentity{
			SourceKind: unit.SourceKind,
			Target:     unit.Target,
			Section:    unit.Section,
			Version:    unit.Version,
			TimeRange:  unit.TimeRange,
		})
	}
	return out
}

func gapGoalIDs(gaps []investigation.EvidenceGap) []string {
	out := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		out = append(out, gap.GoalID)
	}
	return out
}

func investigationUsage(run investigation.InvestigationRun) agent.InvestigationUsage {
	used := run.Budget.Run.Used
	totalTokens := used.TotalTokens
	if totalTokens == 0 {
		totalTokens = used.InputTokens + used.OutputTokens
	}
	return agent.InvestigationUsage{
		InputTokens: used.InputTokens, OutputTokens: used.OutputTokens,
		TotalTokens: totalTokens, ToolCalls: int64(used.ToolCalls), CostMicros: used.CostMicros,
	}
}

func investigationBudget(limit investigation.BudgetVector) agent.InvestigationBudget {
	return agent.InvestigationBudget{
		InputTokens: limit.InputTokens, OutputTokens: limit.OutputTokens,
		TotalTokens: limit.TotalTokens, ToolCalls: int64(limit.ToolCalls), CostMicros: limit.CostMicros,
	}
}

func stopReason(run investigation.InvestigationRun) string {
	if run.Delivery != nil {
		if run.Delivery.Failure != nil {
			return string(run.Delivery.Failure.Code)
		}
		if run.Delivery.Status == investigation.DeliveryEvidenceInsufficient {
			return "evidence_insufficient"
		}
	}
	if run.Failure != nil {
		return string(run.Failure.Code)
	}
	return ""
}

func errorCode(run investigation.InvestigationRun, runErr error) string {
	if run.Failure != nil {
		return string(run.Failure.Code)
	}
	var failure *investigation.RunFailureError
	if errors.As(runErr, &failure) {
		return string(failure.Failure.Code)
	}
	return string(investigation.FailureExecution)
}
