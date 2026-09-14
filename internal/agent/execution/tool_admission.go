package execution

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

type toolAdmissionAction string

const (
	toolAdmissionAllow            toolAdmissionAction = "allow"
	toolAdmissionNarrow           toolAdmissionAction = "narrow"
	toolAdmissionAlreadyAvailable toolAdmissionAction = "already_available"
)

type toolAdmissionInput struct {
	Tool            string
	Scope           tool.EvidenceScope
	RemainingTokens int
	DeclaredTokens  int
}

type toolAdmissionDecision struct {
	Action          toolAdmissionAction
	Reason          string
	Scope           tool.EvidenceScope
	Arguments       tool.Arguments
	RemainingTokens int
	DeclaredTokens  int
	EvidenceKeys    []evidence.Key
}

var toolAdmissionSpec = runtrace.Spec[toolAdmissionInput, toolAdmissionDecision]{
	Operation: "agent.tool_admission",
	Node:      "tool_admission",
	Input: func(input toolAdmissionInput) map[string]any {
		return map[string]any{
			"tool": input.Tool, "scope": input.Scope,
			"remaining_tool_tokens": input.RemainingTokens,
			"declared_max_tokens":   input.DeclaredTokens,
		}
	},
	Output: func(_ toolAdmissionInput, output toolAdmissionDecision, _ error) map[string]any {
		return map[string]any{
			"action": output.Action, "reason": output.Reason,
			"scope": output.Scope, "remaining_tool_tokens": output.RemainingTokens,
			"declared_max_tokens": output.DeclaredTokens,
			"evidence_keys":       len(output.EvidenceKeys),
		}
	},
}

func admitToolCallDecision(
	state *compiledLoop,
	candidate tool.Tool,
	scope tool.EvidenceScope,
	args tool.Arguments,
	remaining, declared int,
) toolAdmissionDecision {
	if keys, covered := state.evidenceLedger.fullyCovers(scope); covered {
		return toolAdmissionDecision{
			Action: toolAdmissionAlreadyAvailable, Reason: "scope_fully_covered",
			Scope: scope, Arguments: args, RemainingTokens: remaining,
			DeclaredTokens: declared, EvidenceKeys: keys,
		}
	}
	if remaining < 0 || declared <= remaining {
		return toolAdmissionDecision{
			Action: toolAdmissionAllow, Reason: "within_budget",
			Scope: scope, Arguments: args, RemainingTokens: remaining,
			DeclaredTokens: declared,
		}
	}
	if candidate.Admission != nil && candidate.Admission.Narrow != nil {
		narrowed, changed := candidate.Admission.Narrow(args, remaining)
		if changed {
			narrowedTokens := declaredToolTokens(candidate, narrowed)
			if narrowedTokens <= remaining {
				return toolAdmissionDecision{
					Action: toolAdmissionNarrow, Reason: "reduced_to_budget",
					Scope: scope, Arguments: narrowed, RemainingTokens: remaining,
					DeclaredTokens: narrowedTokens,
				}
			}
		}
	}
	// The tool always runs; an oversized result is truncated to the remaining
	// budget after execution instead of being refused up front.
	return toolAdmissionDecision{
		Action: toolAdmissionAllow, Reason: "over_budget_will_truncate",
		Scope: scope, Arguments: args, RemainingTokens: remaining,
		DeclaredTokens: declared,
	}
}

func (agent *Agent) admitToolCall(state *compiledLoop, call llm.ToolCall) (llm.ToolCall, toolAdmissionDecision) {
	args, err := parseArgs(state.ctx, call.Function.Arguments)
	if err != nil {
		return call, toolAdmissionDecision{Action: toolAdmissionAllow, Reason: "executor_validation"}
	}
	candidate, ok := state.toolSnapshot.Get(tool.ToolID(call.Function.Name))
	if !ok {
		return call, toolAdmissionDecision{Action: toolAdmissionAllow, Reason: "executor_validation"}
	}
	scope := tool.EvidenceScope{}
	if candidate.Admission != nil && candidate.Admission.ResolveScope != nil {
		resolved, resolveErr := candidate.Admission.ResolveScope(args)
		if resolveErr == nil {
			scope = resolved
		}
	}
	remaining := agent.availableToolTokens(state)
	declared := declaredToolTokens(candidate, args)
	input := toolAdmissionInput{
		Tool: call.Function.Name, Scope: scope,
		RemainingTokens: remaining, DeclaredTokens: declared,
	}
	decision, _ := runtrace.Invoke(state.ctx, toolAdmissionSpec, input, func(
		_ context.Context,
		input toolAdmissionInput,
	) (toolAdmissionDecision, error) {
		return admitToolCallDecision(state, candidate, scope, args, remaining, declared), nil
	})
	if decision.Action == toolAdmissionNarrow {
		encoded, marshalErr := json.Marshal(decision.Arguments)
		if marshalErr == nil {
			call.Function.Arguments = string(encoded)
		}
	}
	return call, decision
}

