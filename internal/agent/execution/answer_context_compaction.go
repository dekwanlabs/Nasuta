package execution

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
)

const (
	answerContextHighWaterPercent = run.ContextHighWaterPercent
	answerContextTargetPercent    = 60
	oldToolResultFloorTokens      = 96
	recentToolResultFloorTokens   = 256
	emergencyToolResultFloor      = 16
)

type answerCompactionResult struct {
	Triggered              bool
	Applied                bool
	ProjectedBeforeTokens  int
	ProjectedAfterTokens   int
	AssistantTokensRemoved int
	ToolResultsCompressed  int
	ToolResults            []toolResultCompaction
}

type toolResultCompaction struct {
	ToolCallID     string `json:"tool_call_id"`
	Tool           string `json:"tool"`
	Strategy       string `json:"strategy"`
	OriginalTokens int    `json:"original_tokens"`
	BeforeTokens   int    `json:"before_tokens"`
	RetainedTokens int    `json:"retained_tokens"`
	ChunkCoverage  string `json:"chunk_coverage"`
	ItemCoverage   string `json:"item_coverage"`
	FieldCoverage  string `json:"field_coverage"`
}

type toolResultCandidate struct {
	index          int
	toolCallID     string
	tool           string
	sourceContent  string
	originalTokens int
	budgetTokens   int
}

type answerCompactionInput struct {
	State *compiledLoop
	Tools []llm.ToolDef
	Phase string
}

var answerCompactionSpec = runtrace.Spec[
	answerCompactionInput,
	answerCompactionResult,
]{
	Operation: "agent.answer_context_compaction",
	Node:      "answer_context_compaction",
	Input: func(input answerCompactionInput) map[string]any {
		return map[string]any{"phase": input.Phase}
	},
	Output: func(
		input answerCompactionInput,
		output answerCompactionResult,
		err error,
	) map[string]any {
		projected := map[string]any{
			"phase":                    input.Phase,
			"triggered":                output.Triggered,
			"applied":                  output.Applied,
			"projected_before_tokens":  output.ProjectedBeforeTokens,
			"projected_after_tokens":   output.ProjectedAfterTokens,
			"assistant_tokens_removed": output.AssistantTokensRemoved,
			"tool_results_compressed":  output.ToolResultsCompressed,
			"tool_results":             output.ToolResults,
		}
		if err != nil {
			projected["error"] = err.Error()
		}
		return projected
	},
	Record: func(output answerCompactionResult, err error) bool {
		return output.Triggered || err != nil
	},
}

// compactAnswerContext bounds transient tool-loop context before the
// next model call. It changes only the model-side copy; session and trace
// records continue to retain the authoritative tool output.
func (agent *Agent) compactAnswerContext(
	state *compiledLoop,
	tools []llm.ToolDef,
	phase string,
) (answerCompactionResult, error) {
	input := answerCompactionInput{State: state, Tools: tools, Phase: phase}
	return runtrace.Invoke(
		stateContext(state),
		answerCompactionSpec,
		input,
		func(_ context.Context, input answerCompactionInput) (answerCompactionResult, error) {
			return agent.compactContext(input.State, input.Tools, input.Phase)
		},
	)
}

