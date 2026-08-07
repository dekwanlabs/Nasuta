package agentworkflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
)

var (
	ErrHumanApprovalRequired   = errors.New("workflow requires human approval")
	ErrWorkflowBudgetExhausted = errors.New("workflow budget exhausted")
)

type RunRequest struct {
	RunID               string
	Input               json.RawMessage
	Actor               agentapi.Actor
	ActorPermissions    agentapi.PermissionPolicy
	ScenarioPermissions agentapi.PermissionPolicy
	StartedAt           time.Time
}

type NodeRequest struct {
	WorkflowRunID        string
	Node                 NodeDefinition
	Inputs               []Handoff
	Attempt              int
	Actor                agentapi.Actor
	EffectivePermissions agentapi.PermissionPolicy
}

type NodeExecutor interface {
	Execute(context.Context, NodeRequest) (NodeResult, error)
}

// NodeDispatcher keeps agent and business transform lifecycles explicit.
type NodeDispatcher struct {
	agent     NodeExecutor
	transform NodeExecutor
}

func NewNodeDispatcher(agent, transform NodeExecutor) *NodeDispatcher {
	if agent == nil && transform == nil {
		return nil
	}
	return &NodeDispatcher{agent: agent, transform: transform}
}

func (dispatcher *NodeDispatcher) Execute(
	ctx context.Context,
	request NodeRequest,
) (NodeResult, error) {
	if dispatcher == nil {
		return NodeResult{}, fmt.Errorf("workflow node dispatcher is unavailable")
	}
	switch request.Node.Kind {
	case NodeAgent:
		if dispatcher.agent == nil {
			return NodeResult{}, fmt.Errorf("agent node executor is unavailable")
		}
		return dispatcher.agent.Execute(ctx, request)
	case NodeTransform:
		if dispatcher.transform == nil {
			return NodeResult{}, fmt.Errorf(
				"transform %q executor is unavailable",
				request.Node.TransformID,
			)
		}
		return dispatcher.transform.Execute(ctx, request)
	default:
		return NodeResult{}, fmt.Errorf("node kind %q is unsupported by dispatcher", request.Node.Kind)
	}
}

type NodeResult struct {
	Handoff    Handoff
	AgentRunID string
	Usage      WorkflowUsage
}

type RunObserver interface {
	NodeStarted(context.Context, NodeRequest) error
	NodeSucceeded(context.Context, NodeRequest, NodeResult, *GateDecision) error
	NodeFailed(context.Context, NodeRequest, NodeResult, error) error
}

type GateEvaluator interface {
	Evaluate(context.Context, NodeRequest) (GateDecision, error)
}

type Result struct {
	RunID       string
	Output      Handoff
	NodeOutputs map[string]Handoff
	Gates       map[string]GateDecision
	Usage       WorkflowUsage
}

type WorkflowProgress struct {
	StartedAt      time.Time
	Input          Handoff
	NodeOutputs    map[string]Handoff
	Gates          map[string]GateDecision
	FailedOptional map[string]struct{}
	WaitingHuman   map[string]struct{}
	NodeAttempts   map[string]NodeAttemptProgress
	Usage          WorkflowUsage
}

// NodeAttemptProgress preserves retry timing across a durable resume.
type NodeAttemptProgress struct {
	NextAttempt    int
	FirstStartedAt time.Time
	NotBefore      time.Time
}

// Orchestrator executes stable DAG waves while each node retains an independent context.
type Orchestrator struct {
	schemas *agentapi.SchemaRegistry
	nodes   NodeExecutor
	gates   map[string]GateEvaluator
}

func NewOrchestrator(
	schemas *agentapi.SchemaRegistry,
	nodes NodeExecutor,
	gates map[string]GateEvaluator,
) *Orchestrator {
	cloned := make(map[string]GateEvaluator, len(gates))
	for id, gate := range gates {
		cloned[id] = gate
	}
	return &Orchestrator{schemas: schemas, nodes: nodes, gates: cloned}
}

