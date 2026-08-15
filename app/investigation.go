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

type qaInvestigator struct {
	platform *Platform
	events   run.ExecutionEventEmitter
}

func (runner qaInvestigator) Available() bool {
	_, _, err := runner.startCapability()
	return err == nil
}

func (runner qaInvestigator) Start(
	ctx context.Context,
	request agent.InvestigationRequest,
) error {
	service, version, err := runner.startCapability()
	if err != nil {
		return err
	}
	workflowRef := workflow.DefinitionRef{
		ID: workflow.FlowID, Version: version,
	}
	agentNodes := defaultAgentNodes()
	if len(request.Contract.EvidenceGoals) > 0 {
		definition, err := runner.platform.investigationFlow(
			ctx,
			version,
			request.Contract.EvidenceGoals,
			request.Proposal,
		)
		if err != nil {
			return fmt.Errorf("prepare QA investigation workflow: %w", err)
		}
		if err := service.PublishAs(
			ctx,
			[]workflow.Definition{definition},
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
		agentNodes = agentNodeIDs(definition)
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
			agentNodes,
		)
	}()
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	_, startErr := service.Start(ctx, workflow.StartRequest{
		RunID: request.WorkflowRunID, ParentRunID: request.Contract.TaskID,
		Workflow: workflowRef,
		Input:    input, Actor: request.Actor, ActorPermissions: readOnly,
		SeedEvidence: request.Contract.Context.SeedEvidence,
		Scenario:     workflow.FlowID, ScenarioPermissions: readOnly,
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

func (p *Platform) investigationFlow(
	ctx context.Context,
	version int64,
	goals []agent.EvidenceGoal,
	plan *agentapi.TaskGraphProposal,
) (workflow.Definition, error) {
	if p == nil || p.agents.catalog == nil ||
		p.agents.schemas == nil || p.agents.capabilities == nil {
		return workflow.Definition{}, workflow.ErrUnavailable
	}
	ids := []string{
		"investigator.code",
		"investigator.runtime",
		"investigator.docs",
		"investigator.web",
		"investigator.memory",
		"synthesizer",
	}
	definitions := make([]agentapi.Definition, 0, len(ids))
	for _, id := range ids {
		definition, err := p.agents.catalog.Resolve(agentapi.DefinitionRef{
			ID: id, Version: version,
		})
		if err != nil {
			return workflow.Definition{}, fmt.Errorf(
				"resolve investigation agent %q version %d: %w",
				id,
				version,
				err,
			)
		}
		definitions = append(definitions, definition)
	}
	budgets, err := investigationBudgets(definitions)
	if err != nil {
		return workflow.Definition{}, err
	}
	if goalsNeedSource(goals, agentapi.EvidenceSourceRuntime) {
		observe, err := p.agents.capabilities.Resolve(agentapi.CapabilityRef{
			ID: "knowledge.runtime.observe", Version: version,
		})
		if err != nil {
			return workflow.Definition{}, fmt.Errorf(
				"resolve live runtime investigation capability version %d: %w",
				version,
				err,
			)
		}
		if len(observe.ToolIDs) == 0 {
			return workflow.Definition{}, fmt.Errorf(
				"live runtime investigation capability has no tools",
			)
		}
		definition, err := p.agents.catalog.Resolve(observe.Agent)
		if err != nil {
			return workflow.Definition{}, fmt.Errorf(
				"resolve live runtime investigator: %w",
				err,
			)
		}
		budgets.Observe, err = agentNodeBudget(
			definition,
			int64(definition.Budget.MaxSteps),
		)
		if err != nil {
			return workflow.Definition{}, err
		}
	}
	flowGoals := make([]workflow.Goal, 0, len(goals))
	for _, goal := range goals {
		flowGoals = append(flowGoals, workflow.Goal{
			Facet: goal.Facet, Required: goal.Required,
			Sources: append(
				[]agentapi.EvidenceSource(nil),
				goal.Sources...,
			),
			Freshness:       goal.Freshness,
			MinimumCoverage: goal.MinimumCoverage,
			HighRisk:        goal.HighRisk,
		})
	}
	compiler, err := workflow.NewProposalCompiler(
		p.agents.schemas,
		p.agents.capabilities,
	)
	if err != nil {
		return workflow.Definition{}, err
	}
	if plan != nil {
		policy, err := workflow.PlanPolicy(
			version,
			definitions[0].Budget.Timeout,
			budgets,
			flowGoals,
			*plan,
		)
		if err != nil {
			return workflow.Definition{}, err
		}
		definition, compileErr := compiler.CompileContext(
			ctx,
			*plan,
			policy,
		)
		if compileErr == nil {
			return definition, nil
		}
		log.WarnfCtx(
			ctx,
			"[qa] planner task graph rejected; using deterministic goal mapping: %v",
			compileErr,
		)
	}
	fallbackPlan, err := workflow.BuildPlan(flowGoals)
	if err != nil {
		return workflow.Definition{}, err
	}
	policy, err := workflow.GoalPolicy(
		version,
		definitions[0].Budget.Timeout,
		budgets,
		flowGoals,
	)
	if err != nil {
		return workflow.Definition{}, err
	}
	return compiler.CompileContext(ctx, fallbackPlan, policy)
}

func goalsNeedSource(
	goals []agent.EvidenceGoal,
	source agentapi.EvidenceSource,
) bool {
	for _, goal := range goals {
		for _, candidate := range goal.Sources {
			if candidate == source {
				return true
			}
		}
	}
	return false
}

func (runner qaInvestigator) AwaitTerminal(
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

func (runner qaInvestigator) LoadTerminal(
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

func (runner qaInvestigator) Cancel(
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

func (runner qaInvestigator) startCapability() (
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
		!p.flow.service.Available() ||
		p.agents.runtime == nil ||
		p.agents.version <= 0 ||
		p.qa.runs == nil {
		return nil, 0, workflow.ErrUnavailable
	}
	return p.flow.service, p.agents.version, nil
}

func (runner qaInvestigator) workflowService() (*workflow.Service, error) {
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
		switch terminal.Output.Completeness {
		case workflow.Complete:
			result.Completeness = agent.InvestigationComplete
		case workflow.Partial:
			result.Completeness = agent.InvestigationPartial
		case workflow.Unavailable:
			result.Completeness = agent.InvestigationUnavailable
		default:
			return agent.InvestigationTerminal{}, fmt.Errorf(
				"QA investigation workflow %q has invalid output completeness %q",
				terminal.Run.ID,
				terminal.Output.Completeness,
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
	agentNodes map[string]struct{},
) {
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			emitInvestigationEvent(emitter, parentRunID, event, agentNodes)
			if isWorkflowTerminal(event.Kind) {
				return
			}
		case <-completed:
			return
		case <-stop:
			return
		}
	}
}

func isWorkflowTerminal(kind string) bool {
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
	agentNodes map[string]struct{},
) {
	if emitter == nil {
		return
	}
	eventType, projected, ok := projectInvestigationEvent(
		parentRunID,
		event,
		agentNodes,
	)
	if ok {
		emitter.EmitEvent(eventType, projected)
	}
}

func projectInvestigationEvent(
	parentRunID string,
	event workflow.Event,
	agentNodes map[string]struct{},
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
		if isAgentNode(agentNodes, event.NodeID) {
			projected.Status = "running"
			return run.EventAgentStarted, projected, true
		}
	case "node_succeeded":
		switch {
		case isAgentNode(agentNodes, event.NodeID):
			projected.Status = "completed"
			return run.EventAgentCompleted, projected, true
		case event.NodeID == "evidence.join":
			projected.Status = "completed"
			return run.EventEvidenceJoined, projected, true
		}
	case "node_failed":
		if isAgentNode(agentNodes, event.NodeID) {
			projected.Status = "failed"
			projected.Reason = event.Summary
			return run.EventAgentCompleted, projected, true
		}
	}
	return "", run.ExecutionEvent{}, false
}

func isAgentNode(
	agentNodes map[string]struct{},
	nodeID string,
) bool {
	_, ok := agentNodes[nodeID]
	return ok
}

func agentNodeIDs(
	definition workflow.Definition,
) map[string]struct{} {
	ids := make(map[string]struct{}, len(definition.Nodes))
	for _, node := range definition.Nodes {
		if node.Kind == workflow.NodeAgent {
			ids[node.ID] = struct{}{}
		}
	}
	return ids
}

func defaultAgentNodes() map[string]struct{} {
	return map[string]struct{}{
		"investigate.code":    {},
		"investigate.runtime": {},
		"investigate.docs":    {},
		"synthesize":          {},
	}
}