func (agent *Agent) compactContext(
	state *compiledLoop,
	tools []llm.ToolDef,
	phase string,
) (answerCompactionResult, error) {
	var result answerCompactionResult
	if state == nil || agent.cfg.ContextWindow <= 0 {
		return result, nil
	}

	inputTokens, err := estimateInputTokens(state.messages, tools)
	if err != nil {
		return result, fmt.Errorf("measure %s context: %w", phase, err)
	}
	outputReserve := agent.outputReserve()
	projectedBefore := inputTokens + outputReserve
	result.ProjectedBeforeTokens = projectedBefore
	result.ProjectedAfterTokens = projectedBefore

	window := agent.cfg.ContextWindow
	highWater := run.ContextHighWaterTokens(window)
	safety := run.ContextSafetyTokens(window)
	defer func() {
		emitContextUsage(agent.observer, state.runID, run.ContextUsageEvent{
			Phase:                 phase,
			ProjectedBeforeTokens: result.ProjectedBeforeTokens,
			ProjectedAfterTokens:  result.ProjectedAfterTokens,
			ContextWindow:         window,
			HighWaterTokens:       highWater,
			SafetyTokens:          safety,
			SafeLimitTokens:       run.ContextSafeLimitTokens(window),
			OutputReserveTokens:   outputReserve,
			CompactionTriggered:   result.Triggered,
			CompactionApplied:     result.Applied,
		})
	}()
	if projectedBefore < highWater {
		return result, nil
	}
	result.Triggered = true

	start := min(max(0, state.initialMessageCount), len(state.messages))
	if state.answerToolSources == nil {
		state.answerToolSources = make(map[int]string)
	}
	messages := append([]llm.Message(nil), state.messages...)
	target := window * answerContextTargetPercent / 100
	needed := max(0, projectedBefore-target)

	// Tool-call narration is not authoritative evidence. Remove older narration
	// first while retaining the assistant tool_calls and tool result pairing.
	for index := start; index < len(messages) && needed > 0; index++ {
		message := &messages[index]
		if message.Role != "assistant" || len(message.ToolCalls) == 0 || message.Content == "" {
			continue
		}
		removed := tooloutput.EstimateTokens(message.Content)
		message.Content = ""
		result.AssistantTokensRemoved += removed
		needed = max(0, needed-removed)
	}

	currentInputTokens, err := estimateInputTokens(messages, tools)
	if err != nil {
		return result, fmt.Errorf("remeasure %s context: %w", phase, err)
	}
	candidates := toolResultCandidates(
		messages,
		start,
		state.answerToolSources,
	)
	needed = max(0, currentInputTokens+outputReserve-target)
	allocateToolBudgets(
		candidates,
		needed,
		oldToolResultFloorTokens,
		recentToolResultFloorTokens,
	)

	plannedReduction := plannedToolReduction(candidates)
	hardInputLimit := window - outputReserve - contextSafetyTokens(window)
	hardOverflow := max(0, currentInputTokens-plannedReduction-hardInputLimit)
	if hardOverflow > 0 {
		allocateToolBudgets(
			candidates,
			hardOverflow,
			emergencyToolResultFloor,
			emergencyToolResultFloor,
		)
	}

	for _, candidate := range candidates {
		if candidate.budgetTokens >= candidate.originalTokens {
			continue
		}
		compressed := tooloutput.Compress(tooloutput.Request{
			Question:  state.input.Question,
			Content:   candidate.sourceContent,
			MaxTokens: candidate.budgetTokens,
		})
		retainedTokens := tooloutput.EstimateTokens(compressed.Content)
		if retainedTokens >= candidate.originalTokens ||
			compressed.Content == messages[candidate.index].Content {
			continue
		}
		messages[candidate.index].Content = compressed.Content
		if _, exists := state.answerToolSources[candidate.index]; !exists {
			state.answerToolSources[candidate.index] = candidate.sourceContent
		}
		result.ToolResultsCompressed++
		result.ToolResults = append(result.ToolResults, toolResultCompaction{
			ToolCallID:     candidate.toolCallID,
			Tool:           candidate.tool,
			Strategy:       compressed.Strategy,
			OriginalTokens: compressed.OriginalTokens,
			BeforeTokens:   candidate.originalTokens,
			RetainedTokens: retainedTokens,
			ChunkCoverage:  compressed.ChunkCoverage,
			ItemCoverage:   compressed.ItemCoverage,
			FieldCoverage:  compressed.FieldCoverage,
		})
		log.InfofCtx(
			state.ctx,
			"[agent] run %s answer context tool result compacted phase=%s tool_call_id=%s tool=%s strategy=%s tokens=%d->%d source_tokens=%d chunk_coverage=%s item_coverage=%s field_coverage=%s",
			state.runID,
			phase,
			candidate.toolCallID,
			candidate.tool,
			compressed.Strategy,
			candidate.originalTokens,
			retainedTokens,
			compressed.OriginalTokens,
			compressed.ChunkCoverage,
			compressed.ItemCoverage,
			compressed.FieldCoverage,
		)
	}

	afterInputTokens, err := estimateInputTokens(messages, tools)
	if err != nil {
		return result, fmt.Errorf("measure compacted %s context: %w", phase, err)
	}
	result.ProjectedAfterTokens = afterInputTokens + outputReserve
	result.Applied = result.AssistantTokensRemoved > 0 || result.ToolResultsCompressed > 0
	if result.Applied {
		state.messages = messages
		emitAnswerCompacted(agent.observer, state.runID, phase)
		log.InfofCtx(
			state.ctx,
			"[agent] run %s final-answer context compacted phase=%s projected=%d->%d window=%d high=%d target=%d assistant_tokens_removed=%d tool_results_compressed=%d",
			state.runID,
			phase,
			result.ProjectedBeforeTokens,
			result.ProjectedAfterTokens,
			window,
			highWater,
			target,
			result.AssistantTokensRemoved,
			result.ToolResultsCompressed,
		)
	} else {
		log.WarnfCtx(
			state.ctx,
			"[agent] run %s final-answer context reached high water but no transient messages were compressible phase=%s projected=%d window=%d high=%d target=%d",
			state.runID,
			phase,
			result.ProjectedBeforeTokens,
			window,
			highWater,
			target,
		)
	}
	if result.ProjectedAfterTokens > target {
		log.WarnfCtx(
			state.ctx,
			"[agent] run %s final-answer context remains above target phase=%s projected=%d target=%d remaining_reduction=%d",
			state.runID,
			phase,
			result.ProjectedAfterTokens,
			target,
			result.ProjectedAfterTokens-target,
		)
	}
	return result, nil
}