func (orchestrator *Orchestrator) Run(ctx context.Context, definition WorkflowDefinition, request RunRequest) (Result, error) {
	return orchestrator.RunObserved(ctx, definition, request, nil)
}

func (orchestrator *Orchestrator) RunObserved(
	ctx context.Context,
	definition WorkflowDefinition,
	request RunRequest,
	observer RunObserver,
) (Result, error) {
	prepared, metadata, err := orchestrator.prepareRun(definition, request)
	if err != nil {
		return Result{}, err
	}
	input, err := PrepareHandoff(Handoff{
		WorkflowRunID: request.RunID, ProducerNodeID: "workflow.input",
		Schema: prepared.InputSchema, Payload: request.Input, Completeness: Complete,
	}, prepared.Budget.MaxHandoffBytes, orchestrator.schemas)
	if err != nil {
		return Result{}, fmt.Errorf("workflow %q input: %w", prepared.ID, err)
	}
	startedAt := request.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return orchestrator.runPrepared(ctx, prepared, metadata, request, WorkflowProgress{
		StartedAt: startedAt, Input: input,
		NodeOutputs: make(map[string]Handoff, len(prepared.Nodes)),
		Gates:       make(map[string]GateDecision),
	}, observer)
}

func (orchestrator *Orchestrator) ResumeObserved(
	ctx context.Context,
	definition WorkflowDefinition,
	request RunRequest,
	progress WorkflowProgress,
	observer RunObserver,
) (Result, error) {
	prepared, metadata, err := orchestrator.prepareRun(definition, request)
	if err != nil {
		return Result{}, err
	}
	input, err := PrepareHandoff(
		progress.Input,
		prepared.Budget.MaxHandoffBytes,
		orchestrator.schemas,
	)
	if err != nil {
		return Result{}, fmt.Errorf("workflow %q stored input: %w", prepared.ID, err)
	}
	progress.Input = input
	if progress.StartedAt.IsZero() {
		return Result{}, fmt.Errorf("workflow %q progress start time is required", prepared.ID)
	}
	return orchestrator.runPrepared(ctx, prepared, metadata, request, progress, observer)
}

func (orchestrator *Orchestrator) prepareRun(
	definition WorkflowDefinition,
	request RunRequest,
) (WorkflowDefinition, graphMetadata, error) {
	if orchestrator == nil {
		return WorkflowDefinition{}, graphMetadata{}, fmt.Errorf("workflow orchestrator is unavailable")
	}
	prepared, err := Prepare(definition, orchestrator.schemas)
	if err != nil {
		return WorkflowDefinition{}, graphMetadata{}, err
	}
	if request.RunID == "" {
		return WorkflowDefinition{}, graphMetadata{}, fmt.Errorf("workflow run id is required")
	}
	metadata, err := graph(prepared, orchestrator.schemas)
	if err != nil {
		return WorkflowDefinition{}, graphMetadata{}, err
	}
	return prepared, metadata, nil
}

