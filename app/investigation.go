package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/log"
)

type platformQAInvestigationRunner struct {
	platform *Platform
	events   run.ExecutionEventEmitter
}

func (runner platformQAInvestigationRunner) Available() bool {
	_, _, err := runner.startCapability()
	return err == nil
}

func (runner platformQAInvestigationRunner) Start(
	ctx context.Context,
	request agent.InvestigationRequest,
) error {
	service, version, err := runner.startCapability()
	if err != nil {
		return err
	}
	workflowRef := workflow.DefinitionRef{
		ID: workflow.DelegatedInvestigationID, Version: version,
	}
	if len(request.Contract.EvidenceGoals) > 0 {
		definition, err := runner.platform.delegatedInvestigationWorkflowForGoals(
			ctx,
			version,
			request.Contract.EvidenceGoals,
		)
		if err != nil {
			return fmt.Errorf("prepare QA investigation workflow: %w", err)
		}
		if err := service.PublishDefinitionsAs(
			ctx,
			[]workflow.WorkflowDefinition{definition},
			request.Actor.UserID,
			true,
		); err != nil {
			return fmt.Errorf(
				"publish QA investigation workflow %q version %d: %w",
				definition.ID,
				definition.Version,
				err,
			)
		}
		workflowRef = workflow.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		}
	}
	input, err := json.Marshal(request.Contract)
	if err != nil {
		return fmt.Errorf("marshal QA task contract: %w", err)
	}
	events, unsubscribe, err := service.SubscribeEvents(request.WorkflowRunID)
	if err != nil {
		return fmt.Errorf(
			"subscribe QA investigation workflow %q: %w",
			request.WorkflowRunID,
			err,
		)
	}
	stop := make(chan struct{})
	completed := make(chan struct{})
	var bridge sync.WaitGroup
	bridge.Add(1)
	go func() {
		defer bridge.Done()
		defer unsubscribe()
		bridgeInvestigationEvents(
			events,
			stop,
			completed,
			runner.events,
			request.Contract.TaskID,
		)
	}()
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	_, startErr := service.Start(ctx, workflow.StartRequest{
		RunID: request.WorkflowRunID, ParentRunID: request.Contract.TaskID,
		Workflow: workflowRef,
		Input:    input, Actor: request.Actor, ActorPermissions: readOnly,
		SeedEvidence: request.Contract.Context.SeedEvidence,
		Scenario:     workflow.DelegatedInvestigationID, ScenarioPermissions: readOnly,
	})
	if startErr == nil {
		awaitCtx := context.WithoutCancel(ctx)
		go func() {
			defer close(completed)
			if _, err := service.AwaitTerminal(
				awaitCtx,
				request.WorkflowRunID,
			); err != nil {
				log.ErrorfCtx(
					awaitCtx,
					"[qa] await workflow %s event projection completion: %v",
					request.WorkflowRunID,
					err,
				)
			}
		}()
		return nil
	}
	close(stop)
	bridge.Wait()
	return fmt.Errorf(
		"start QA investigation workflow version %d: %w",
		version,
		startErr,
	)
}

func (p *Platform) delegatedInvestigationWorkflowForGoals(
	ctx context.Context,
	version int64,
	goals []agent.EvidenceGoal,
) (workflow.WorkflowDefinition, error) {
	if p == nil || p.agents.catalog == nil ||
		p.agents.schemas == nil || p.agents.capabilities == nil {
		return workflow.WorkflowDefinition{}, workflow.ErrUnavailable
	}
	ids := []string{
		"investigator.code",
		"investigator.runtime",
		"investigator.docs",
		"synthesizer",
	}
	definitions := make([]agentapi.Definition, 0, len(ids))
	for _, id := range ids {
		definition, err := p.agents.catalog.Resolve(agentapi.DefinitionRef{
			ID: id, Version: version,
		})
		if err != nil {
			return workflow.WorkflowDefinition{}, fmt.Errorf(
				"resolve delegated investigation agent %q version %d: %w",
				id,
				version,
				err,
			)
		}
		definitions = append(definitions, definition)
	}
	budgets, err := delegatedInvestigationBudgetPolicy(definitions)
	if err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	workflowGoals := make([]workflow.DelegatedInvestigationGoal, 0, len(goals))
	for _, goal := range goals {
		workflowGoals = append(workflowGoals, workflow.DelegatedInvestigationGoal{
			Facet: goal.Facet, Required: goal.Required,
		})
	}
	proposal, err := workflow.DelegatedInvestigationProposalForGoals(workflowGoals)
	if err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	policy, err := workflow.DelegatedInvestigationCompilationPolicyForGoals(
		version,
		definitions[0].Budget.Timeout,
		budgets,
		workflowGoals,
	)
	if err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	compiler, err := workflow.NewProposalCompiler(
		p.agents.schemas,
		p.agents.capabilities,
	)
	if err != nil {
		return workflow.WorkflowDefinition{}, err
	}
	return compiler.CompileContext(ctx, proposal, policy)
}

func (runner platformQAInvestigationRunner) AwaitTerminal(
	ctx context.Context,
	workflowRunID string,
) (agent.InvestigationTerminal, error) {
	service, err := runner.workflowService()
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	terminal, err := service.AwaitTerminal(ctx, workflowRunID)
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	return investigationTerminal(terminal)
}

func (runner platformQAInvestigationRunner) LoadTerminal(
	ctx context.Context,
	workflowRunID string,
) (agent.InvestigationTerminal, error) {
	service, err := runner.workflowService()
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	terminal, err := service.LoadTerminalResult(ctx, workflowRunID)
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	return investigationTerminal(terminal)
}