func stateContext(state *compiledLoop) context.Context {
	if state == nil || state.ctx == nil {
		return context.Background()
	}
	return state.ctx
}

func emitAnswerCompacted(observer Observer, runID, phase string) {
	emitter, ok := observer.(interface {
		EmitPhase(string, string)
	})
	if ok {
		text := "上下文已压缩，正在继续处理问题"
		if phase == "forced_conclusion" {
			text = "上下文已压缩，正在基于保留证据生成答案"
		}
		emitter.EmitPhase(runID, text)
	}
}

func emitContextUsage(observer Observer, runID string, event run.ContextUsageEvent) {
	if emitter, ok := observer.(run.ContextUsageObserver); ok {
		emitter.OnContextUsage(context.Background(), runID, event)
	}
}

func toolResultCandidates(
	messages []llm.Message,
	start int,
	sources map[int]string,
) []toolResultCandidate {
	candidates := make([]toolResultCandidate, 0)
	for index := start; index < len(messages); index++ {
		message := messages[index]
		if message.Role != "tool" || message.Content == "" {
			continue
		}
		tokens := tooloutput.EstimateTokens(message.Content)
		if tokens <= emergencyToolResultFloor {
			continue
		}
		sourceContent := message.Content
		if original, exists := sources[index]; exists {
			sourceContent = original
		}
		candidates = append(candidates, toolResultCandidate{
			index: index, toolCallID: message.ToolCallID, tool: message.Name,
			sourceContent: sourceContent, originalTokens: tokens, budgetTokens: tokens,
		})
	}
	return candidates
}

func allocateToolBudgets(
	candidates []toolResultCandidate,
	needed int,
	oldFloor int,
	recentFloor int,
) int {
	for index := range candidates {
		if needed <= 0 {
			break
		}
		floor := oldFloor
		if index == len(candidates)-1 {
			floor = recentFloor
		}
		available := max(0, candidates[index].budgetTokens-floor)
		reduction := min(needed, available)
		candidates[index].budgetTokens -= reduction
		needed -= reduction
	}
	return needed
}

func plannedToolReduction(candidates []toolResultCandidate) int {
	reduction := 0
	for _, candidate := range candidates {
		reduction += candidate.originalTokens - candidate.budgetTokens
	}
	return reduction
}
