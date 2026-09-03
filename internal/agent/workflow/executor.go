package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

var (
	ErrHumanApprovalRequired = errors.New("workflow requires human approval")
	ErrNoAffordableTask      = errors.New("workflow has no affordable task")
	ErrBudgetExhausted       = errors.New("workflow budget exhausted")
	ErrEvidenceConflict      = errors.New("workflow evidence conflict")
)

// RunRequest carries the immutable identity and authorization context for one Workflow Run.
type RunRequest struct {
	RunID       string
	ParentRunID string
	Round       int
	// BaseDepth carries nesting depth across parent Workflow invocations.
	BaseDepth    int
	Input        json.RawMessage
	InputHandoff *Handoff
	Actor        agentapi.Actor
	// ActorPermissions and ScenarioPermissions are intersected before node execution.
	ActorPermissions    agentapi.PermissionPolicy
	ScenarioPermissions agentapi.PermissionPolicy
	StartedAt           time.Time
}

// NodeRequest contains one attempt's resolved inputs and effective permissions.
type NodeRequest struct {
	WorkflowRunID string
	ParentRunID   string
	Round         int
	Depth         int
	Node          NodeDefinition
	Inputs        []Handoff
	// WorkflowInput preserves the immutable root contract for nodes whose
	// output must answer the original request rather than only a predecessor.
	WorkflowInput Handoff
	// UnavailablePredecessors preserves optional dependency failures for downstream decisions.
	UnavailablePredecessors []string
	UnavailableReasons      map[string]StopReason
	// BaselineEvidence prevents a node from presenting seeded evidence as newly discovered.
	BaselineEvidence []tool.EvidenceUnit
	Attempt          int
	// retryAccounted marks a retry charged by the coordinator before this
	// attempt. It is internal bookkeeping used to persist the retry exactly
	// once without charging the shared ledger twice.
	retryAccounted bool
	Actor          agentapi.Actor
	// EffectivePermissions is final and must not be widened by a node executor.
	EffectivePermissions agentapi.PermissionPolicy
}

// NodeExecutor executes one node without owning Workflow scheduling or persistence.
type NodeExecutor interface {
	Execute(context.Context, NodeRequest) (NodeResult, error)
}

// NodeDispatcher keeps agent and business transform lifecycles explicit.
type NodeDispatcher struct {
	agent     NodeExecutor
	transform NodeExecutor
}

// NewNodeDispatcher routes nodes to their specialized executors.
// Agent and deterministic transform lifecycles remain independently owned.
// A nil result means neither execution capability is available.
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
	Usage      Usage
	// accountedUsage is the subset of Usage already settled through the
	// per-attempt Run budget gate. The coordinator records only the remainder,
	// which keeps custom Runtimes that ignore the gate fully accounted as well.
	accountedUsage Usage
}

// RunObserver persists or projects node transitions without controlling scheduling.
type RunObserver interface {
	NodeStarted(context.Context, NodeRequest) error
	NodeSucceeded(context.Context, NodeRequest, NodeResult, *GateDecision) error
	NodeFailed(context.Context, NodeRequest, NodeResult, error) error
}

// GateEvaluator decides a named gate from the same immutable node request.
type GateEvaluator interface {
	Evaluate(context.Context, NodeRequest) (GateDecision, error)
}

// Result contains the converged Workflow output and aggregate resource usage.
type Result struct {
	RunID       string
	Output      Handoff
	NodeOutputs map[string]Handoff
	Gates       map[string]GateDecision
	Usage       Usage
	// StopReason explains a successful early stop as well as a terminal failure boundary.
	StopReason StopReason
}

// Progress is the resumable execution projection rebuilt from durable facts.
type Progress struct {
	StartedAt      time.Time
	Input          Handoff
	NodeOutputs    map[string]Handoff
	Gates          map[string]GateDecision
	FailedOptional map[string]struct{}
	// FailedOptionalReasons restores dependency availability without rerunning failed nodes.
	FailedOptionalReasons map[string]StopReason
	// WaitingHuman prevents approval-gated nodes from being scheduled during resume.
	WaitingHuman map[string]struct{}
	// NodeAttempts preserves retry counters and backoff deadlines across process restarts.
	NodeAttempts map[string]NodeAttemptProgress
	Usage        Usage
}