func (orchestrator *Orchestrator) runPrepared(
	ctx context.Context,
	definition WorkflowDefinition,
	metadata graphMetadata,
	request RunRequest,
	progress WorkflowProgress,
	observer RunObserver,
) (Result, error) {
	runCtx, cancel := context.WithDeadline(ctx, progress.StartedAt.Add(definition.Budget.Timeout))
	defer cancel()
	outputs := cloneHandoffMap(progress.NodeOutputs)
	gates := cloneGateMap(progress.Gates)
	failedOptional := cloneStringSet(progress.FailedOptional)
	waitingHuman := cloneStringSet(progress.WaitingHuman)
	account, err := newWorkflowBudgetAccount(definition.Budget, progress.Usage)
	if err != nil {
		return Result{}, err
	}

	for len(outputs)+len(failedOptional) < len(definition.Nodes) {
		if err := runCtx.Err(); err != nil {
			return Result{}, err
		}
		ready := readyNodes(metadata, outputs, failedOptional, waitingHuman)
		if len(ready) == 0 {
			if len(waitingHuman) > 0 {
				return Result{}, ErrHumanApprovalRequired
			}
			return Result{}, fmt.Errorf("workflow %q cannot make progress", definition.ID)
		}
		wave := make([]nodeOutcome, len(ready))
		var wg sync.WaitGroup
		sem := make(chan struct{}, definition.Budget.MaxParallelism)
		for index, nodeID := range ready {
			index, node := index, metadata.nodes[nodeID]
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-runCtx.Done():
					wave[index].err = runCtx.Err()
					return
				}
				inputs := predecessorHandoffs(node.ID, metadata.predecessors, outputs, progress.Input)
				wave[index] = orchestrator.executeNode(
					runCtx,
					definition,
					request,
					node,
					inputs,
					progress.NodeAttempts[node.ID],
					account,
					observer,
				)
			}()
		}
		wg.Wait()
		var waveErr error
		for index, outcome := range wave {
			node := metadata.nodes[ready[index]]
			if outcome.err != nil {
				if errors.Is(outcome.err, ErrHumanApprovalRequired) {
					waitingHuman[node.ID] = struct{}{}
					continue
				}
				if node.Optional && definition.FailurePolicy.Mode == CollectAvailable {
					failedOptional[node.ID] = struct{}{}
					continue
				}
				if waveErr == nil {
					waveErr = fmt.Errorf("workflow %q node %q: %w", definition.ID, node.ID, outcome.err)
				}
				continue
			}
			outputs[node.ID] = outcome.handoff
			delete(waitingHuman, node.ID)
			if outcome.gate != nil {
				gates[node.ID] = *outcome.gate
			}
		}
		if waveErr != nil {
			return Result{}, waveErr
		}
	}
	terminals := make([]Handoff, 0)
	for _, nodeID := range metadata.order {
		if len(metadata.successors[nodeID]) == 0 {
			if output, ok := outputs[nodeID]; ok {
				terminals = append(terminals, output)
			}
		}
	}
	if len(terminals) == 0 {
		return Result{}, fmt.Errorf("workflow %q produced no terminal output", definition.ID)
	}
	output := terminals[0]
	if len(terminals) > 1 {
		output, err = joinHandoffs(
			request.RunID,
			"workflow.output",
			definition.OutputSchema,
			terminals,
			definition.Budget.MaxHandoffBytes,
			orchestrator.schemas,
		)
		if err != nil {
			return Result{}, err
		}
	} else {
		if err := orchestrator.schemas.ValidateCompatibility(output.Schema, definition.OutputSchema); err != nil {
			return Result{}, fmt.Errorf("workflow %q terminal output schema: %w", definition.ID, err)
		}
		output, err = PrepareHandoff(Handoff{
			WorkflowRunID: request.RunID, ProducerNodeID: "workflow.output",
			Schema: definition.OutputSchema, Payload: output.Payload,
			References: output.References, Completeness: output.Completeness,
		}, definition.Budget.MaxHandoffBytes, orchestrator.schemas)
		if err != nil {
			return Result{}, fmt.Errorf("workflow %q output: %w", definition.ID, err)
		}
	}
	return Result{
		RunID: request.RunID, Output: output, NodeOutputs: outputs, Gates: gates,
		Usage: account.Usage(),
	}, nil
}

type nodeOutcome struct {
	handoff    Handoff
	nodeResult NodeResult
	gate       *GateDecision
	err        error
	retryable  bool
}

