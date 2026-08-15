package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/scope"
)

// AgentExecutor binds Workflow nodes to the shared immutable Agent Runtime.
type AgentExecutor struct {
	schemas *agentapi.SchemaRegistry
	agents  AgentResolver
	runtime agentapi.Runtime
}

func NewAgentExecutor(
	schemas *agentapi.SchemaRegistry,
	agents AgentResolver,
	runtime agentapi.Runtime,
) (*AgentExecutor, error) {
	if schemas == nil {
		return nil, fmt.Errorf("agent node executor: schema registry is required")
	}
	if agents == nil {
		return nil, fmt.Errorf("agent node executor: definition resolver is required")
	}
	if runtime == nil {
		return nil, fmt.Errorf("agent node executor: runtime is required")
	}
	return &AgentExecutor{schemas: schemas, agents: agents, runtime: runtime}, nil
}

func (executor *AgentExecutor) Execute(
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
	input := request.Inputs[0]
	contextBlock, err := contextFromHandoff(input)
	if err != nil {
		return NodeResult{}, fmt.Errorf("agent node %q context: %w", request.Node.ID, err)
	}
	contextBlocks := []agentapi.ContextBlock{contextBlock}
	if request.Node.Task != nil {
		taskBlock, err := contextFromDirective(*request.Node.Task)
		if err != nil {
			return NodeResult{}, fmt.Errorf("agent node %q task context: %w", request.Node.ID, err)
		}
		contextBlocks = append(contextBlocks, taskBlock)
	}
	runRequest := agentapi.RunRequest{
		RunID: runID, Agent: request.Node.Agent, DefinitionHash: definition.ContentHash,
		Input: input.Payload, Context: contextBlocks,
		Permissions: permissions,
		ToolScope: agentapi.ToolScope{
			AllowWrite:      scope.Has(permissions.Scopes, scope.KnowledgeWrite),
			RestrictVisible: request.Node.RestrictVisibleTools,
			VisibleToolIDs:  append([]string(nil), request.Node.VisibleToolIDs...),
		},
		Policy: agentapi.RunPolicy{
			EvidenceSeeded: len(input.EvidenceUnits) > 0,
			MaxToolCalls:   request.Node.Budget.MaxToolCalls,
		},
		Actor: request.Actor,
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
		childRunTraceSpec,
		runRequest,
		func(ctx context.Context, runRequest agentapi.RunRequest) (agentapi.RunResult, error) {
			return executor.runtime.Run(ctx, runRequest)
		},
	)
	nodeResult := NodeResult{
		AgentRunID: runID,
		Usage: Usage{
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
	completeness := input.Completeness
	if request.Node.Task != nil &&
		request.Node.OutputSchema.ID == "investigation.report" &&
		len(request.Node.Task.RequiredFacets) > 0 {
		reportCompleteness, err := reportCompleteness(
			input.Payload,
			result.Output,
			request.Node.Task.RequiredFacets,
		)
		if err != nil {
			return nodeResult, fmt.Errorf(
				"agent node %q goal coverage: %w",
				request.Node.ID,
				err,
			)
		}
		completeness = leastComplete(completeness, reportCompleteness)
	}
	evidenceUnits, evidenceConflicts := mergeHandoffEvidence([]Handoff{
		input,
		{
			ProducerNodeID:    request.Node.ID,
			EvidenceUnits:     result.EvidenceUnits,
			EvidenceConflicts: result.EvidenceConflicts,
		},
	})
	nodeResult.Handoff = Handoff{
		WorkflowRunID:  request.WorkflowRunID,
		ProducerNodeID: request.Node.ID,
		ProducerRunID:  runID,
		Schema:         request.Node.OutputSchema,
		Payload:        append([]byte(nil), result.Output...),
		References: append(
			append([]agentapi.Reference(nil), input.References...),
			result.References...,
		),
		EvidenceUnits:     evidenceUnits,
		EvidenceConflicts: evidenceConflicts,
		Completeness:      completeness,
	}
	return nodeResult, nil
}

type contractCoverage struct {
	EvidenceGoals []struct {
		ID              string `json:"id"`
		Facet           string `json:"facet"`
		MinimumCoverage int    `json:"minimum_coverage"`
	} `json:"evidence_goals"`
}

type reportCoverage struct {
	CoveredGoals    []string `json:"covered_goals"`
	UnresolvedGoals []string `json:"unresolved_goals"`
	Findings        []struct {
		GoalIDs  []string          `json:"goal_ids"`
		Evidence []json.RawMessage `json:"evidence"`
	} `json:"findings"`
}

func reportCompleteness(
	contractPayload json.RawMessage,
	reportPayload json.RawMessage,
	requiredFacets []string,
) (Completeness, error) {
	required := make(map[string]struct{}, len(requiredFacets))
	for _, facet := range requiredFacets {
		required[facet] = struct{}{}
	}
	var report reportCoverage
	if err := json.Unmarshal(reportPayload, &report); err != nil {
		return "", fmt.Errorf("decode investigation report: %w", err)
	}
	status := make(map[string]Completeness, len(required))
	for _, goal := range report.CoveredGoals {
		if _, ok := required[goal]; !ok {
			return "", fmt.Errorf("covered goal %q was not requested", goal)
		}
		if _, duplicate := status[goal]; duplicate {
			return "", fmt.Errorf("goal %q is reported more than once", goal)
		}
		status[goal] = Complete
	}
	for _, goal := range report.UnresolvedGoals {
		if _, ok := required[goal]; !ok {
			return "", fmt.Errorf("unresolved goal %q was not requested", goal)
		}
		if _, duplicate := status[goal]; duplicate {
			return "", fmt.Errorf("goal %q is both covered and unresolved", goal)
		}
		status[goal] = Unavailable
	}
	for _, facet := range requiredFacets {
		if _, ok := status[facet]; !ok {
			return "", fmt.Errorf("required goal %q is not classified", facet)
		}
	}

	minimum := minimumCoverage(contractPayload)
	findingCounts := make(map[string]int, len(report.CoveredGoals))
	for index, finding := range report.Findings {
		if len(finding.Evidence) == 0 {
			return "", fmt.Errorf("finding %d has no concrete evidence", index)
		}
		seen := make(map[string]struct{}, len(finding.GoalIDs))
		for _, goal := range finding.GoalIDs {
			if _, ok := required[goal]; !ok {
				return "", fmt.Errorf("finding %d references unrequested goal %q", index, goal)
			}
			if status[goal] != Complete {
				return "", fmt.Errorf("finding %d references unresolved goal %q", index, goal)
			}
			if _, duplicate := seen[goal]; duplicate {
				continue
			}
			seen[goal] = struct{}{}
			findingCounts[goal]++
		}
	}
	covered := 0
	for _, facet := range requiredFacets {
		if status[facet] != Complete {
			continue
		}
		needed := minimum[facet]
		if needed <= 0 {
			needed = 1
		}
		if findingCounts[facet] < needed {
			return "", fmt.Errorf(
				"covered goal %q has %d evidence-backed findings; minimum is %d",
				facet,
				findingCounts[facet],
				needed,
			)
		}
		covered++
	}
	switch {
	case covered == len(requiredFacets):
		return Complete, nil
	case covered == 0:
		return Unavailable, nil
	default:
		return Partial, nil
	}
}

func minimumCoverage(payload json.RawMessage) map[string]int {
	var contract contractCoverage
	if err := json.Unmarshal(payload, &contract); err != nil {
		return nil
	}
	minimum := make(map[string]int, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		if goal.MinimumCoverage <= 0 {
			continue
		}
		minimum[goal.Facet] = goal.MinimumCoverage
		if goal.ID != "" {
			minimum[goal.ID] = goal.MinimumCoverage
		}
	}
	return minimum
}

func leastComplete(left, right Completeness) Completeness {
	if left == Unavailable || right == Unavailable {
		return Unavailable
	}
	if left == Partial || right == Partial {
		return Partial
	}
	return Complete
}

var childRunTraceSpec = runtrace.Spec[agentapi.RunRequest, agentapi.RunResult]{
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
