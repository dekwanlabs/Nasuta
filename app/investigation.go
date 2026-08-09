package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
)

type platformQAInvestigationRunner struct {
	platform *Platform
	events   agentrun.ExecutionEventEmitter
}

func (runner platformQAInvestigationRunner) Available() bool {
	if runner.platform == nil {
		return false
	}
	p := runner.platform
	p.qa.reload.RLock()
	defer p.qa.reload.RUnlock()
	return p.flow.service.ExecutionAvailable() &&
		p.agents.runtime != nil && p.agents.version > 0
}

func (runner platformQAInvestigationRunner) Run(
	ctx context.Context,
	request agent.InvestigationRequest,
) (agent.InvestigationResult, error) {
	if runner.platform == nil {
		return agent.InvestigationResult{}, workflow.ErrUnavailable
	}
	p := runner.platform
	p.qa.reload.RLock()
	defer p.qa.reload.RUnlock()
	if !p.flow.service.ExecutionAvailable() ||
		p.agents.runtime == nil || p.agents.version <= 0 {
		return agent.InvestigationResult{}, workflow.ErrUnavailable
	}
	input, err := json.Marshal(struct {
		Question string `json:"question"`
	}{Question: request.Question})
	if err != nil {
		return agent.InvestigationResult{}, fmt.Errorf("marshal QA investigation request: %w", err)
	}
	events, unsubscribe, err := p.flow.service.SubscribeEvents(request.WorkflowRunID)
	if err != nil {
		return agent.InvestigationResult{}, fmt.Errorf("subscribe QA investigation workflow %q: %w", request.WorkflowRunID, err)
	}
	stop := make(chan struct{})
	var bridge sync.WaitGroup
	bridge.Add(1)
	go func() {
		defer bridge.Done()
		bridgeInvestigationEvents(events, stop, runner.events, request.ParentRunID)
	}()
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	workflowResult, executeErr := p.flow.service.Execute(ctx, workflow.ExecuteRequest{
		RunID: request.WorkflowRunID, ParentRunID: request.ParentRunID,
		Workflow: workflow.DefinitionRef{
			ID: workflow.DelegatedInvestigationID, Version: p.agents.version,
		},
		Input: input, Actor: request.Actor, ActorPermissions: readOnly,
		Scenario: workflow.DelegatedInvestigationID, ScenarioPermissions: readOnly,
	})
	unsubscribe()
	close(stop)
	bridge.Wait()
	if executeErr != nil {
		return agent.InvestigationResult{}, fmt.Errorf("execute QA investigation workflow version %d: %w", p.agents.version, executeErr)
	}
	var result agent.InvestigationResult
	if err := json.Unmarshal(workflowResult.Output.Payload, &result); err != nil {
		return agent.InvestigationResult{}, fmt.Errorf("decode QA investigation answer: %w", err)
	}
	result.WorkflowRunID = workflowResult.RunID
	result.Usage = agent.InvestigationUsage{
		InputTokens: workflowResult.Usage.InputTokens, OutputTokens: workflowResult.Usage.OutputTokens,
		ReasoningTokens: workflowResult.Usage.ReasoningTokens, TotalTokens: workflowResult.Usage.TotalTokens,
		ToolCalls: workflowResult.Usage.ToolCalls, CostMicros: workflowResult.Usage.CostMicros,
	}
	return result, nil
}

func bridgeInvestigationEvents(
	events <-chan workflow.Event,
	stop <-chan struct{},
	emitter agentrun.ExecutionEventEmitter,
	parentRunID string,
) {
	for {
		select {
		case event := <-events:
			emitInvestigationEvent(emitter, parentRunID, event)
		case <-stop:
			for {
				select {
				case event := <-events:
					emitInvestigationEvent(emitter, parentRunID, event)
				default:
					return
				}
			}
		}
	}
}

func emitInvestigationEvent(
	emitter agentrun.ExecutionEventEmitter,
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
) (agentrun.EventType, agentrun.ExecutionEvent, bool) {
	projected := agentrun.ExecutionEvent{
		RunID: parentRunID, WorkflowRunID: event.WorkflowRunID,
		NodeID: event.NodeID, Strategy: "multi_agent",
	}
	switch event.Kind {
	case "workflow_started":
		projected.Status = "running"
		return agentrun.EventWorkflowStarted, projected, true
	case "node_started":
		if investigationAgentNode(event.NodeID) {
			projected.Status = "running"
			return agentrun.EventAgentStarted, projected, true
		}
	case "node_succeeded":
		switch {
		case investigationAgentNode(event.NodeID):
			projected.Status = "completed"
			return agentrun.EventAgentCompleted, projected, true
		case event.NodeID == "evidence.join":
			projected.Status = "completed"
			return agentrun.EventEvidenceJoined, projected, true
		}
	case "node_failed":
		if investigationAgentNode(event.NodeID) {
			projected.Status = "failed"
			projected.Reason = event.Summary
			return agentrun.EventAgentCompleted, projected, true
		}
	}
	return "", agentrun.ExecutionEvent{}, false
}

func investigationAgentNode(nodeID string) bool {
	switch nodeID {
	case "investigate.code", "investigate.runtime", "investigate.docs", "synthesize":
		return true
	default:
		return false
	}
}