func declaredToolTokens(candidate tool.Tool, args tool.Arguments) int {
	// Read tools without an Admission spec declare a fixed result size. It feeds
	// the narrow decision and the trace only; admission now truncates oversized
	// results after execution rather than refusing them up front.
	const conservativeDefault = 1024
	if candidate.Admission == nil || candidate.Admission.MaxResultTokens == nil {
		return conservativeDefault
	}
	return max(1, candidate.Admission.MaxResultTokens(args))
}

func (agent *Agent) availableToolTokens(state *compiledLoop) int {
	window := agent.effectiveContextWindow()
	if window <= 0 {
		return -1
	}
	inputTokens, err := estimateInputTokens(state.messages, state.tools)
	if err != nil {
		return 0
	}
	contextRemaining := max(
		0,
		window-inputTokens-agent.outputReserve()-contextSafetyTokens(window),
	)
	if state.remainingToolTokens < 0 {
		return contextRemaining
	}
	return min(contextRemaining, state.remainingToolTokens)
}

func initialToolTokenBudget(agent *Agent, messages []llm.Message, tools []llm.ToolDef) int {
	window := agent.effectiveContextWindow()
	if window <= 0 {
		return -1
	}
	inputTokens, err := estimateInputTokens(messages, tools)
	if err != nil {
		return 0
	}
	return max(
		0,
		window-inputTokens-agent.outputReserve()-contextSafetyTokens(window),
	)
}

func toolAdmissionExecution(decision toolAdmissionDecision) ToolExecution {
	keys := make([]map[string]string, 0, len(decision.EvidenceKeys))
	for _, key := range decision.EvidenceKeys {
		keys = append(keys, map[string]string{
			"sourceKind": key.SourceKind, "target": key.Target, "section": key.Section,
			"version": key.Version, "timeRange": key.TimeRange,
		})
	}
	payload := map[string]any{
		"action": decision.Action, "reason": decision.Reason,
		"scope": decision.Scope, "evidence": keys,
		"remainingToolTokens": decision.RemainingTokens,
		"declaredMaxTokens":   decision.DeclaredTokens,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		encoded = []byte(fmt.Sprintf(`{"action":%q,"reason":%q}`, decision.Action, decision.Reason))
	}
	return ToolExecution{AuthoritativeContent: string(encoded), PromptContent: string(encoded)}
}

func consumeToolTokens(state *compiledLoop, content string) {
	if state.remainingToolTokens < 0 {
		return
	}
	state.remainingToolTokens = max(0, state.remainingToolTokens-tooloutput.EstimateTokens(content))
}

// toolBudgetExhaustedPlaceholder is emitted in place of a tool result when the
// remaining tool-token budget cannot retain any content. It keeps the message
// non-empty so the model still sees the tool ran and its output was dropped.
const toolBudgetExhaustedPlaceholder = "[tool result omitted: tool-token budget exhausted]"

// boundToolResultToBudget truncates the model-facing copy of a tool result to
// the remaining tool-token budget. The authoritative content is untouched and
// stays available to the trace and evidence paths; only what the model sees is
// reduced.
func boundToolResultToBudget(state *compiledLoop, content string) string {
	if state.remainingToolTokens < 0 ||
		tooloutput.EstimateTokens(content) <= state.remainingToolTokens {
		return content
	}
	if state.remainingToolTokens <= 0 {
		return toolBudgetExhaustedPlaceholder
	}
	compressed := tooloutput.Compress(tooloutput.Request{
		Question:  state.input.Question,
		Content:   content,
		MaxTokens: state.remainingToolTokens,
	}).Content
	if compressed == "" {
		return toolBudgetExhaustedPlaceholder
	}
	return compressed
}