func (orchestrator *Orchestrator) executeNode(
	ctx context.Context,
	definition WorkflowDefinition,
	request RunRequest,
	node NodeDefinition,
	inputs []Handoff,
	attemptProgress NodeAttemptProgress,
	account *workflowBudgetAccount,
	observer RunObserver,
) nodeOutcome {
	firstAttempt := 1
	firstStartedAt := time.Now().UTC()
	if attemptProgress.NextAttempt > 0 {
		firstAttempt = attemptProgress.NextAttempt
		firstStartedAt = attemptProgress.FirstStartedAt
	}
	if firstAttempt <= 0 || firstAttempt > node.Retry.MaxAttempts {
		return nodeOutcome{err: fmt.Errorf(
			"workflow node %q next attempt %d exceeds retry policy",
			node.ID, firstAttempt,
		)}
	}
	if firstStartedAt.IsZero() {
		return nodeOutcome{err: fmt.Errorf(
			"workflow node %q first attempt start time is required",
			node.ID,
		)}
	}
	nodeCtx, cancel := context.WithDeadline(ctx, firstStartedAt.Add(node.Timeout))
	defer cancel()
	if !waitUntil(nodeCtx, attemptProgress.NotBefore) {
		return nodeOutcome{err: nodeCtx.Err()}
	}
	baseRequest := NodeRequest{
		WorkflowRunID: request.RunID, Node: node, Inputs: inputs, Actor: request.Actor,
		EffectivePermissions: IntersectPermissions(
			request.ActorPermissions, request.ScenarioPermissions,
			definition.Permissions, node.Permissions,
		),
	}
	for attempt := firstAttempt; attempt <= node.Retry.MaxAttempts; attempt++ {
		nodeRequest := baseRequest
		nodeRequest.Attempt = attempt
		outcome := orchestrator.executeNodeAttempt(
			nodeCtx, definition, request, nodeRequest, account, observer,
		)
		if outcome.err == nil || !outcome.retryable || attempt == node.Retry.MaxAttempts {
			return outcome
		}
		if !waitForRetry(nodeCtx, node.Retry.Backoff) {
			return nodeOutcome{nodeResult: outcome.nodeResult, err: nodeCtx.Err()}
		}
	}
	return nodeOutcome{err: fmt.Errorf("workflow node %q retry policy has no attempts", node.ID)}
}

func (orchestrator *Orchestrator) executeNodeAttempt(
	nodeCtx context.Context,
	definition WorkflowDefinition,
	request RunRequest,
	nodeRequest NodeRequest,
	account *workflowBudgetAccount,
	observer RunObserver,
) nodeOutcome {
	node := nodeRequest.Node
	inputs := nodeRequest.Inputs
	reservation, err := account.Reserve(node, nodeRequest.Attempt)
	if err != nil {
		return nodeOutcome{err: err}
	}
	if observer != nil {
		if err := observer.NodeStarted(nodeCtx, nodeRequest); err != nil {
			account.Release(reservation)
			return nodeOutcome{err: fmt.Errorf("persist node start: %w", err)}
		}
	}
	if err := orchestrator.validateNodeInputs(node, inputs, definition.Budget.MaxHandoffBytes); err != nil {
		result := NodeResult{}
		budgetErr := account.Settle(reservation, &result.Usage, node.Budget)
		return orchestrator.failNode(nodeCtx, nodeRequest, result, errors.Join(err, budgetErr), observer)
	}
	var handoff Handoff
	var result NodeResult
	var decision *GateDecision
	var executeErr error
	switch node.Kind {
	case NodeJoin:
		handoff, executeErr = joinHandoffs(
			request.RunID,
			node.ID,
			node.OutputSchema,
			inputs,
			definition.Budget.MaxHandoffBytes,
			orchestrator.schemas,
		)
	case NodeGate:
		evaluator := orchestrator.gates[node.Gate.ID]
		if evaluator == nil {
			executeErr = fmt.Errorf("gate evaluator %q is not registered", node.Gate.ID)
			break
		}
		gateDecision, err := evaluator.Evaluate(nodeCtx, nodeRequest)
		if err != nil {
			executeErr = err
			break
		}
		if !contains(node.Gate.AllowedDecisions, gateDecision.Decision) {
			executeErr = fmt.Errorf("gate %q returned unsupported decision %q", node.Gate.ID, gateDecision.Decision)
			break
		}
		gateDecision.GateID = node.Gate.ID
		if gateDecision.EvaluatedAt.IsZero() {
			gateDecision.EvaluatedAt = time.Now().UTC()
		}
		payload, err := json.Marshal(gateDecision)
		if err != nil {
			executeErr = fmt.Errorf("marshal gate decision: %w", err)
			break
		}
		handoff, executeErr = PrepareHandoff(Handoff{
			WorkflowRunID: request.RunID, ProducerNodeID: node.ID,
			Schema: node.OutputSchema, Payload: payload, Completeness: Complete,
		}, definition.Budget.MaxHandoffBytes, orchestrator.schemas)
		decision = &gateDecision
	case NodeHumanApproval:
		executeErr = ErrHumanApprovalRequired
	case NodeAgent, NodeTransform:
		if orchestrator.nodes == nil {
			executeErr = fmt.Errorf("node executor is not configured")
			break
		}
		result, executeErr = orchestrator.nodes.Execute(nodeCtx, nodeRequest)
		if executeErr == nil {
			handoff = result.Handoff
		}
	default:
		executeErr = fmt.Errorf("node kind %q is unsupported", node.Kind)
	}
	if executeErr == nil {
		if err := nodeCtx.Err(); err != nil {
			executeErr = err
		}
	}
	var prepared Handoff
	if executeErr == nil {
		handoff.WorkflowRunID = request.RunID
		handoff.ProducerNodeID = node.ID
		handoff.Schema = node.OutputSchema
		prepared, err = PrepareHandoff(
			handoff,
			definition.Budget.MaxHandoffBytes,
			orchestrator.schemas,
		)
		if err != nil {
			executeErr = err
		}
	}
	if budgetErr := account.Settle(reservation, &result.Usage, node.Budget); budgetErr != nil {
		executeErr = errors.Join(executeErr, budgetErr)
	}
	if executeErr != nil {
		return orchestrator.failNode(nodeCtx, nodeRequest, result, executeErr, observer)
	}
	result.Handoff = prepared
	if observer != nil {
		err = observer.NodeSucceeded(nodeCtx, nodeRequest, result, decision)
		if err != nil {
			err = fmt.Errorf("persist node success: %w", err)
		}
	}
	return nodeOutcome{handoff: prepared, nodeResult: result, gate: decision, err: err}
}