// NodeAttemptProgress preserves retry timing across a durable resume.
type NodeAttemptProgress struct {
	NextAttempt    int
	FirstStartedAt time.Time
	NotBefore      time.Time
}

// Orchestrator executes stable DAG waves while each node retains an independent context.
type Orchestrator struct {
	schemas            *agentapi.SchemaRegistry
	nodes              NodeExecutor
	gates              map[string]GateEvaluator
	capabilityMu       sync.Mutex
	capabilityLimiters map[capabilityLimitKey]*capabilityLimiter
}

// NewOrchestrator builds a Workflow DAG scheduler.
// Schema validation and node execution remain explicit dependencies.
// Gate evaluators are copied so caller mutation cannot alter scheduling.
func NewOrchestrator(
	schemas *agentapi.SchemaRegistry,
	nodes NodeExecutor,
	gates map[string]GateEvaluator,
) *Orchestrator {
	cloned := make(map[string]GateEvaluator, len(gates))
	for id, gate := range gates {
		cloned[id] = gate
	}
	return &Orchestrator{
		schemas:            schemas,
		nodes:              nodes,
		gates:              cloned,
		capabilityLimiters: make(map[capabilityLimitKey]*capabilityLimiter),
	}
}

func (orchestrator *Orchestrator) Run(ctx context.Context, definition Definition, request RunRequest) (Result, error) {
	return orchestrator.RunObserved(ctx, definition, request, nil)
}

// RunObserved executes one prepared Workflow as stable DAG waves.
// Node contexts remain independent while shared budgets converge centrally.
// Observer callbacks run at transition boundaries suitable for persistence.
func (orchestrator *Orchestrator) RunObserved(
	ctx context.Context,
	definition Definition,
	request RunRequest,
	observer RunObserver,
) (Result, error) {
	trace, ownsTrace := executionTrace(ctx)
	if ownsTrace {
		defer trace.Close()
	}
	ctx = runtrace.WithScope(ctx, trace)
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: request.RunID, ParentRunID: request.ParentRunID,
		WorkflowRunID: request.RunID,
	})
	prepared, metadata, err := orchestrator.prepareRun(definition, request)
	if err != nil {
		return Result{}, err
	}
	input, err := orchestrator.prepareInputHandoff(prepared, request)
	if err != nil {
		return Result{}, fmt.Errorf("workflow %q input: %w", prepared.ID, err)
	}
	startedAt := request.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	return orchestrator.runPrepared(ctx, prepared, metadata, request, Progress{
		StartedAt: startedAt, Input: input,
		NodeOutputs:           make(map[string]Handoff, len(prepared.Nodes)),
		Gates:                 make(map[string]GateDecision),
		FailedOptionalReasons: make(map[string]StopReason),
	}, observer)
}

func (orchestrator *Orchestrator) prepareInputHandoff(
	definition Definition,
	request RunRequest,
) (Handoff, error) {
	if request.InputHandoff == nil {
		return PrepareHandoff(Handoff{
			WorkflowRunID: request.RunID, ProducerNodeID: "workflow.input",
			Schema: definition.InputSchema, Payload: request.Input, Completeness: Complete,
		}, definition.Budget.MaxHandoffBytes, orchestrator.schemas)
	}
	input, err := PrepareHandoff(
		*request.InputHandoff,
		definition.Budget.MaxHandoffBytes,
		orchestrator.schemas,
	)
	if err != nil {
		return Handoff{}, err
	}
	if input.WorkflowRunID != request.RunID ||
		input.ProducerNodeID != "workflow.input" ||
		input.Schema != definition.InputSchema {
		return Handoff{}, fmt.Errorf("prepared workflow input identity does not match the run")
	}
	return input, nil
}

