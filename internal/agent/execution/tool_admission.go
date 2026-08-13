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
	toolAdmissionDenyBudget       toolAdmissionAction = "deny_budget"
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
	remaining := agent.remainingToolTokens(state)
	declared := declaredToolTokens(candidate, args)
	input := toolAdmissionInput{
		Tool: call.Function.Name, Scope: scope,
		RemainingTokens: remaining, DeclaredTokens: declared,
	}
	decision, _ := runtrace.Invoke(state.ctx, toolAdmissionSpec, input, func(
		_ context.Context,
		input toolAdmissionInput,
	) (toolAdmissionDecision, error) {
		if keys, covered := state.evidenceLedger.fullyCovers(scope); covered {
			return toolAdmissionDecision{
				Action: toolAdmissionAlreadyAvailable, Reason: "scope_fully_covered",
				Scope: scope, Arguments: args, RemainingTokens: remaining,
				DeclaredTokens: declared, EvidenceKeys: keys,
			}, nil
		}
		if remaining < 0 || declared <= remaining {
			return toolAdmissionDecision{
				Action: toolAdmissionAllow, Reason: "within_budget",
				Scope: scope, Arguments: args, RemainingTokens: remaining,
				DeclaredTokens: declared,
			}, nil
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
					}, nil
				}
			}
		}
		return toolAdmissionDecision{
			Action: toolAdmissionDenyBudget, Reason: "declared_result_exceeds_budget",
			Scope: scope, Arguments: args, RemainingTokens: remaining,
			DeclaredTokens: declared,
		}, nil
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
	const conservativeDefault = 4096
	if candidate.Admission == nil || candidate.Admission.MaxResultTokens == nil {
		return conservativeDefault
	}
	return max(1, candidate.Admission.MaxResultTokens(args))
}

func (agent *Agent) remainingToolTokens(state *compiledLoop) int {
	if agent.cfg.ContextWindow <= 0 {
		return -1
	}
	inputTokens, err := estimateInputTokens(state.messages, state.tools)
	if err != nil {
		return 0
	}
	contextRemaining := max(
		0,
		agent.cfg.ContextWindow-inputTokens-agent.outputTokenReserve()-contextSafetyTokens(agent.cfg.ContextWindow),
	)
	if state.remainingToolTokens < 0 {
		return contextRemaining
	}
	return min(contextRemaining, state.remainingToolTokens)
}

func initialToolTokenBudget(agent *Agent, messages []llm.Message, tools []llm.ToolDef) int {
	if agent.cfg.ContextWindow <= 0 {
		return -1
	}
	inputTokens, err := estimateInputTokens(messages, tools)
	if err != nil {
		return 0
	}
	return max(
		0,
		agent.cfg.ContextWindow-inputTokens-agent.outputTokenReserve()-contextSafetyTokens(agent.cfg.ContextWindow),
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