func (orchestrator *Orchestrator) failNode(
	ctx context.Context,
	request NodeRequest,
	result NodeResult,
	runErr error,
	observer RunObserver,
) nodeOutcome {
	if observer != nil {
		if err := observer.NodeFailed(ctx, request, result, runErr); err != nil {
			return nodeOutcome{
				nodeResult: result,
				err:        fmt.Errorf("%v; persist node failure: %w", runErr, err),
			}
		}
	}
	return nodeOutcome{
		nodeResult: result,
		err:        runErr,
		retryable:  retryableNodeFailure(request, runErr),
	}
}

func retryableNodeFailure(request NodeRequest, runErr error) bool {
	if request.Attempt <= 0 ||
		(request.Node.Kind != NodeAgent &&
			!(request.Node.Kind == NodeTransform && request.Node.RetrySafe)) ||
		platformscope.HasSideEffect(request.EffectivePermissions.Scopes) ||
		errors.Is(runErr, ErrWorkflowBudgetExhausted) ||
		errors.Is(runErr, context.Canceled) ||
		errors.Is(runErr, context.DeadlineExceeded) {
		return false
	}
	var classified interface{ Retryable() bool }
	return errors.As(runErr, &classified) && classified.Retryable()
}

type workflowBudgetAccount struct {
	mu       sync.Mutex
	budget   WorkflowBudget
	usage    WorkflowUsage
	reserved WorkflowUsage
}

func newWorkflowBudgetAccount(
	budget WorkflowBudget,
	usage WorkflowUsage,
) (*workflowBudgetAccount, error) {
	if err := validateWorkflowUsage(usage); err != nil {
		return nil, fmt.Errorf("restore workflow usage: %w", err)
	}
	return &workflowBudgetAccount{budget: budget, usage: usage}, nil
}

