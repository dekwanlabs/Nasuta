package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	agentqa "github.com/dekwanlabs/nasuta/internal/agent/qa"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
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

func (runner *qaInvestigator) Available() bool {
	_, err := runner.coordinator()
	return err == nil
}

func (runner *qaInvestigator) Start(
	ctx context.Context,
	request agent.InvestigationRequest,
) error {
	parentRunID := strings.TrimSpace(request.ParentRunID)
	if parentRunID == "" {
		parentRunID = request.Contract.TaskID
	}
	coordinator, err := runner.newCoordinator(request, parentRunID)
	if err != nil {
		return err
	}
	contract := contractFromTaskContract(request)
	state := &investigationState{
		runID:       investigation.ContractRunID(contract),
		coordinator: coordinator,
		done:        make(chan struct{}),
	}
	runner.track(request.WorkflowRunID, state)
	go func() {
		defer close(state.done)
		run, runErr := coordinator.ExecuteWithProposal(
			context.WithoutCancel(ctx),
			contract,
			request.Proposal,
		)
		terminal, mapErr := investigationTerminal(run, runErr)
		runner.complete(request.WorkflowRunID, terminal, mapErr)
	}()
	return nil
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

// LoadDelivery returns the one persisted  delivery result without projecting it
// through the legacy QA terminal contract.
func (runner *qaInvestigator) LoadDelivery(
	ctx context.Context,
	workflowRunID string,
) (investigation.DeliveryResult, error) {
	run, err := runner.LoadRun(ctx, workflowRunID)
	if err != nil {
		return investigation.DeliveryResult{}, err
	}
	if run.Delivery == nil {
		return investigation.DeliveryResult{}, fmt.Errorf(
			"%w: run %q has no delivery result", investigation.ErrNoDelivery, run.ID,
		)
	}
	return *run.Delivery, nil
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
		return agent.InvestigationTerminal{}, fmt.Errorf(" investigation run id is required")
	}
	if state, ok := runner.state(workflowRunID); ok {
		select {
		case <-state.done:
			if state.err != nil {
				return agent.InvestigationTerminal{}, state.err
			}
			return state.terminal, nil
		default:
		}
	}

	coordinator, err := runner.coordinator()
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	// The process-local channel is only an optimization. A durable poll makes
	// waiting work after a restart or when another process owns execution.
	const pollInterval = 100 * time.Millisecond
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		run, loadErr := coordinator.LoadRun(ctx, workflowRunID)
		if loadErr != nil {
			return agent.InvestigationTerminal{}, loadErr
		}
		if run.Status.Terminal() {
			return investigationTerminal(run, nil)
		}
		if state, ok := runner.state(workflowRunID); ok {
			select {
			case <-state.done:
				if state.err != nil {
					return agent.InvestigationTerminal{}, state.err
				}
				return state.terminal, nil
			default:
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return agent.InvestigationTerminal{}, ctx.Err()
		}
	}
}

func (runner *qaInvestigator) LoadTerminal(
	ctx context.Context,
	workflowRunID string,
) (agent.InvestigationTerminal, error) {
	if state, ok := runner.state(workflowRunID); ok {
		select {
		case <-state.done:
			if state.err != nil {
				return agent.InvestigationTerminal{}, state.err
			}
			return state.terminal, nil
		default:
			return agent.InvestigationTerminal{}, fmt.Errorf(
				" investigation run %q is not terminal",
				workflowRunID,
			)
		}
	}
	coordinator, err := runner.coordinator()
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	run, err := coordinator.LoadRun(ctx, workflowRunID)
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	return investigationTerminal(run, nil)
}

func (runner *qaInvestigator) Cancel(
	ctx context.Context,
	workflowRunID string,
	_ int64,
) error {
	if state, ok := runner.state(workflowRunID); ok {
		if state.coordinator != nil {
			return state.coordinator.Cancel(ctx, state.runID)
		}
		workflowRunID = state.runID
	}
	coordinator, err := runner.coordinator()
	if err != nil {
		return err
	}
	return coordinator.Cancel(ctx, workflowRunID)
}

