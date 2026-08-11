package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/scope"
)

// AgentNodeExecutor binds Workflow nodes to the shared immutable Agent Runtime.
type AgentNodeExecutor struct {
	schemas *agentapi.SchemaRegistry
	agents  AgentDefinitionResolver
	runtime agentapi.Runtime
}

func NewAgentNodeExecutor(
	schemas *agentapi.SchemaRegistry,
	agents AgentDefinitionResolver,
	runtime agentapi.Runtime,
) (*AgentNodeExecutor, error) {
	if schemas == nil {
		return nil, fmt.Errorf("agent node executor: schema registry is required")
	}
	if agents == nil {
		return nil, fmt.Errorf("agent node executor: definition resolver is required")
	}
	if runtime == nil {
		return nil, fmt.Errorf("agent node executor: runtime is required")
	}
	return &AgentNodeExecutor{schemas: schemas, agents: agents, runtime: runtime}, nil
}

func (executor *AgentNodeExecutor) Execute(
	ctx context.Context,
	request NodeRequest,
) (NodeResult, error) {
	if executor == nil || executor.schemas == nil || executor.agents == nil || executor.runtime == nil {
		return NodeResult{}, fmt.Errorf("agent node executor is unavailable")
	}
	if request.Node.Kind != NodeAgent {
		return NodeResult{}, fmt.Errorf("node %q kind %q is not an agent", request.Node.ID, request.Node.Kind)
	}
	if len(request.Inputs) != 1 {
		return NodeResult{}, fmt.Errorf(
			"agent node %q requires exactly one handoff; use a join or transform for %d inputs",
			request.Node.ID,
			len(request.Inputs),
		)
	}
	definition, err := executor.agents.Resolve(request.Node.Agent)
	if err != nil {
		return NodeResult{}, fmt.Errorf("resolve agent node %q definition: %w", request.Node.ID, err)
	}
	if definition.ID != request.Node.Agent.ID || definition.Version != request.Node.Agent.Version {
		return NodeResult{}, fmt.Errorf("agent node %q definition is not pinned", request.Node.ID)
	}
	if err := executor.schemas.ValidateCompatibility(request.Node.InputSchema, definition.InputSchema); err != nil {
		return NodeResult{}, fmt.Errorf("agent node %q input schema: %w", request.Node.ID, err)
	}
	if err := executor.schemas.ValidateCompatibility(definition.OutputSchema, request.Node.OutputSchema); err != nil {
		return NodeResult{}, fmt.Errorf("agent node %q output schema: %w", request.Node.ID, err)
	}
	permissions := IntersectPermissions(request.EffectivePermissions, definition.Permissions)
	runID, err := randomRunID()
	if err != nil {
		return NodeResult{}, err
	}
	runRequest := agentapi.RunRequest{
		RunID: runID, Agent: request.Node.Agent, DefinitionHash: definition.ContentHash,
		Input: request.Inputs[0].Payload, Permissions: permissions,
		ToolScope: agentapi.ToolScope{
			AllowWrite: scope.Has(permissions.Scopes, scope.KnowledgeWrite),
		},
		Policy: agentapi.RunPolicy{MaxToolCalls: request.Node.Budget.MaxToolCalls},
		Actor:  request.Actor,
		Correlation: agentapi.Correlation{
			ParentRunID:   request.WorkflowRunID,
			WorkflowRunID: request.WorkflowRunID,
			NodeID:        request.Node.ID,
		},
	}
	childCtx := runtrace.WithCorrelation(ctx, runtrace.Correlation{
		RunID: runID, ParentRunID: request.WorkflowRunID,
		WorkflowRunID: request.WorkflowRunID, AgentRunID: runID,
		WorkflowNodeID: request.Node.ID,
	})
	result, err := runtrace.Invoke(
		childCtx,
		multiAgentChildRunTraceSpec,
		runRequest,
		func(ctx context.Context, runRequest agentapi.RunRequest) (agentapi.RunResult, error) {
			return executor.runtime.Run(ctx, runRequest)
		},
	)
	nodeResult := NodeResult{
		AgentRunID: runID,
		Usage: WorkflowUsage{
			InputTokens:     result.Usage.InputTokens,
			OutputTokens:    result.Usage.OutputTokens,
			ReasoningTokens: result.Usage.ReasoningTokens,
			TotalTokens:     result.Usage.TotalTokens,
			ToolCalls:       int64(result.Evidence.ToolCallCount),
			CostMicros:      result.Usage.CostMicros,
		},
	}
	if err != nil {
		return nodeResult, fmt.Errorf("run agent node %q: %w", request.Node.ID, err)
	}
	if result.Status != agentapi.RunSucceeded {
		if result.Error == nil {
			return nodeResult, fmt.Errorf("agent node %q run %q failed", request.Node.ID, runID)
		}
		return nodeResult, &agentNodeRunError{
			message: fmt.Sprintf(
				"agent node %q run %q failed (%s): %s",
				request.Node.ID,
				runID,
				result.Error.Code,
				result.Error.Message,
			),
			retryable: result.Error.Retryable,
		}
	}
	nodeResult.Handoff = Handoff{
		WorkflowRunID:  request.WorkflowRunID,
		ProducerNodeID: request.Node.ID,
		ProducerRunID:  runID,
		Schema:         request.Node.OutputSchema,
		Payload:        append([]byte(nil), result.Output...),
		References:     append([]agentapi.Reference(nil), result.References...),
		Completeness:   Complete,
	}
	return nodeResult, nil
}

var multiAgentChildRunTraceSpec = runtrace.Spec[agentapi.RunRequest, agentapi.RunResult]{
	Operation: "multi_agent.child_run",
	Node:      "multi_agent_child_run",
	Input: func(request agentapi.RunRequest) map[string]any {
		return map[string]any{
			"agent_id":         request.Agent.ID,
			"agent_version":    request.Agent.Version,
			"workflow_node_id": request.Correlation.NodeID,
		}
	},
	Output: func(request agentapi.RunRequest, result agentapi.RunResult, err error) map[string]any {
		fields := map[string]any{
			"agent_run_id":  request.RunID,
			"run_status":    result.Status,
			"input_tokens":  result.Usage.InputTokens,
			"output_tokens": result.Usage.OutputTokens,
			"total_tokens":  result.Usage.TotalTokens,
			"tool_calls":    result.Evidence.ToolCallCount,
		}
		if err != nil {
			fields["error"] = err.Error()
		} else if result.Error != nil {
			fields["error_code"] = result.Error.Code
		}
		return fields
	},
	Status: func(result agentapi.RunResult, err error) string {
		if err != nil {
			return ""
		}
		switch result.Status {
		case agentapi.RunFailed:
			return "failed"
		case agentapi.RunCancelled:
			return "cancelled"
		default:
			return ""
		}
	},
}

type agentNodeRunError struct {
	message   string
	retryable bool
}

func (err *agentNodeRunError) Error() string {
	return err.message
}

func (err *agentNodeRunError) Retryable() bool {
	return err.retryable
}

var _ interface {
	error
	Retryable() bool
} = (*agentNodeRunError)(nil)

func randomRunID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate agent run id: %w", err)
	}
	return "run_" + hex.EncodeToString(id[:]), nil
}