func (account *workflowBudgetAccount) Reserve(
	node NodeDefinition,
	attempt int,
) (WorkflowUsage, error) {
	reservation := WorkflowUsage{
		InputTokens:  node.Budget.MaxInputTokens,
		OutputTokens: node.Budget.MaxOutputTokens,
		TotalTokens:  node.Budget.MaxTotalTokens,
		ToolCalls:    node.Budget.MaxToolCalls,
		CostMicros:   node.Budget.MaxCostMicros,
	}
	if attempt > 1 {
		reservation.Retries = 1
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if err := account.checkCapacity(reservation); err != nil {
		return WorkflowUsage{}, fmt.Errorf(
			"%w for node %q attempt %d: %v",
			ErrWorkflowBudgetExhausted,
			node.ID,
			attempt,
			err,
		)
	}
	account.reserved = addWorkflowUsage(account.reserved, reservation)
	return reservation, nil
}

func (account *workflowBudgetAccount) Release(reservation WorkflowUsage) {
	account.mu.Lock()
	account.reserved = subtractWorkflowUsage(account.reserved, reservation)
	account.mu.Unlock()
}

func (account *workflowBudgetAccount) Settle(
	reservation WorkflowUsage,
	actual *WorkflowUsage,
	nodeBudget NodeBudget,
) error {
	account.mu.Lock()
	defer account.mu.Unlock()
	account.reserved = subtractWorkflowUsage(account.reserved, reservation)
	actual.Retries = reservation.Retries
	account.usage = addWorkflowUsage(account.usage, *actual)
	if err := nodeUsageWithinBudget(*actual, nodeBudget); err != nil {
		return fmt.Errorf("%w: %v", ErrWorkflowBudgetExhausted, err)
	}
	if err := account.checkUsage(); err != nil {
		return fmt.Errorf("%w: %v", ErrWorkflowBudgetExhausted, err)
	}
	return nil
}

func (account *workflowBudgetAccount) Usage() WorkflowUsage {
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.usage
}

func (account *workflowBudgetAccount) checkCapacity(additional WorkflowUsage) error {
	checks := []struct {
		name     string
		limit    int64
		used     int64
		reserved int64
		add      int64
	}{
		{"input tokens", account.budget.MaxInputTokens, account.usage.InputTokens, account.reserved.InputTokens, additional.InputTokens},
		{"output tokens", account.budget.MaxOutputTokens, account.usage.OutputTokens, account.reserved.OutputTokens, additional.OutputTokens},
		{"total tokens", account.budget.MaxTotalTokens, account.usage.TotalTokens, account.reserved.TotalTokens, additional.TotalTokens},
		{"tool calls", account.budget.MaxToolCalls, account.usage.ToolCalls, account.reserved.ToolCalls, additional.ToolCalls},
		{"cost", account.budget.MaxCostMicros, account.usage.CostMicros, account.reserved.CostMicros, additional.CostMicros},
		{"retries", account.budget.MaxRetries, account.usage.Retries, account.reserved.Retries, additional.Retries},
	}
	for _, check := range checks {
		if exceedsBudget(check.limit, check.used, check.reserved, check.add) {
			return fmt.Errorf("%s limit %d is exhausted", check.name, check.limit)
		}
	}
	return nil
}

func (account *workflowBudgetAccount) checkUsage() error {
	return (&workflowBudgetAccount{
		budget: account.budget,
		usage:  account.usage,
	}).checkCapacity(WorkflowUsage{})
}

func nodeUsageWithinBudget(usage WorkflowUsage, budget NodeBudget) error {
	checks := []struct {
		name  string
		limit int64
		value int64
	}{
		{"input tokens", budget.MaxInputTokens, usage.InputTokens},
		{"output tokens", budget.MaxOutputTokens, usage.OutputTokens},
		{"total tokens", budget.MaxTotalTokens, usage.TotalTokens},
		{"tool calls", budget.MaxToolCalls, usage.ToolCalls},
		{"cost", budget.MaxCostMicros, usage.CostMicros},
	}
	for _, check := range checks {
		if check.limit > 0 && check.value > check.limit {
			return fmt.Errorf("%s usage %d exceeds node reservation %d", check.name, check.value, check.limit)
		}
	}
	return nil
}

func validateWorkflowUsage(usage WorkflowUsage) error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningTokens < 0 ||
		usage.TotalTokens < 0 || usage.ToolCalls < 0 || usage.CostMicros < 0 ||
		usage.Retries < 0 {
		return fmt.Errorf("usage cannot be negative")
	}
	return nil
}

