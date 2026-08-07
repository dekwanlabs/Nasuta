package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agentworkflow"
	"github.com/dekwanlabs/nasuta/internal/investigation"
)

type platformInvestigationExecutor struct {
	platform *Platform
}

func (executor platformInvestigationExecutor) Execute(
	ctx context.Context,
	request agentworkflow.ExecuteRequest,
) (agentworkflow.Result, error) {
	if executor.platform == nil {
		return agentworkflow.Result{}, investigation.ErrUnavailable
	}
	platform := executor.platform
	platform.qaReloadMu.RLock()
	defer platform.qaReloadMu.RUnlock()
	if platform.workflowService == nil || platform.definitionRuntime == nil ||
		platform.agentDefinitionVer <= 0 {
		return agentworkflow.Result{}, investigation.ErrUnavailable
	}
	request.Workflow.Version = platform.agentDefinitionVer
	result, err := platform.workflowService.Execute(ctx, request)
	if err != nil {
		return agentworkflow.Result{}, fmt.Errorf("execute active workflow version %d: %w", request.Workflow.Version, err)
	}
	return result, nil
}

type platformQAInvestigationRunner struct {
	platform *Platform
	events   agent.ExecutionEventEmitter
}

func (runner platformQAInvestigationRunner) Available() bool {
	if runner.platform == nil {
		return false
	}
	platform := runner.platform
	platform.qaReloadMu.RLock()
	defer platform.qaReloadMu.RUnlock()
	return platform.workflowService != nil && platform.workflowRunner != nil &&
		platform.definitionRuntime != nil && platform.agentDefinitionVer > 0
}

func (runner platformQAInvestigationRunner) Run(
	ctx context.Context,
	request agent.InvestigationRequest,
) (agent.InvestigationResult, error) {
	if runner.platform == nil {
		return agent.InvestigationResult{}, investigation.ErrUnavailable
	}
	platform := runner.platform
	platform.qaReloadMu.RLock()
	defer platform.qaReloadMu.RUnlock()
	if platform.workflowService == nil || platform.workflowRunner == nil ||
		platform.definitionRuntime == nil || platform.agentDefinitionVer <= 0 {
		return agent.InvestigationResult{}, investigation.ErrUnavailable
	}
	input, err := json.Marshal(struct {
		Question string `json:"question"`
	}{Question: request.Question})
	if err != nil {
		return agent.InvestigationResult{}, fmt.Errorf("marshal QA investigation request: %w", err)
	}
	events, unsubscribe, err := platform.workflowService.SubscribeEvents(request.WorkflowRunID)
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
	workflowResult, executeErr := platform.workflowService.Execute(ctx, agentworkflow.ExecuteRequest{
		RunID: request.WorkflowRunID, ParentRunID: request.ParentRunID,
		Workflow: agentworkflow.DefinitionRef{
			ID: agentworkflow.DelegatedInvestigationID, Version: platform.agentDefinitionVer,
		},
		Input: input, Actor: request.Actor, ActorPermissions: readOnly,
		Scenario: investigation.Scenario, ScenarioPermissions: readOnly,
	})
	unsubscribe()
	close(stop)
	bridge.Wait()
	if executeErr != nil {
		return agent.InvestigationResult{}, fmt.Errorf("execute QA investigation workflow version %d: %w", platform.agentDefinitionVer, executeErr)
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
	events <-chan agentworkflow.Event,
	stop <-chan struct{},
	emitter agent.ExecutionEventEmitter,
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
	emitter agent.ExecutionEventEmitter,
	parentRunID string,
	event agentworkflow.Event,
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
	event agentworkflow.Event,
) (agent.EventType, agent.ExecutionEvent, bool) {
	projected := agent.ExecutionEvent{
		RunID: parentRunID, WorkflowRunID: event.WorkflowRunID,
		NodeID: event.NodeID, Strategy: "multi_agent",
	}
	switch event.Kind {
	case "workflow_started":
		projected.Status = "running"
		return agent.EventWorkflowStarted, projected, true
	case "node_started":
		if investigationAgentNode(event.NodeID) {
			projected.Status = "running"
			return agent.EventAgentStarted, projected, true
		}
	case "node_succeeded":
		switch {
		case investigationAgentNode(event.NodeID):
			projected.Status = "completed"
			return agent.EventAgentCompleted, projected, true
		case event.NodeID == "evidence.join":
			projected.Status = "completed"
			return agent.EventEvidenceJoined, projected, true
		}
	case "node_failed":
		if investigationAgentNode(event.NodeID) {
			projected.Status = "failed"
			projected.Reason = event.Summary
			return agent.EventAgentCompleted, projected, true
		}
	}
	return "", agent.ExecutionEvent{}, false
}

func investigationAgentNode(nodeID string) bool {
	switch nodeID {
	case "investigate.code", "investigate.runtime", "investigate.docs", "synthesize":
		return true
	default:
		return false
	}
}
