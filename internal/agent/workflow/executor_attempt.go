package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/scope"
)

func (orchestrator *Orchestrator) executeNodeAttempt(
	nodeCtx context.Context,
	definition Definition,
	request RunRequest,
	nodeRequest NodeRequest,
	account *budgetAccount,
	observer RunObserver,
) nodeOutcome {
	nodeCtx = runtrace.WithCorrelation(nodeCtx, runtrace.Correlation{
		RunID: nodeRequest.WorkflowRunID, WorkflowRunID: nodeRequest.WorkflowRunID,
		WorkflowNodeID: nodeRequest.Node.ID,
	})
	outcome, err := runtrace.Invoke(nodeCtx, nodeTraceSpec, nodeRequest, func(nodeCtx context.Context, _ NodeRequest) (nodeOutcome, error) {
		outcome := orchestrator.executeAttempt(nodeCtx, definition, request, nodeRequest, account, observer)
		return outcome, outcome.err
	})
	outcome.err = err
	return outcome
}

func (orchestrator *Orchestrator) executeAttempt(
	nodeCtx context.Context,
	definition Definition,
	request RunRequest,
	nodeRequest NodeRequest,
	account *budgetAccount,
	observer RunObserver,
) nodeOutcome {
	node := nodeRequest.Node
	inputs := nodeRequest.Inputs
	// Every attempt gets its own adapter so the coordinator can tell whether
	// the child Runtime actually settled physical model usage. All attempts
	// still delegate admission and settlement to the shared Run ledger.
	phase := runBudgetPhaseForNode(node)
	attemptGate := newAttemptBudgetGate(account, phase)
	nodeCtx = agentapi.WithRunBudgetGate(nodeCtx, attemptGate)
	nodeCtx = agentapi.WithRunBudgetPhase(nodeCtx, phase)
	// Custom NodeExecutors used by deterministic integrations and tests may not
	// call the Runtime model-call gate themselves. Admit Agent nodes here as a
	// coordinator-level preflight so a restored run cannot start a child after
	// its shared Workflow budget is already exhausted. The Runtime still
	// performs the authoritative ReserveCall/Settle for every physical call.
	if node.Kind == NodeAgent {
		if err := account.checkForPhase(phase); err != nil {
			return nodeOutcome{err: errors.Join(ErrNoAffordableTask, err)}
		}
		if !account.CanStartAgentForPhase(phase) {
			return nodeOutcome{err: errors.Join(
				ErrNoAffordableTask,
				budgetExceededError("workflow Run has no capacity for another Agent model call in phase %q", phase),
			)}
		}
	}
	if observer != nil {
		if err := observer.NodeStarted(nodeCtx, nodeRequest); err != nil {
			return nodeOutcome{err: fmt.Errorf("persist node start: %w", err)}
		}
	}
	if err := orchestrator.validateNodeInputs(node, inputs, definition.Budget.MaxHandoffBytes); err != nil {
		return orchestrator.failNode(nodeCtx, nodeRequest, NodeResult{}, err, observer)
	}
	handoff, result, decision, executeErr := orchestrator.executeNodeByKind(
		nodeCtx, request, nodeRequest, definition,
	)
	if node.Kind == NodeAgent {
		result.accountedUsage = attemptGate.AccountedUsage()
	}
	if executeErr == nil {
		if err := nodeCtx.Err(); err != nil {
			executeErr = err
		}
	}
	prepared, result, executeErr := orchestrator.finalizeAttempt(
		nodeCtx, request, nodeRequest, definition, account,
		handoff, result, decision, executeErr,
	)
	if executeErr != nil {
		return orchestrator.failNode(nodeCtx, nodeRequest, result, executeErr, observer)
	}
	var err error
	if observer != nil {
		err = observer.NodeSucceeded(nodeCtx, nodeRequest, result, decision)
		if err != nil {
			err = fmt.Errorf("persist node success: %w", err)
		}
	}
	return nodeOutcome{handoff: prepared, nodeResult: result, gate: decision, err: err}
}