func exceedsBudget(limit, used, reserved, additional int64) bool {
	if limit == 0 {
		return false
	}
	if used > limit || reserved > limit-used {
		return true
	}
	return additional > limit-used-reserved
}

func addWorkflowUsage(left, right WorkflowUsage) WorkflowUsage {
	return WorkflowUsage{
		InputTokens:     left.InputTokens + right.InputTokens,
		OutputTokens:    left.OutputTokens + right.OutputTokens,
		ReasoningTokens: left.ReasoningTokens + right.ReasoningTokens,
		TotalTokens:     left.TotalTokens + right.TotalTokens,
		ToolCalls:       left.ToolCalls + right.ToolCalls,
		CostMicros:      left.CostMicros + right.CostMicros,
		Retries:         left.Retries + right.Retries,
	}
}

func subtractWorkflowUsage(left, right WorkflowUsage) WorkflowUsage {
	return WorkflowUsage{
		InputTokens:     left.InputTokens - right.InputTokens,
		OutputTokens:    left.OutputTokens - right.OutputTokens,
		ReasoningTokens: left.ReasoningTokens - right.ReasoningTokens,
		TotalTokens:     left.TotalTokens - right.TotalTokens,
		ToolCalls:       left.ToolCalls - right.ToolCalls,
		CostMicros:      left.CostMicros - right.CostMicros,
		Retries:         left.Retries - right.Retries,
	}
}