func (orchestrator *Orchestrator) ResumeObserved(
	ctx context.Context,
	definition Definition,
	request RunRequest,
	progress Progress,
	observer RunObserver,
) (Result, error) {
	trace, ownsTrace := executionTrace(ctx)
	if ownsTrace {
		defer trace.Close()
	}
	ctx = runtrace.WithScope(ctx, trace)
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: request.RunID, ParentRunID: request.ParentRunID,
		WorkflowRunID: request.RunID,
	})
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

func executionTrace(ctx context.Context) (*runtrace.Scope, bool) {
	inherited := runtrace.FromContext(ctx)
	trace := runtrace.Begin(ctx)
	return trace, trace != nil && inherited == nil
}

func (orchestrator *Orchestrator) prepareRun(
	definition Definition,
	request RunRequest,
) (Definition, graphMetadata, error) {
	if orchestrator == nil {
		return Definition{}, graphMetadata{}, fmt.Errorf("workflow orchestrator is unavailable")
	}
	prepared, err := prepareForExecution(definition, orchestrator.schemas)
	if err != nil {
		return Definition{}, graphMetadata{}, err
	}
	if request.RunID == "" {
		return Definition{}, graphMetadata{}, fmt.Errorf("workflow run id is required")
	}
	metadata, err := graph(prepared, orchestrator.schemas)
	if err != nil {
		return Definition{}, graphMetadata{}, err
	}
	return prepared, metadata, nil
}

type dispatchInput struct {
	definition Definition
	ready      []string
}

type dispatchOutput struct {
	outcomes []nodeOutcome
}

var dispatchTraceSpec = runtrace.Spec[dispatchInput, dispatchOutput]{
	Operation: "multi_agent.dispatch",
	Node:      "multi_agent_dispatch",
	Input: func(input dispatchInput) map[string]any {
		return map[string]any{
			"workflow_id":    input.definition.ID,
			"ready_nodes":    append([]string(nil), input.ready...),
			"parallel_limit": input.definition.Budget.MaxParallelism,
		}
	},
	Output: func(_ dispatchInput, output dispatchOutput, _ error) map[string]any {
		childRunIDs := make([]string, 0, len(output.outcomes))
		failed := 0
		for _, outcome := range output.outcomes {
			if outcome.nodeResult.AgentRunID != "" {
				childRunIDs = append(childRunIDs, outcome.nodeResult.AgentRunID)
			}
			if outcome.err != nil {
				failed++
			}
		}
		return map[string]any{
			"child_run_ids": childRunIDs,
			"dispatched":    len(output.outcomes),
			"failed":        failed,
		}
	},
	Record: func(output dispatchOutput, _ error) bool {
		return len(output.outcomes) > 1
	},
}

type nodeOutcome struct {
	handoff    Handoff
	nodeResult NodeResult
	gate       *GateDecision
	err        error
	retryable  bool
}

var nodeTraceSpec = runtrace.Spec[NodeRequest, nodeOutcome]{
	Operation: "workflow.node.execute",
	Node:      "workflow_node",
	Input: func(request NodeRequest) map[string]any {
		return map[string]any{"input_count": len(request.Inputs)}
	},
	Output: func(request NodeRequest, result nodeOutcome, err error) map[string]any {
		output := map[string]any{
			"workflow_run_id": request.WorkflowRunID,
			"round":           request.Round,
			"depth":           request.Depth,
			"node_id":         request.Node.ID,
			"node_kind":       request.Node.Kind,
			"attempt":         request.Attempt,
			"agent_run_id":    result.nodeResult.AgentRunID,
		}
		if err != nil {
			output["error"] = err.Error()
		}
		if result.gate != nil {
			output["gate_decision"] = result.gate.Decision
			output["gate_reason_codes"] = append(
				[]string(nil),
				result.gate.ReasonCodes...,
			)
			output["gate_finding_count"] = len(result.gate.FindingIDs)
		}
		return output
	},
	Status: func(_ nodeOutcome, err error) string {
		if errors.Is(err, ErrHumanApprovalRequired) {
			return "waiting_human"
		}
		return ""
	},
}
