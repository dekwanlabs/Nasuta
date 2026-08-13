package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
)

type capabilityLimitKey struct {
	id      string
	version int64
}

type capabilityLimiter struct {
	max    int
	tokens chan struct{}
}

type workflowConvergedTraceInput struct {
	definition WorkflowDefinition
}

type workflowConvergedTraceOutput struct {
	result               Result
	completedNodeCount   int
	unavailableNodeCount int
	waitingHumanCount    int
	usage                WorkflowUsage
}

var workflowConvergedTraceSpec = runtrace.Spec[
	workflowConvergedTraceInput,
	workflowConvergedTraceOutput,
]{
	Operation: "workflow.converged",
	Node:      "workflow.converged",
	Input: func(input workflowConvergedTraceInput) map[string]any {
		return map[string]any{
			"workflow_id":      input.definition.ID,
			"workflow_version": input.definition.Version,
			"workflow_hash":    input.definition.ContentHash,
			"node_count":       len(input.definition.Nodes),
		}
	},
	Output: func(
		_ workflowConvergedTraceInput,
		output workflowConvergedTraceOutput,
		err error,
	) map[string]any {
		outcome, errorCode := workflowConvergenceOutcome(output, err)
		fields := map[string]any{
			"outcome":                outcome,
			"completed_node_count":   output.completedNodeCount,
			"unavailable_node_count": output.unavailableNodeCount,
			"waiting_human_count":    output.waitingHumanCount,
			"usage":                  output.usage,
		}
		if output.result.Output.Completeness != "" {
			fields["completeness"] = output.result.Output.Completeness
		}
		if errorCode != "" {
			fields["error_code"] = errorCode
		}
		return fields
	},
	Status: func(output workflowConvergedTraceOutput, err error) string {
		outcome, _ := workflowConvergenceOutcome(output, err)
		switch outcome {
		case string(Complete):
			return "completed"
		case string(Partial), string(Unavailable):
			return "degraded"
		case "waiting_human":
			return "waiting_human"
		case "cancelled", "timed_out":
			return "cancelled"
		default:
			return "failed"
		}
	},
}

func (orchestrator *Orchestrator) runPrepared(
	ctx context.Context,
	definition WorkflowDefinition,
	metadata graphMetadata,
	request RunRequest,
	progress WorkflowProgress,
	observer RunObserver,
) (Result, error) {
	output, err := runtrace.Invoke(
		ctx,
		workflowConvergedTraceSpec,
		workflowConvergedTraceInput{definition: definition},
		func(
			ctx context.Context,
			_ workflowConvergedTraceInput,
		) (workflowConvergedTraceOutput, error) {
			return orchestrator.executePrepared(
				ctx,
				definition,
				metadata,
				request,
				progress,
				observer,
			)
		},
	)
	return output.result, err
}

