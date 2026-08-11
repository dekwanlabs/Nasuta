package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

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
		wave, err := orchestrator.dispatchWave(
			runCtx, definition, metadata, request, progress, outputs, ready, account, observer,
		)
		if err != nil {
			return Result{}, err
		}
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
		output, err = orchestrator.aggregateHandoffs(
			ctx,
			request.RunID,
			NodeDefinition{ID: "workflow.output", OutputSchema: definition.OutputSchema},
			terminals,
			definition.Budget.MaxHandoffBytes,
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

func (orchestrator *Orchestrator) dispatchWave(
	ctx context.Context,
	definition WorkflowDefinition,
	metadata graphMetadata,
	request RunRequest,
	progress WorkflowProgress,
	outputs map[string]Handoff,
	ready []string,
	account *workflowBudgetAccount,
	observer RunObserver,
) ([]nodeOutcome, error) {
	result, err := runtrace.Invoke(
		ctx,
		multiAgentDispatchTraceSpec,
		dispatchInput{definition: definition, ready: ready},
		func(ctx context.Context, _ dispatchInput) (dispatchOutput, error) {
			wave := make([]nodeOutcome, len(ready))
			var waitGroup sync.WaitGroup
			semaphore := make(chan struct{}, definition.Budget.MaxParallelism)
			for index, nodeID := range ready {
				index, node := index, metadata.nodes[nodeID]
				waitGroup.Add(1)
				go func() {
					defer waitGroup.Done()
					select {
					case semaphore <- struct{}{}:
						defer func() { <-semaphore }()
					case <-ctx.Done():
						wave[index].err = ctx.Err()
						return
					}
					inputs := predecessorHandoffs(node.ID, metadata.predecessors, outputs, progress.Input)
					wave[index] = orchestrator.executeNode(
						ctx,
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
			waitGroup.Wait()
			return dispatchOutput{outcomes: wave}, nil
		},
	)
	return result.outcomes, err
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
