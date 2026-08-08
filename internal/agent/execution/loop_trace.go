package execution

import (
	"time"

	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

type agentModelTurnInput struct {
	Step     int
	Messages []llm.Message
	Tools    []llm.ToolDef
	Stream   *StreamPipe
}

type agentModelTurnOutput struct {
	Result *llm.ChatStreamResult
	Timing StreamTiming
}

type forceConclusionInput struct {
	RunID          string
	Messages       []llm.Message
	AnswerContract *exactAnswerContract
}

type forceConclusionOutput struct {
	Result         *llm.ChatStreamResult
	Stream         *StreamPipe
	Timing         StreamTiming
	AttemptStarted time.Time
}

type historyCompileInput struct {
	Messages []llm.Message
}

var historyCompileSpec = executiontrace.Spec[historyCompileInput, []llm.Message]{
	Operation: "agent.history_compile",
	Node:      "history_compile",
	Input: func(input historyCompileInput) map[string]any {
		return map[string]any{
			"compiled_messages": len(input.Messages),
			"compiled_chars":    messageChars(input.Messages),
		}
	},
	Output: func(_ historyCompileInput, output []llm.Message, _ error) map[string]any {
		return map[string]any{
			"messages":      len(output),
			"context_chars": contextChars(output),
		}
	},
}

type toolPruningInput struct {
	Tools   []llm.ToolDef
	Offered map[tool.ToolID]struct{}
	Applied bool
}

type toolPruningOutput struct {
	Effective    []llm.ToolDef
	FullTokens   int
	PrunedTokens int
	RemovedIDs   []string
}

var toolPruningSpec = executiontrace.Spec[toolPruningInput, toolPruningOutput]{
	Operation: "agent.tool_pruning",
	Node:      "tool_pruning",
	Input: func(input toolPruningInput) map[string]any {
		return map[string]any{"applied": input.Applied}
	},
	Output: func(input toolPruningInput, output toolPruningOutput, _ error) map[string]any {
		return map[string]any{
			"offered":          len(output.Effective),
			"total":            len(input.Tools),
			"full_tokens":      output.FullTokens,
			"pruned_tokens":    output.PrunedTokens,
			"saved_tokens":     output.FullTokens - output.PrunedTokens,
			"removed_tool_ids": output.RemovedIDs,
		}
	},
}

type contextBudgetInput struct {
	Step     int
	Messages []llm.Message
	Tools    []llm.ToolDef
}

var contextBudgetSpec = executiontrace.Spec[contextBudgetInput, struct{}]{
	Operation: "agent.context_budget",
	Node:      "context_budget",
	Input: func(input contextBudgetInput) map[string]any {
		return map[string]any{
			"step":     input.Step,
			"messages": len(input.Messages),
			"tools":    len(input.Tools),
		}
	},
	Output: func(_ contextBudgetInput, _ struct{}, err error) map[string]any {
		return map[string]any{"error": err.Error()}
	},
	Record: func(_ struct{}, err error) bool {
		return err != nil
	},
}

var agentModelTurnSpec = executiontrace.Spec[agentModelTurnInput, agentModelTurnOutput]{
	Operation: "agent.model_turn",
	Node:      "agent_model_turn",
	Input: func(input agentModelTurnInput) map[string]any {
		return map[string]any{
			"step":     input.Step,
			"messages": len(input.Messages),
			"tools":    len(input.Tools),
		}
	},
	Output: func(_ agentModelTurnInput, output agentModelTurnOutput, err error) map[string]any {
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		toolNames := make([]string, 0, len(output.Result.ToolCalls))
		for _, call := range output.Result.ToolCalls {
			toolNames = append(toolNames, call.Function.Name)
		}
		return map[string]any{
			"finish_reason":       output.Result.FinishReason,
			"tool_calls":          toolNames,
			"content_chars":       len([]rune(output.Result.Content)),
			"reasoning_tokens":    output.Result.ReasoningTokens,
			"first_event_ms":      output.Timing.FirstEvent.Milliseconds(),
			"first_reasoning_ms":  output.Timing.FirstReasoning.Milliseconds(),
			"first_content_ms":    output.Timing.FirstContent.Milliseconds(),
			"first_tool_delta_ms": output.Timing.FirstToolDelta.Milliseconds(),
			"first_tool_call_ms":  output.Timing.FirstToolCall.Milliseconds(),
		}
	},
}

var forceConclusionSpec = executiontrace.Spec[*forceConclusionInput, forceConclusionOutput]{
	Operation: "agent.force_conclusion",
	Node:      "force_conclusion",
	Input: func(input *forceConclusionInput) map[string]any {
		return map[string]any{"messages": len(input.Messages)}
	},
	Output: func(_ *forceConclusionInput, output forceConclusionOutput, err error) map[string]any {
		result := map[string]any{
			"first_event_ms":     output.Timing.FirstEvent.Milliseconds(),
			"first_reasoning_ms": output.Timing.FirstReasoning.Milliseconds(),
			"first_content_ms":   output.Timing.FirstContent.Milliseconds(),
		}
		if err != nil {
			result["error"] = err.Error()
		}
		if output.Result != nil {
			result["finish_reason"] = output.Result.FinishReason
			result["content_chars"] = len([]rune(output.Result.Content))
			result["reasoning_tokens"] = output.Result.ReasoningTokens
		}
		return result
	},
}

type firstAnswerTokenTraceInput struct {
	Step         any
	TurnTTFTMS   int64
	RunElapsedMS int64
}

var firstAnswerTokenTraceSpec = executiontrace.Spec[firstAnswerTokenTraceInput, struct{}]{
	Operation: "agent.first_answer_token",
	Node:      "first_answer_token",
	Output: func(input firstAnswerTokenTraceInput, _ struct{}, _ error) map[string]any {
		return map[string]any{
			"step":           input.Step,
			"turn_ttft_ms":   input.TurnTTFTMS,
			"run_elapsed_ms": input.RunElapsedMS,
		}
	},
}