func (orchestrator *Orchestrator) executePrepared(
	ctx context.Context,
	definition WorkflowDefinition,
	metadata graphMetadata,
	request RunRequest,
	progress WorkflowProgress,
	observer RunObserver,
) (traceOutput workflowConvergedTraceOutput, runErr error) {
	runCtx, cancel := context.WithDeadline(ctx, progress.StartedAt.Add(definition.Budget.Timeout))
	defer cancel()
	outputs := cloneHandoffMap(progress.NodeOutputs)
	gates := cloneGateMap(progress.Gates)
	failedOptional := cloneStringSet(progress.FailedOptional)
	waitingHuman := cloneStringSet(progress.WaitingHuman)
	account, err := newWorkflowBudgetAccount(definition.Budget, progress.Usage)
	defer func() {
		traceOutput.completedNodeCount = len(outputs)
		traceOutput.unavailableNodeCount = len(failedOptional)
		traceOutput.waitingHumanCount = len(waitingHuman)
		if account != nil {
			traceOutput.usage = account.Usage()
		}
	}()
	if err != nil {
		return traceOutput, err
	}

	for len(outputs)+len(failedOptional) < len(definition.Nodes) {
		if err := runCtx.Err(); err != nil {
			return traceOutput, err
		}
		ready := readyNodes(metadata, outputs, failedOptional, waitingHuman)
		if len(ready) == 0 {
			if len(waitingHuman) > 0 {
				return traceOutput, ErrHumanApprovalRequired
			}
			return traceOutput, fmt.Errorf("workflow %q cannot make progress", definition.ID)
		}
		wave, err := orchestrator.dispatchWave(
			runCtx, definition, metadata, request, progress, outputs, failedOptional,
			ready, account, observer,
		)
		if err != nil {
			return traceOutput, err
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
			return traceOutput, waveErr
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
		return traceOutput, fmt.Errorf("workflow %q produced no terminal output", definition.ID)
	}
	output := terminals[0]
	if len(terminals) > 1 {
		output, err = orchestrator.aggregateHandoffs(
			ctx,
			request.RunID,
			NodeDefinition{ID: "workflow.output", OutputSchema: definition.OutputSchema},
			terminals,
			nil,
			definition.Budget.MaxHandoffBytes,
		)
		if err != nil {
			return traceOutput, err
		}
	} else {
		if err := orchestrator.schemas.ValidateCompatibility(output.Schema, definition.OutputSchema); err != nil {
			return traceOutput, fmt.Errorf("workflow %q terminal output schema: %w", definition.ID, err)
		}
		output, err = PrepareHandoff(Handoff{
			WorkflowRunID: request.RunID, ProducerNodeID: "workflow.output",
			Schema: definition.OutputSchema, Payload: output.Payload,
			References: output.References, EvidenceUnits: output.EvidenceUnits,
			EvidenceConflicts: output.EvidenceConflicts,
			Completeness:      output.Completeness,
		}, definition.Budget.MaxHandoffBytes, orchestrator.schemas)
		if err != nil {
			return traceOutput, fmt.Errorf("workflow %q output: %w", definition.ID, err)
		}
	}
	traceEvidenceDelivered(ctx, "workflow_output", output)
	traceOutput.result = Result{
		RunID: request.RunID, Output: output, NodeOutputs: outputs, Gates: gates,
		Usage: account.Usage(),
	}
	return traceOutput, nil
}

func workflowConvergenceOutcome(
	output workflowConvergedTraceOutput,
	err error,
) (string, string) {
	if err == nil {
		switch output.result.Output.Completeness {
		case Partial:
			return string(Partial), ""
		case Unavailable:
			return string(Unavailable), ""
		default:
			return string(Complete), ""
		}
	}
	switch {
	case errors.Is(err, ErrHumanApprovalRequired):
		return "waiting_human", "human_approval_required"
	case errors.Is(err, ErrEvidenceConflict):
		return "evidence_conflict", "evidence_conflict"
	case errors.Is(err, ErrWorkflowBudgetExhausted):
		return "failed", "workflow_budget_exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed_out", "workflow_timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled", "workflow_cancelled"
	default:
		return "failed", "workflow_failed"
	}
}

func (orchestrator *Orchestrator) dispatchWave(
	ctx context.Context,
	definition WorkflowDefinition,
	metadata graphMetadata,
	request RunRequest,
	progress WorkflowProgress,
	outputs map[string]Handoff,
	failedOptional map[string]struct{},
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
			capabilityLimiters, err := orchestrator.limitersForWave(
				ready,
				metadata.nodes,
			)
			if err != nil {
				return dispatchOutput{}, err
			}
			var waitGroup sync.WaitGroup
			semaphore := make(chan struct{}, definition.Budget.MaxParallelism)
			for index, nodeID := range ready {
				index, node := index, metadata.nodes[nodeID]
				waitGroup.Add(1)
				go func() {
					defer waitGroup.Done()
					if limiter := capabilityLimiters[capabilityKey(node.Capability)]; limiter != nil {
						select {
						case limiter.tokens <- struct{}{}:
							defer func() { <-limiter.tokens }()
						case <-ctx.Done():
							wave[index].err = ctx.Err()
							return
						}
					}
					select {
					case semaphore <- struct{}{}:
						defer func() { <-semaphore }()
					case <-ctx.Done():
						wave[index].err = ctx.Err()
						return
					}
					inputs := predecessorHandoffs(node.ID, metadata.predecessors, outputs, progress.Input)
					unavailable := unavailablePredecessors(
						node.ID,
						metadata.predecessors,
						failedOptional,
					)
					wave[index] = orchestrator.executeNode(
						ctx,
						definition,
						request,
						node,
						inputs,
						unavailable,
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

func (orchestrator *Orchestrator) limitersForWave(
	ready []string,
	nodes map[string]NodeDefinition,
) (map[capabilityLimitKey]*capabilityLimiter, error) {
	limits := make(map[capabilityLimitKey]int)
	for _, nodeID := range ready {
		node := nodes[nodeID]
		if node.Capability.ID == "" {
			continue
		}
		key := capabilityKey(node.Capability)
		if published, exists := limits[key]; exists &&
			published != node.CapabilityMaxConcurrency {
			return nil, fmt.Errorf(
				"capability %q version %d concurrency limit changed from %d to %d",
				key.id,
				key.version,
				published,
				node.CapabilityMaxConcurrency,
			)
		}
		limits[key] = node.CapabilityMaxConcurrency
	}
	orchestrator.capabilityMu.Lock()
	defer orchestrator.capabilityMu.Unlock()
	if orchestrator.capabilityLimiters == nil {
		orchestrator.capabilityLimiters = make(
			map[capabilityLimitKey]*capabilityLimiter,
		)
	}
	resolved := make(map[capabilityLimitKey]*capabilityLimiter, len(limits))
	for key, maxConcurrency := range limits {
		limiter, exists := orchestrator.capabilityLimiters[key]
		if exists && limiter.max != maxConcurrency {
			return nil, fmt.Errorf(
				"capability %q version %d concurrency limit changed from %d to %d",
				key.id,
				key.version,
				limiter.max,
				maxConcurrency,
			)
		}
		if !exists {
			limiter = &capabilityLimiter{
				max:    maxConcurrency,
				tokens: make(chan struct{}, maxConcurrency),
			}
			orchestrator.capabilityLimiters[key] = limiter
		}
		resolved[key] = limiter
	}
	return resolved, nil
}

func capabilityKey(ref agentapi.CapabilityRef) capabilityLimitKey {
	return capabilityLimitKey{id: ref.ID, version: ref.Version}
}

func (orchestrator *Orchestrator) executeNode(
	ctx context.Context,
	definition WorkflowDefinition,
	request RunRequest,
	node NodeDefinition,
	inputs []Handoff,
	unavailablePredecessors []string,
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
		WorkflowRunID: request.RunID, Node: node, Inputs: inputs,
		UnavailablePredecessors: append([]string(nil), unavailablePredecessors...),
		Actor:                   request.Actor,
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