// executeNodeByKind dispatches a single attempt to the executor matching the
// node kind and returns the raw handoff, result, gate decision and any error.
// Accounting and final handoff preparation happen in the caller so each
// attempt still settles its usage exactly once.
func (orchestrator *Orchestrator) executeNodeByKind(
	nodeCtx context.Context,
	request RunRequest,
	nodeRequest NodeRequest,
	definition Definition,
) (handoff Handoff, result NodeResult, decision *GateDecision, executeErr error) {
	node := nodeRequest.Node
	inputs := nodeRequest.Inputs
	switch node.Kind {
	case NodeJoin:
		handoff, executeErr = orchestrator.aggregateHandoffs(
			nodeCtx,
			request.RunID,
			node,
			inputs,
			unavailableTaskViews(
				nodeRequest.UnavailablePredecessors,
				nodeRequest.UnavailableReasons,
			),
			nodeRequest.BaselineEvidence,
			definition.Budget.MaxDuplicateRatio,
			definition.Budget.MaxHandoffBytes,
		)
	case NodeVerifier:
		handoff, executeErr = orchestrator.verifyEvidence(
			nodeCtx,
			request.RunID,
			node,
			inputs,
			definition.Budget.MaxHandoffBytes,
		)
	case NodeGate:
		evaluator := orchestrator.gates[node.Gate.ID]
		if evaluator == nil {
			executeErr = fmt.Errorf("gate evaluator %q is not registered", node.Gate.ID)
			return handoff, result, decision, executeErr
		}
		gateDecision, err := evaluator.Evaluate(nodeCtx, nodeRequest)
		if err != nil {
			return handoff, result, decision, err
		}
		if !contains(node.Gate.AllowedDecisions, gateDecision.Decision) {
			return handoff, result, decision, fmt.Errorf("gate %q returned unsupported decision %q", node.Gate.ID, gateDecision.Decision)
		}
		gateDecision.GateID = node.Gate.ID
		if gateDecision.EvaluatedAt.IsZero() {
			gateDecision.EvaluatedAt = time.Now().UTC()
		}
		if node.Gate.ForwardInput {
			source := inputs[0]
			handoff = Handoff{
				Schema:            node.OutputSchema,
				Payload:           append(json.RawMessage(nil), source.Payload...),
				References:        append([]agentapi.Reference(nil), source.References...),
				EvidenceUnits:     evidence.CloneUnits(source.EvidenceUnits),
				EvidenceConflicts: cloneConflicts(source.EvidenceConflicts),
				Completeness:      source.Completeness,
			}
		} else {
			payload, err := json.Marshal(gateDecision)
			if err != nil {
				return handoff, result, decision, fmt.Errorf("marshal gate decision: %w", err)
			}
			handoff = Handoff{
				Schema: node.OutputSchema, Payload: payload, Completeness: Complete,
			}
		}
		decision = &gateDecision
	case NodeHumanApproval:
		executeErr = ErrHumanApprovalRequired
	case NodeAgent, NodeTransform:
		if orchestrator.nodes == nil {
			executeErr = fmt.Errorf("node executor is not configured")
			return handoff, result, decision, executeErr
		}
		result, executeErr = orchestrator.nodes.Execute(nodeCtx, nodeRequest)
		if executeErr == nil {
			handoff = result.Handoff
		}
	default:
		executeErr = fmt.Errorf("node kind %q is unsupported", node.Kind)
	}
	return handoff, result, decision, executeErr
}

// finalizeAttempt records retry bookkeeping, prepares the handoff and settles
// the attempt's remaining usage against the shared Run ledger. It returns the
// prepared handoff and a possibly-joined error; on error the caller is
// responsible for recording the node failure.
func (orchestrator *Orchestrator) finalizeAttempt(
	nodeCtx context.Context,
	request RunRequest,
	nodeRequest NodeRequest,
	definition Definition,
	account *budgetAccount,
	handoff Handoff,
	result NodeResult,
	decision *GateDecision,
	executeErr error,
) (Handoff, NodeResult, error) {
	node := nodeRequest.Node
	if nodeRequest.retryAccounted {
		// Make the retry visible in the node transition so durable stores retain
		// the same aggregate usage as the in-memory Run ledger.
		result.Usage.Retries++
	}
	var prepared Handoff
	if executeErr == nil {
		handoff.WorkflowRunID = request.RunID
		handoff.ProducerNodeID = node.ID
		handoff.Schema = node.OutputSchema
		var err error
		prepared, err = PrepareHandoff(
			handoff,
			definition.Budget.MaxHandoffBytes,
			orchestrator.schemas,
		)
		if err != nil {
			executeErr = err
		}
	}
	accountingUsage := subtractAccountedUsage(result.Usage, result.accountedUsage)
	if nodeRequest.retryAccounted && accountingUsage.Retries > 0 {
		// ConsumeRetry already charged this retry in the shared ledger. Keep it
		// in the persisted NodeResult, but do not charge it a second time here.
		accountingUsage.Retries--
	}
	if usageErr := account.RecordUsage(accountingUsage); usageErr != nil {
		executeErr = errors.Join(executeErr, usageErr)
	}
	if executeErr != nil {
		return prepared, result, executeErr
	}
	result.Handoff = prepared
	return prepared, result, nil
}

func runBudgetPhaseForNode(node NodeDefinition) agentapi.RunBudgetPhase {
	if node.Kind == NodeVerifier {
		return agentapi.RunBudgetPhaseVerifier
	}
	if node.Kind != NodeAgent {
		return agentapi.RunBudgetPhaseDefault
	}
	switch node.Agent.ID {
	case "delegation.verifier", "evidence.verify", "verifier":
		return agentapi.RunBudgetPhaseVerifier
	case "synthesizer", "composer":
		return agentapi.RunBudgetPhaseAnswer
	default:
		return agentapi.RunBudgetPhaseDefault
	}
}

func subtractAccountedUsage(total, accounted Usage) Usage {
	return Usage{
		InputTokens:     maxInt64(0, total.InputTokens-accounted.InputTokens),
		OutputTokens:    maxInt64(0, total.OutputTokens-accounted.OutputTokens),
		ReasoningTokens: maxInt64(0, total.ReasoningTokens-accounted.ReasoningTokens),
		TotalTokens:     maxInt64(0, total.TotalTokens-accounted.TotalTokens),
		ToolCalls:       total.ToolCalls,
		CostMicros:      maxInt64(0, total.CostMicros-accounted.CostMicros),
		Retries:         total.Retries,
	}
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
		(scope.HasSideEffect(request.EffectivePermissions.Scopes) &&
			!(request.Node.Kind == NodeAgent && request.Node.RetrySafe)) ||
		errors.Is(runErr, ErrNoAffordableTask) ||
		errors.Is(runErr, ErrBudgetExhausted) ||
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
