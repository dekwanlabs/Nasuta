package investigation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/tool"
)

type TaskExecutor interface {
	Execute(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error)
}

type TaskExecutorFunc func(context.Context, ExecutableTask, TaskExecutionInput) (TaskExecutionResult, error)

func (fn TaskExecutorFunc) Execute(ctx context.Context, task ExecutableTask, input TaskExecutionInput) (TaskExecutionResult, error) {
	return fn(ctx, task, input)
}

// ExecutorRegistry resolves the immutable execution boundary selected by a task plan.
type ExecutorRegistry interface {
	Resolve(ExecutorType) (TaskExecutor, error)
}

type executorRegistry struct {
	executors map[ExecutorType]TaskExecutor
}

// NewExecutorRegistry creates a registry from explicit executor bindings.
func NewExecutorRegistry(bindings map[ExecutorType]TaskExecutor) ExecutorRegistry {
	cloned := make(map[ExecutorType]TaskExecutor, len(bindings))
	for executorType, executor := range bindings {
		if executor != nil {
			cloned[executorType] = executor
		}
	}
	return executorRegistry{executors: cloned}
}

func (registry executorRegistry) Resolve(executorType ExecutorType) (TaskExecutor, error) {
	executor, ok := registry.executors[executorType]
	if !ok {
		return nil, fmt.Errorf("%w: executor %q is not registered", ErrCapabilityGap, executorType)
	}
	return executor, nil
}

// ToolPipelineExecutor runs only the tool calls compiled into a task template.
type ToolPipelineExecutor struct {
	Executor *tool.Executor
	Snapshot tool.Snapshot
}

func (executor ToolPipelineExecutor) Execute(
	ctx context.Context,
	task ExecutableTask,
	_ TaskExecutionInput,
) (TaskExecutionResult, error) {
	if executor.Executor == nil {
		return TaskExecutionResult{}, fmt.Errorf("tool executor is required")
	}
	outputs := make([]map[string]any, 0, len(task.ToolCalls))
	result := TaskExecutionResult{}
	for _, call := range task.ToolCalls {
		if !grantedTool(task.AllowedTools, call.ToolID) {
			return result, fmt.Errorf("task %q attempted ungranted tool %q", task.ID, call.ToolID)
		}
		toolResult, err := executor.Executor.Execute(ctx, executor.Snapshot, call.ToolID, call.Args)
		result.Usage.ToolCalls++
		if err != nil {
			return result, err
		}
		outputs = append(outputs, map[string]any{
			"tool_id": call.ToolID,
			"content": toolResult.Content,
		})
		appendToolEvidence(&result, call.ToolID, toolResult)
	}
	output, err := json.Marshal(outputs)
	if err != nil {
		return result, fmt.Errorf("encode task %q output: %w", task.ID, err)
	}
	result.Output = output
	return result, nil
}

// DirectToolExecutor executes exactly the single deterministic tool compiled into a task.
type DirectToolExecutor struct {
	Executor *tool.Executor
	Snapshot tool.Snapshot
}

func (executor DirectToolExecutor) Execute(
	ctx context.Context,
	task ExecutableTask,
	_ TaskExecutionInput,
) (TaskExecutionResult, error) {
	if executor.Executor == nil {
		return TaskExecutionResult{}, fmt.Errorf("tool executor is required")
	}
	if len(task.ToolCalls) != 1 {
		return TaskExecutionResult{}, fmt.Errorf("task %q direct_tool requires exactly one tool call, got %d", task.ID, len(task.ToolCalls))
	}
	call := task.ToolCalls[0]
	if !grantedTool(task.AllowedTools, call.ToolID) {
		return TaskExecutionResult{}, fmt.Errorf("task %q attempted ungranted tool %q", task.ID, call.ToolID)
	}
	result := TaskExecutionResult{}
	args := boundToolArguments(task, call)
	toolResult, err := executor.Executor.Execute(ctx, executor.Snapshot, call.ToolID, args)
	result.Usage.ToolCalls++
	if err != nil {
		return result, err
	}
	appendToolEvidence(&result, call.ToolID, toolResult)
	output, err := json.Marshal(map[string]any{"tool_id": call.ToolID, "content": toolResult.Content})
	if err != nil {
		return result, fmt.Errorf("encode task %q output: %w", task.ID, err)
	}
	result.Output = output
	return result, nil
}

func boundToolArguments(task ExecutableTask, call ToolCallSpec) tool.Arguments {
	if len(call.Args) > 0 {
		return call.Args
	}
	args := tool.Arguments{}
	objective := strings.TrimSpace(task.Objective)
	if objective == "" {
		return args
	}
	entity := ""
	if len(task.Entities) > 0 {
		entity = strings.TrimSpace(task.Entities[0])
	}
	switch call.ToolID {
	case "get_service", "search_code", "get_symbol", "trace_calls", "search_runbooks", "check_docs":
		args["query"] = objective
	case "trace_deps":
		if entity != "" {
			args["service"] = entity
		} else {
			args["service"] = objective
		}
	case "list_apis":
		if entity != "" {
			args["service"] = entity
		}
		args["keyword"] = objective
	}
	return args
}

func grantedTool(allowed []tool.ToolID, wanted tool.ToolID) bool {
	for _, id := range allowed {
		if id == wanted {
			return true
		}
	}
	return false
}

func appendToolEvidence(result *TaskExecutionResult, toolID tool.ToolID, toolResult tool.Result) {
	if strings.TrimSpace(toolResult.Content) == "" {
		return
	}
	if len(toolResult.EvidenceUnits) == 0 {
		result.EvidenceCandidates = append(result.EvidenceCandidates, EvidenceCandidate{
			SourceKind: "tool",
			Target:     string(toolID),
			Content:    toolResult.Content,
		})
		return
	}
	for _, evidence := range toolResult.EvidenceUnits {
		result.EvidenceCandidates = append(result.EvidenceCandidates, EvidenceCandidate{
			SourceKind:    evidence.SourceKind,
			Target:        evidence.Target,
			Section:       firstString(evidence.Sections),
			Content:       toolResult.Content,
			ContentHash:   evidence.ContentHash,
			Facets:        append([]string(nil), evidence.Facets...),
			TrustTier:     evidence.TrustTier,
			EvidenceClass: evidence.EvidenceClass,
			Version:       evidence.Version,
			TimeRange:     evidence.TimeRange,
		})
	}
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
