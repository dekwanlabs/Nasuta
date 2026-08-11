package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/scope"
)

func (orchestrator *Orchestrator) executeNodeAttempt(
	nodeCtx context.Context,
	definition WorkflowDefinition,
	request RunRequest,
	nodeRequest NodeRequest,
	account *workflowBudgetAccount,
	observer RunObserver,
) nodeOutcome {
	nodeCtx = runtrace.WithCorrelation(nodeCtx, runtrace.Correlation{
		RunID: nodeRequest.WorkflowRunID, WorkflowRunID: nodeRequest.WorkflowRunID,
		WorkflowNodeID: nodeRequest.Node.ID,
	})
	outcome, err := runtrace.Invoke(nodeCtx, workflowNodeTraceSpec, nodeRequest, func(nodeCtx context.Context, _ NodeRequest) (nodeOutcome, error) {
		outcome := orchestrator.executeNodeAttemptUntraced(nodeCtx, definition, request, nodeRequest, account, observer)
		return outcome, outcome.err
	})
	outcome.err = err
	return outcome
}

func (orchestrator *Orchestrator) executeNodeAttemptUntraced(
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
		handoff, executeErr = orchestrator.aggregateHandoffs(
			nodeCtx, request.RunID, node, inputs, definition.Budget.MaxHandoffBytes,
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
		scope.HasSideEffect(request.EffectivePermissions.Scopes) ||
		errors.Is(runErr, ErrWorkflowBudgetExhausted) ||
		errors.Is(runErr, context.Canceled) ||
		errors.Is(runErr, context.DeadlineExceeded) {
		return false
	}
	var classified interface{ Retryable() bool }
	return errors.As(runErr, &classified) && classified.Retryable()
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