func (runner platformQAInvestigationRunner) Cancel(
	ctx context.Context,
	workflowRunID string,
	userID int64,
) error {
	service, err := runner.workflowService()
	if err != nil {
		return err
	}
	if _, err := service.Cancel(ctx, workflowRunID, userID, false); err != nil {
		return err
	}
	return nil
}

func (runner platformQAInvestigationRunner) startCapability() (
	*workflow.Service,
	int64,
	error,
) {
	if runner.platform == nil {
		return nil, 0, workflow.ErrUnavailable
	}
	p := runner.platform
	p.qa.reload.RLock()
	defer p.qa.reload.RUnlock()
	if p.flow.service == nil ||
		!p.flow.service.ExecutionAvailable() ||
		p.agents.runtime == nil ||
		p.agents.version <= 0 ||
		p.qa.runs == nil {
		return nil, 0, workflow.ErrUnavailable
	}
	return p.flow.service, p.agents.version, nil
}

func (runner platformQAInvestigationRunner) workflowService() (*workflow.Service, error) {
	if runner.platform == nil || runner.platform.flow.service == nil {
		return nil, workflow.ErrUnavailable
	}
	return runner.platform.flow.service, nil
}

func investigationTerminal(
	terminal workflow.TerminalResult,
) (agent.InvestigationTerminal, error) {
	result := agent.InvestigationTerminal{
		WorkflowRunID: terminal.Run.ID,
		ErrorCode:     terminal.Run.ErrorCode,
		Usage: agent.InvestigationUsage{
			InputTokens:     terminal.Run.Usage.InputTokens,
			OutputTokens:    terminal.Run.Usage.OutputTokens,
			ReasoningTokens: terminal.Run.Usage.ReasoningTokens,
			TotalTokens:     terminal.Run.Usage.TotalTokens,
			ToolCalls:       terminal.Run.Usage.ToolCalls,
			CostMicros:      terminal.Run.Usage.CostMicros,
		},
	}
	switch terminal.Run.Status {
	case workflow.RunSucceeded:
		result.Status = agent.InvestigationSucceeded
		if terminal.Output == nil {
			return agent.InvestigationTerminal{}, fmt.Errorf(
				"QA investigation workflow %q succeeded without output",
				terminal.Run.ID,
			)
		}
		var output agent.InvestigationResult
		if err := json.Unmarshal(terminal.Output.Payload, &output); err != nil {
			return agent.InvestigationTerminal{}, fmt.Errorf(
				"decode QA investigation workflow %q output: %w",
				terminal.Run.ID,
				err,
			)
		}
		result.Output = &output
	case workflow.RunFailed:
		result.Status = agent.InvestigationFailed
	case workflow.RunCancelled:
		result.Status = agent.InvestigationCancelled
	case workflow.RunTimedOut:
		result.Status = agent.InvestigationTimedOut
	default:
		return agent.InvestigationTerminal{}, fmt.Errorf(
			"QA investigation workflow %q has non-terminal status %q",
			terminal.Run.ID,
			terminal.Run.Status,
		)
	}
	return result, nil
}

func bridgeInvestigationEvents(
	events <-chan workflow.Event,
	stop <-chan struct{},
	completed <-chan struct{},
	emitter run.ExecutionEventEmitter,
	parentRunID string,
) {
	for {
		select {
		case event := <-events:
			emitInvestigationEvent(emitter, parentRunID, event)
			if investigationTerminalEvent(event.Kind) {
				return
			}
		case <-completed:
			return
		case <-stop:
			return
		}
	}
}

func investigationTerminalEvent(kind string) bool {
	switch kind {
	case "workflow_succeeded", "workflow_failed",
		"workflow_cancelled", "workflow_timed_out":
		return true
	default:
		return false
	}
}

func emitInvestigationEvent(
	emitter run.ExecutionEventEmitter,
	parentRunID string,
	event workflow.Event,
) {
	if emitter == nil {
		return
	}
	eventType, projected, ok := projectInvestigationEvent(parentRunID, event)
	if ok {
		emitter.EmitExecutionEvent(eventType, projected)
	}
}

func projectInvestigationEvent(
	parentRunID string,
	event workflow.Event,
) (run.EventType, run.ExecutionEvent, bool) {
	projected := run.ExecutionEvent{
		RunID: parentRunID, WorkflowRunID: event.WorkflowRunID,
		NodeID: event.NodeID, Strategy: "multi_agent",
	}
	switch event.Kind {
	case "workflow_started":
		projected.Status = "running"
		return run.EventWorkflowStarted, projected, true
	case "node_started":
		if investigationAgentNode(event.NodeID) {
			projected.Status = "running"
			return run.EventAgentStarted, projected, true
		}
	case "node_succeeded":
		switch {
		case investigationAgentNode(event.NodeID):
			projected.Status = "completed"
			return run.EventAgentCompleted, projected, true
		case event.NodeID == "evidence.join":
			projected.Status = "completed"
			return run.EventEvidenceJoined, projected, true
		}
	case "node_failed":
		if investigationAgentNode(event.NodeID) {
			projected.Status = "failed"
			projected.Reason = event.Summary
			return run.EventAgentCompleted, projected, true
		}
	}
	return "", run.ExecutionEvent{}, false
}

func investigationAgentNode(nodeID string) bool {
	switch nodeID {
	case "investigate.code", "investigate.runtime", "investigate.docs", "synthesize":
		return true
	default:
		return false
	}
}
