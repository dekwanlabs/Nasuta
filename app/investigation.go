package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/evidence"
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
	payloadBudget := 0
	if len(request.Contract.EvidenceGoals) > 0 {
		definition, flowPayloadBudget, err := runner.platform.investigationFlowWithEvidenceSubjects(
			ctx,
			version,
			request.Contract.EvidenceGoals,
			investigationSubjectRequirements(request.Contract),
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
		payloadBudget = flowPayloadBudget
	} else {
		payloadBudget, err = runner.platform.investigatorPayloadBudget(version)
		if err != nil {
			return err
		}
	}
	input, err := marshalInvestigationContract(
		request.Contract,
		payloadBudget,
	)
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
	parentRunID := request.ParentRunID
	if strings.TrimSpace(parentRunID) == "" {
		parentRunID = request.Contract.TaskID
	}
	round := request.Round
	if round <= 0 {
		round = 1
	}
	_, startErr := service.Start(ctx, workflow.StartRequest{
		RunID: request.WorkflowRunID, ParentRunID: parentRunID,
		Round: round, BaseDepth: request.BaseDepth,
		Workflow: workflowRef,
		Input:    input, Actor: request.Actor, ActorPermissions: readOnly,
		SeedEvidence: request.SeedEvidence,
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
	definition, _, err := p.investigationFlowWithBudget(
		ctx,
		version,
		goals,
		plan,
	)
	return definition, err
}

func (p *Platform) investigationFlowWithBudget(
	ctx context.Context,
	version int64,
	goals []agent.EvidenceGoal,
	plan *agentapi.TaskGraphProposal,
) (workflow.Definition, int, error) {
	return p.investigationFlowWithEvidenceSubjects(ctx, version, goals, nil, plan)
}

func (p *Platform) investigationFlowWithEvidenceSubjects(
	ctx context.Context,
	version int64,
	goals []agent.EvidenceGoal,
	subjects []workflow.SubjectRequirement,
	plan *agentapi.TaskGraphProposal,
) (workflow.Definition, int, error) {
	if p == nil || p.agents.catalog == nil ||
		p.agents.schemas == nil || p.agents.capabilities == nil {
		return workflow.Definition{}, 0, workflow.ErrUnavailable
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
			return workflow.Definition{}, 0, fmt.Errorf(
				"resolve investigation agent %q version %d: %w",
				id,
				version,
				err,
			)
		}
		definitions = append(definitions, definition)
	}
	baseBudgets, err := investigationBudgets(definitions)
	if err != nil {
		return workflow.Definition{}, 0, err
	}
	flowGoals := make([]workflow.Goal, 0, len(goals))
	for _, goal := range goals {
		flowGoals = append(flowGoals, workflow.Goal{
			Facet: goal.Facet, Required: goal.Required,
			Sources: append(
				[]agentapi.EvidenceSource(nil),
				goal.Sources...,
			),
			RequiredSources: append(
				[]agentapi.EvidenceSource(nil),
				goal.RequiredSources...,
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
		return workflow.Definition{}, 0, err
	}
	if plan != nil {
		budgets, err := p.investigationBudgetsForPlan(
			version,
			baseBudgets,
			*plan,
		)
		if err == nil {
			var policy workflow.CompilationPolicy
			policy, err = workflow.PlanPolicy(
				version,
				definitions[0].Budget.Timeout,
				budgets,
				flowGoals,
				*plan,
			)
			if err == nil {
				policy.SubjectRequirements = cloneSubjectRequirements(subjects)
				var definition workflow.Definition
				definition, err = compiler.CompileContext(ctx, *plan, policy)
				if err == nil {
					return definition, budgets.InvestigatorPayloadTokens, nil
				}
			}
		}
		log.WarnfCtx(
			ctx,
			"[qa] planner task graph rejected; using deterministic goal mapping: %v",
			err,
		)
	}
	fallbackPlan, err := workflow.BuildPlan(flowGoals)
	if err != nil {
		return workflow.Definition{}, 0, err
	}
	isolateFallbackInvestigatorInputs(&fallbackPlan)
	budgets, err := p.investigationBudgetsForPlan(
		version,
		baseBudgets,
		fallbackPlan,
	)
	if err != nil {
		return workflow.Definition{}, 0, err
	}
	policy, err := workflow.GoalPolicy(
		version,
		definitions[0].Budget.Timeout,
		budgets,
		flowGoals,
	)
	if err != nil {
		return workflow.Definition{}, 0, err
	}
	policy.SubjectRequirements = cloneSubjectRequirements(subjects)
	definition, err := compiler.CompileContext(ctx, fallbackPlan, policy)
	return definition, budgets.InvestigatorPayloadTokens, err
}

func isolateFallbackInvestigatorInputs(
	plan *agentapi.TaskGraphProposal,
) {
	if plan == nil {
		return
	}
	for index := range plan.Tasks {
		task := &plan.Tasks[index]
		if task.OutputSchema.ID == "investigation.report" {
			task.InputRefs = []agentapi.EvidenceRef{}
		}
	}
}

func investigationSubjectRequirements(contract agent.TaskContract) []workflow.SubjectRequirement {
	if len(contract.Entities) < 2 {
		return nil
	}
	facets := make([]string, 0, len(contract.EvidenceGoals))
	sources := make(map[agentapi.EvidenceSource]struct{})
	for _, goal := range contract.EvidenceGoals {
		if !goal.Required || goal.MinimumCoverage < len(contract.Entities) {
			continue
		}
		facets = appendUniqueString(facets, goal.Facet)
		for _, source := range goal.RequiredSources {
			sources[source] = struct{}{}
		}
	}
	if len(facets) == 0 {
		return nil
	}
	requiredSources := make([]agentapi.EvidenceSource, 0, len(sources))
	for source := range sources {
		requiredSources = append(requiredSources, source)
	}
	sort.Slice(requiredSources, func(i, j int) bool { return requiredSources[i] < requiredSources[j] })
	requirements := make([]workflow.SubjectRequirement, 0, len(contract.Entities))
	for _, entity := range contract.Entities {
		requirements = append(requirements, workflow.SubjectRequirement{
			EntityID: entity.ID, Label: entity.Label, Role: entity.Role,
			Aliases:         append([]string(nil), entity.Aliases...),
			RequiredFacets:  append([]string(nil), facets...),
			RequiredSources: append([]agentapi.EvidenceSource(nil), requiredSources...),
		})
	}
	return requirements
}

func cloneSubjectRequirements(values []workflow.SubjectRequirement) []workflow.SubjectRequirement {
	if len(values) == 0 {
		return nil
	}
	out := make([]workflow.SubjectRequirement, len(values))
	for index, value := range values {
		out[index] = value
		out[index].Aliases = append([]string(nil), value.Aliases...)
		out[index].RequiredFacets = append([]string(nil), value.RequiredFacets...)
		out[index].RequiredSources = append([]agentapi.EvidenceSource(nil), value.RequiredSources...)
	}
	return out
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (p *Platform) investigationBudgetsForPlan(
	version int64,
	budgets workflow.Budgets,
	plan agentapi.TaskGraphProposal,
) (workflow.Budgets, error) {
	if !planUsesCapability(plan, "knowledge.runtime.observe") {
		return budgets, nil
	}
	observe, err := p.agents.capabilities.Resolve(agentapi.CapabilityRef{
		ID: "knowledge.runtime.observe", Version: version,
	})
	if err != nil {
		return workflow.Budgets{}, fmt.Errorf(
			"resolve live runtime investigation capability version %d: %w",
			version,
			err,
		)
	}
	if len(observe.ToolIDs) == 0 {
		return workflow.Budgets{}, fmt.Errorf(
			"live runtime investigation capability has no tools",
		)
	}
	definition, err := p.agents.catalog.Resolve(observe.Agent)
	if err != nil {
		return workflow.Budgets{}, fmt.Errorf(
			"resolve live runtime investigator: %w",
			err,
		)
	}
	if definition.Budget.MaxToolCalls <= 0 {
		return workflow.Budgets{}, fmt.Errorf(
			"live runtime investigator %q requires max_tool_calls",
			definition.ID,
		)
	}
	budgets.Observe, err = agentNodeBudget(
		definition,
		definition.Budget.MaxToolCalls,
	)
	if err != nil {
		return workflow.Budgets{}, err
	}
	observePayloadTokens, err := agentPayloadBudget(definition)
	if err != nil {
		return workflow.Budgets{}, err
	}
	if observePayloadTokens < budgets.InvestigatorPayloadTokens {
		budgets.InvestigatorPayloadTokens = observePayloadTokens
	}
	return budgets, nil
}

func planUsesCapability(
	plan agentapi.TaskGraphProposal,
	capabilityID string,
) bool {
	for _, task := range plan.Tasks {
		if task.Capability == capabilityID {
			return true
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
	terminal, verifiedPayload, err := runner.awaitWorkflowTerminal(
		ctx, service, workflowRunID,
	)
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	return investigationTerminal(terminal, verifiedPayload)
}

func (runner qaInvestigator) LoadTerminal(
	ctx context.Context,
	workflowRunID string,
) (agent.InvestigationTerminal, error) {
	service, err := runner.workflowService()
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	terminal, verifiedPayload, err := runner.loadWorkflowTerminal(
		ctx, service, workflowRunID,
	)
	if err != nil {
		return agent.InvestigationTerminal{}, err
	}
	return investigationTerminal(terminal, verifiedPayload)
}

func (runner qaInvestigator) LoadRound(
	ctx context.Context,
	workflowRunID string,
) (agent.InvestigationRoundSnapshot, error) {
	service, err := runner.workflowService()
	if err != nil {
		return agent.InvestigationRoundSnapshot{}, err
	}
	state, err := service.LoadFullRunState(ctx, workflowRunID)
	if err != nil {
		return agent.InvestigationRoundSnapshot{}, err
	}
	if state.Run.Status == workflow.RunRunning ||
		state.Run.Status == workflow.RunWaitingHuman {
		return agent.InvestigationRoundSnapshot{}, fmt.Errorf(
			"workflow run %q is %q: %w",
			workflowRunID,
			state.Run.Status,
			workflow.ErrConflict,
		)
	}
	terminal, verifiedPayload := terminalFromWorkflowState(state)
	investigationTerminal, err := investigationTerminal(terminal, verifiedPayload)
	if err != nil {
		return agent.InvestigationRoundSnapshot{}, err
	}
	contract := agent.TaskContract{}
	if len(state.Input.Payload) == 0 {
		return agent.InvestigationRoundSnapshot{}, fmt.Errorf(
			"workflow run %q input contract is empty",
			workflowRunID,
		)
	}
	if err := json.Unmarshal(state.Input.Payload, &contract); err != nil {
		return agent.InvestigationRoundSnapshot{}, fmt.Errorf(
			"decode QA investigation workflow %q contract: %w",
			workflowRunID,
			err,
		)
	}
	return agent.InvestigationRoundSnapshot{
		Terminal:     investigationTerminal,
		Contract:     contract,
		SeedEvidence: evidence.CloneUnits(state.Input.EvidenceUnits),
		Actor: agentapi.Actor{
			UserID: state.Run.ActorUserID, TenantID: state.Run.ActorTenantID,
		},
	}, nil
}

func (runner qaInvestigator) StartNextRound(
	ctx context.Context,
	request agent.InvestigationContinuationRequest,
) error {
	return runner.Start(ctx, agent.InvestigationRequest{
		WorkflowRunID: request.WorkflowRunID,
		ParentRunID:   request.ParentRunID,
		Round:         request.Round,
		BaseDepth:     request.BaseDepth,
		Contract:      request.Contract,
		Proposal:      request.Proposal,
		SeedEvidence:  request.SeedEvidence,
		Actor:         request.Actor,
	})
}

func (runner qaInvestigator) awaitWorkflowTerminal(
	ctx context.Context,
	service *workflow.Service,
	workflowRunID string,
) (workflow.TerminalResult, json.RawMessage, error) {
	terminal, err := service.AwaitTerminal(ctx, workflowRunID)
	if err == nil {
		if shouldRecoverWorkflowEvidence(terminal) {
			return runner.recoverWorkflowTerminal(ctx, service, workflowRunID, nil)
		}
		return terminal, nil, nil
	}
	if !errors.Is(err, workflow.ErrInvariant) {
		return workflow.TerminalResult{}, nil, err
	}
	return runner.recoverWorkflowTerminal(ctx, service, workflowRunID, err)
}

func (runner qaInvestigator) loadWorkflowTerminal(
	ctx context.Context,
	service *workflow.Service,
	workflowRunID string,
) (workflow.TerminalResult, json.RawMessage, error) {
	terminal, err := service.LoadTerminalResult(ctx, workflowRunID)
	if err == nil {
		if shouldRecoverWorkflowEvidence(terminal) {
			return runner.recoverWorkflowTerminal(ctx, service, workflowRunID, nil)
		}
		return terminal, nil, nil
	}
	if !errors.Is(err, workflow.ErrInvariant) {
		return workflow.TerminalResult{}, nil, err
	}
	return runner.recoverWorkflowTerminal(ctx, service, workflowRunID, err)
}

func (runner qaInvestigator) recoverWorkflowTerminal(
	ctx context.Context,
	service *workflow.Service,
	workflowRunID string,
	terminalErr error,
) (workflow.TerminalResult, json.RawMessage, error) {
	state, err := service.LoadFullRunState(ctx, workflowRunID)
	if err != nil {
		return workflow.TerminalResult{}, nil, errors.Join(terminalErr, err)
	}
	terminal, verifiedPayload := terminalFromWorkflowState(state)
	return terminal, verifiedPayload, nil
}

func shouldRecoverWorkflowEvidence(terminal workflow.TerminalResult) bool {
	if terminal.Run.Status != workflow.RunSucceeded ||
		terminal.Output == nil || len(terminal.Output.Payload) == 0 {
		return true
	}
	var result agent.InvestigationResult
	if err := json.Unmarshal(terminal.Output.Payload, &result); err != nil {
		return true
	}
	return !investigationResultHasStructuredEvidence(result)
}

func investigationResultHasStructuredEvidence(result agent.InvestigationResult) bool {
	return len(result.Citations) > 0 || len(result.EvidenceUnits) > 0 ||
		len(result.SupportedClaims) > 0 || len(result.PartialClaims) > 0
}

func terminalFromWorkflowState(
	state *workflow.RunState,
) (workflow.TerminalResult, json.RawMessage) {
	if state == nil {
		return workflow.TerminalResult{}, nil
	}
	terminal := workflow.TerminalResult{Run: state.Run}
	if output, ok := state.NodeOutputs["workflow.output"]; ok {
		outputCopy := output
		terminal.Output = &outputCopy
	}
	verified, ok := state.NodeOutputs["evidence.verify"]
	if !ok || len(verified.Payload) == 0 {
		return terminal, nil
	}
	return terminal, append(json.RawMessage(nil), verified.Payload...)
}

func investigationTerminal(
	terminal workflow.TerminalResult,
	verifiedPayload json.RawMessage,
) (agent.InvestigationTerminal, error) {
	result := agent.InvestigationTerminal{
		WorkflowRunID: terminal.Run.ID,
		ErrorCode:     terminal.Run.ErrorCode,
		Round:         terminal.Run.Round,
		BaseDepth:     terminal.Run.BaseDepth,
		StopReason:    string(terminal.Run.StopReason),
		Usage: agent.InvestigationUsage{
			InputTokens:     terminal.Run.Usage.InputTokens,
			OutputTokens:    terminal.Run.Usage.OutputTokens,
			ReasoningTokens: terminal.Run.Usage.ReasoningTokens,
			TotalTokens:     terminal.Run.Usage.TotalTokens,
			ToolCalls:       terminal.Run.Usage.ToolCalls,
			CostMicros:      terminal.Run.Usage.CostMicros,
		},
	}
	var output *agent.InvestigationResult
	if terminal.Output != nil && len(terminal.Output.Payload) > 0 {
		decoded := new(agent.InvestigationResult)
		if err := json.Unmarshal(terminal.Output.Payload, decoded); err != nil {
			return agent.InvestigationTerminal{}, fmt.Errorf(
				"decode QA investigation workflow %q output: %w",
				terminal.Run.ID,
				err,
			)
		}
		output = decoded
	}
	if len(verifiedPayload) > 0 {
		verified := new(agent.InvestigationResult)
		if err := json.Unmarshal(verifiedPayload, verified); err != nil {
			return agent.InvestigationTerminal{}, fmt.Errorf(
				"decode QA investigation workflow %q verified evidence: %w",
				terminal.Run.ID,
				err,
			)
		}
		output = mergeInvestigationResults(output, verified)
	}
	if output != nil {
		output.Round = result.Round
		output.BaseDepth = result.BaseDepth
		output.StopReason = result.StopReason
		result.Output = output
	}
	switch terminal.Run.Status {
	case workflow.RunSucceeded:
		result.Status = agent.InvestigationSucceeded
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
	if terminal.Output != nil {
		result.Completeness = investigationCompleteness(terminal.Output.Completeness)
	} else if output != nil {
		result.Completeness = investigationCompletenessFromResult(*output)
	}
	if result.Output != nil && result.Completeness == "" {
		result.Completeness = agent.InvestigationPartial
	}
	return result, nil
}

func investigationCompleteness(value workflow.Completeness) agent.InvestigationCompleteness {
	switch value {
	case workflow.Complete:
		return agent.InvestigationComplete
	case workflow.Partial:
		return agent.InvestigationPartial
	case workflow.Unavailable:
		return agent.InvestigationUnavailable
	default:
		return ""
	}
}

func investigationCompletenessFromResult(
	result agent.InvestigationResult,
) agent.InvestigationCompleteness {
	switch strings.TrimSpace(result.WorkflowCompleteness) {
	case string(agent.InvestigationComplete):
		return agent.InvestigationComplete
	case string(agent.InvestigationUnavailable):
		return agent.InvestigationUnavailable
	default:
		return agent.InvestigationPartial
	}
}

func mergeInvestigationResults(
	answer *agent.InvestigationResult,
	verified *agent.InvestigationResult,
) *agent.InvestigationResult {
	if answer == nil {
		clone := *verified
		return &clone
	}
	merged := agent.MergeInvestigationResults(*answer, *verified)
	return &merged
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
	case "workflow_succeeded", "workflow_failed",
		"workflow_cancelled", "workflow_timed_out":
		status, ok := investigationTerminalStatus(event.Kind)
		if !ok {
			break
		}
		projected.Status = string(status)
		projected.Reason = event.Summary
		var detail workflow.TerminalEventDetail
		if len(event.Detail) > 0 && json.Unmarshal(event.Detail, &detail) == nil &&
			(detail.RunStatus == "" || detail.RunStatus == status) {
			projected.ErrorCode = detail.ErrorCode
			if detail.StopReason != "" {
				projected.Reason = string(detail.StopReason)
			}
			if validInvestigationCompleteness(detail.Completeness) {
				projected.Completeness = string(detail.Completeness)
			}
		}
		return run.EventWorkflowCompleted, projected, true
	}
	return "", run.ExecutionEvent{}, false
}

func investigationTerminalStatus(kind string) (workflow.RunStatus, bool) {
	switch kind {
	case "workflow_succeeded":
		return workflow.RunSucceeded, true
	case "workflow_failed":
		return workflow.RunFailed, true
	case "workflow_cancelled":
		return workflow.RunCancelled, true
	case "workflow_timed_out":
		return workflow.RunTimedOut, true
	default:
		return "", false
	}
}

func validInvestigationCompleteness(value workflow.Completeness) bool {
	switch value {
	case workflow.Complete, workflow.Partial, workflow.Unavailable:
		return true
	default:
		return false
	}
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
