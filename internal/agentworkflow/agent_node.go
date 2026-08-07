package agentworkflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
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
	result, err := executor.runtime.Run(ctx, agentapi.RunRequest{
		RunID: runID, Agent: request.Node.Agent, DefinitionHash: definition.ContentHash,
		Input: request.Inputs[0].Payload, Permissions: permissions,
		ToolScope: agentapi.ToolScope{
			AllowWrite: platformscope.Has(permissions.Scopes, platformscope.KnowledgeWrite),
		},
		Policy: agentapi.RunPolicy{MaxToolCalls: request.Node.Budget.MaxToolCalls},
		Actor:  request.Actor,
		Correlation: agentapi.Correlation{
			WorkflowRunID: request.WorkflowRunID,
			NodeID:        request.Node.ID,
		},
	})
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