func waitUntil(ctx context.Context, notBefore time.Time) bool {
	if notBefore.IsZero() {
		return ctx.Err() == nil
	}
	delay := time.Until(notBefore)
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func waitForRetry(ctx context.Context, backoff time.Duration) bool {
	if backoff <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (orchestrator *Orchestrator) validateNodeInputs(
	node NodeDefinition,
	inputs []Handoff,
	maxBytes int64,
) error {
	for _, input := range inputs {
		if _, err := PrepareHandoff(input, maxBytes, orchestrator.schemas); err != nil {
			return fmt.Errorf("input from %q: %w", input.ProducerNodeID, err)
		}
		if err := orchestrator.schemas.ValidateCompatibility(input.Schema, node.InputSchema); err != nil {
			return fmt.Errorf("input from %q schema: %w", input.ProducerNodeID, err)
		}
		if err := orchestrator.schemas.Validate(node.InputSchema, input.Payload); err != nil {
			return fmt.Errorf("input from %q payload: %w", input.ProducerNodeID, err)
		}
	}
	return nil
}

func readyNodes(
	metadata graphMetadata,
	outputs map[string]Handoff,
	failedOptional map[string]struct{},
	waitingHuman map[string]struct{},
) []string {
	ready := make([]string, 0)
	for _, nodeID := range metadata.order {
		if _, done := outputs[nodeID]; done {
			continue
		}
		if _, failed := failedOptional[nodeID]; failed {
			continue
		}
		if _, waiting := waitingHuman[nodeID]; waiting {
			continue
		}
		runnable := true
		for _, predecessor := range metadata.predecessors[nodeID] {
			if _, succeeded := outputs[predecessor]; succeeded {
				continue
			}
			if _, failed := failedOptional[predecessor]; failed {
				if metadata.required[predecessor+"\x00"+nodeID] {
					runnable = false
				}
				continue
			}
			runnable = false
			break
		}
		if runnable {
			ready = append(ready, nodeID)
		}
	}
	return ready
}

func (orchestrator *Orchestrator) approvalHandoff(
	definition WorkflowDefinition,
	runID string,
	node NodeDefinition,
	inputs []Handoff,
	decidedAt time.Time,
) (Handoff, error) {
	if node.Kind != NodeHumanApproval {
		return Handoff{}, fmt.Errorf("workflow node %q does not require human approval", node.ID)
	}
	if err := orchestrator.validateNodeInputs(node, inputs, definition.Budget.MaxHandoffBytes); err != nil {
		return Handoff{}, err
	}
	if len(inputs) == 0 {
		return Handoff{}, fmt.Errorf("workflow human approval node %q has no input", node.ID)
	}
	if len(inputs) > 1 {
		handoff, err := joinHandoffs(
			runID,
			node.ID,
			node.OutputSchema,
			inputs,
			definition.Budget.MaxHandoffBytes,
			orchestrator.schemas,
		)
		if err != nil {
			return Handoff{}, err
		}
		handoff.CreatedAt = decidedAt
		return handoff, nil
	}
	input := inputs[0]
	return PrepareHandoff(Handoff{
		WorkflowRunID: runID, ProducerNodeID: node.ID, Schema: node.OutputSchema,
		Payload: input.Payload, References: input.References,
		Completeness: input.Completeness, CreatedAt: decidedAt,
	}, definition.Budget.MaxHandoffBytes, orchestrator.schemas)
}

func predecessorHandoffs(nodeID string, predecessors map[string][]string, outputs map[string]Handoff, input Handoff) []Handoff {
	ids := predecessors[nodeID]
	if len(ids) == 0 {
		return []Handoff{input}
	}
	handoffs := make([]Handoff, 0, len(ids))
	for _, id := range ids {
		if handoff, ok := outputs[id]; ok {
			handoffs = append(handoffs, handoff)
		}
	}
	return handoffs
}

func joinHandoffs(
	runID,
	producer string,
	schema agentapi.SchemaRef,
	inputs []Handoff,
	maxBytes int64,
	schemas *agentapi.SchemaRegistry,
) (Handoff, error) {
	ordered := append([]Handoff(nil), inputs...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].ProducerNodeID < ordered[j].ProducerNodeID
	})
	payloads := make([]json.RawMessage, 0, len(ordered))
	references := make([]agentapi.Reference, 0)
	completeness := Complete
	for _, input := range ordered {
		payloads = append(payloads, append(json.RawMessage(nil), input.Payload...))
		references = append(references, input.References...)
		if input.Completeness == Unavailable {
			completeness = Unavailable
		} else if input.Completeness == Partial && completeness == Complete {
			completeness = Partial
		}
	}
	payload, err := json.Marshal(payloads)
	if err != nil {
		return Handoff{}, fmt.Errorf("marshal joined handoffs: %w", err)
	}
	return PrepareHandoff(Handoff{
		WorkflowRunID: runID, ProducerNodeID: producer, Schema: schema,
		Payload: payload, References: references, Completeness: completeness,
	}, maxBytes, schemas)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneHandoffMap(source map[string]Handoff) map[string]Handoff {
	cloned := make(map[string]Handoff, len(source))
	for nodeID, handoff := range source {
		handoff.Payload = append(json.RawMessage(nil), handoff.Payload...)
		handoff.References = append([]agentapi.Reference(nil), handoff.References...)
		cloned[nodeID] = handoff
	}
	return cloned
}

func cloneGateMap(source map[string]GateDecision) map[string]GateDecision {
	cloned := make(map[string]GateDecision, len(source))
	for nodeID, decision := range source {
		decision.ReasonCodes = append([]string(nil), decision.ReasonCodes...)
		decision.FindingIDs = append([]string(nil), decision.FindingIDs...)
		cloned[nodeID] = decision
	}
	return cloned
}

func cloneStringSet(source map[string]struct{}) map[string]struct{} {
	cloned := make(map[string]struct{}, len(source))
	for value := range source {
		cloned[value] = struct{}{}
	}
	return cloned
}