func (runner *qaInvestigator) coordinator() (*investigation.Coordinator, error) {
	if runner == nil || runner.platform == nil {
		return nil, fmt.Errorf(" investigation platform is unavailable")
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
	coordinator.Observer = runner.progressObserver(request.WorkflowRunID, parentRunID)
	return coordinator, nil
}

func (runner *qaInvestigator) progressObserver(
	workflowRunID string,
	parentRunID string,
) investigation.ProgressObserver {
	return func(event investigation.ProgressEvent) {
		if runner == nil || runner.events == nil {
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

func (runner *qaInvestigator) track(runID string, state *investigationState) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.runs == nil {
		runner.runs = make(map[string]*investigationState)
	}
	runner.runs[runID] = state
}

func (runner *qaInvestigator) complete(
	runID string,
	terminal agent.InvestigationTerminal,
	err error,
) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.runs == nil {
		runner.runs = make(map[string]*investigationState)
	}
	state := runner.runs[runID]
	if state == nil {
		state = &investigationState{done: make(chan struct{})}
		runner.runs[runID] = state
	}
	state.terminal = terminal
	state.err = err
}

func (runner *qaInvestigator) state(runID string) (*investigationState, bool) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	state, ok := runner.runs[runID]
	return state, ok
}

func contractFromTaskContract(request agent.InvestigationRequest) investigation.InvestigationContract {
	contract := request.Contract
	goals := make([]investigation.EvidenceGoal, 0, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		kind := strings.TrimSpace(goal.Facet)
		if kind == "" {
			kind = goal.ID
		}
		goals = append(goals, investigation.EvidenceGoal{
			ID:              goal.ID,
			Kind:            kind,
			Description:     goal.ID,
			Facets:          []string{kind},
			Sources:         append([]agentapi.EvidenceSource(nil), goal.Sources...),
			RequiredSources: append([]agentapi.EvidenceSource(nil), goal.RequiredSources...),
			Freshness:       goal.Freshness,
			Required:        goal.Required,
			HighRisk:        goal.HighRisk,
			MinimumCoverage: goal.MinimumCoverage,
		})
	}
	entities := make([]string, 0, len(contract.Entities))
	for _, entity := range contract.Entities {
		entities = append(entities, entity.ID)
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
	return investigation.InvestigationContract{
		ID:           request.WorkflowRunID,
		Version:      1,
		Entities:     entities,
		Question:     contract.Objective,
		Goals:        goals,
		SeedEvidence: seed,
		CreatedAt:    time.Now().UTC(),
	}
}

func investigationTerminal(
	run investigation.InvestigationRun,
	runErr error,
) (agent.InvestigationTerminal, error) {
	if runErr != nil && run.Delivery == nil {
		return agent.InvestigationTerminal{
			WorkflowRunID: run.ID,
			Status:        agent.InvestigationFailed,
			ErrorCode:     errorCode(run, runErr),
		}, nil
	}
	round := run.Metrics.Rounds
	if round <= 0 {
		round = 1
	}
	terminal := agent.InvestigationTerminal{
		WorkflowRunID: run.ID,
		Round:         round,
		StopReason:    stopReason(run),
		Usage:         investigationUsage(run),
	}
	switch run.Status {
	case investigation.RunDelivered:
		if run.Delivery != nil {
			terminal.Status = agent.InvestigationSucceeded
			terminal.Output = investigationResult(run, run.Delivery)
			terminal.Completeness = investigationCompleteness(run.Delivery.Status)
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
			" investigation run %q is not terminal",
			run.ID,
		)
	}
	if terminal.Output == nil && terminal.Status == agent.InvestigationSucceeded {
		return agent.InvestigationTerminal{}, fmt.Errorf(
			" investigation run %q succeeded without delivery",
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
	result := &agent.InvestigationResult{
		Answer:               delivery.Text,
		EvidenceUnits:        evidenceUnits(report.Evidence),
		UnresolvedGoals:      gapGoalIDs(report.Gaps),
		WorkflowCompleteness: string(investigationCompleteness(delivery.Status)),
	}
	for _, gap := range report.Gaps {
		reason := strings.TrimSpace(gap.Reason)
		if reason == "" {
			reason = "no verified claim covers this goal"
		}
		result.Limitations = append(result.Limitations, gap.GoalID+": "+reason)
	}
	if delivery.Failure != nil {
		result.FailureReason = delivery.Failure.Message
	}
	for index, claim := range report.Claims {
		item := agent.InvestigationClaim{
			ProducerNodeID:     claim.VerifierTaskID,
			FindingIndex:       index,
			Claim:              claim.Text,
			GoalIDs:            []string{claim.GoalID},
			Evidence:           claimEvidence(claim.EvidenceRefs, report.Evidence),
			EvidenceIdentities: evidenceIdentities(claim.EvidenceRefs, report.Evidence),
			Confidence:         claim.Confidence,
			Support:            string(claim.Status),
		}
		if claim.Status == investigation.ClaimSupported {
			result.SupportedClaims = append(result.SupportedClaims, item)
		} else {
			result.PartialClaims = append(result.PartialClaims, item)
		}
	}
	return result
}

func investigationCompleteness(status investigation.DeliveryStatus) agent.InvestigationCompleteness {
	switch status {
	case investigation.DeliverySucceeded:
		return agent.InvestigationComplete
	case investigation.DeliveryPartial:
		return agent.InvestigationPartial
	case investigation.DeliveryEvidenceInsufficient:
		return agent.InvestigationUnavailable
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
	var usage agent.InvestigationUsage
	for _, record := range run.Results {
		usage.InputTokens += record.Usage.InputTokens
		usage.OutputTokens += record.Usage.OutputTokens
		usage.ToolCalls += int64(record.Usage.ToolCalls)
		usage.CostMicros += record.Usage.CostMicros
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	return usage
}

func stopReason(run investigation.InvestigationRun) string {
	if run.Delivery != nil && run.Delivery.Failure != nil {
		return string(run.Delivery.Failure.Code)
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
	if runErr != nil {
		return "execution_failed"
	}
	return "execution_failed"
}
