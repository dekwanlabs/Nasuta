package execution

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

func (agent *Agent) buildAgentMessages(question string, conversation ConversationContext, rc *retrieval.RetrievedContext, plan domain.EvidencePlan) []llm.Message {
	return BuildAgentMessages(question, conversation, rc, plan, agent.cfg.DomainKnowledge, agent.cfg.HistoryLimit)
}

// BuildAgentMessages compiles the request-scoped prompt and replayable history.
func BuildAgentMessages(question string, conversation ConversationContext, rc *retrieval.RetrievedContext, plan domain.EvidencePlan, domainKnowledge string, historyLimit int) []llm.Message {
	mode := ClassifyResponseMode(question)
	hint := "\n\n---\n" + prompts.MustRender(prompts.AgentQAResponseMode, struct {
		Mode string
	}{Mode: string(mode)})
	sysPrompt := composeSystemPrompt(agentPromptForPlan(plan), conversation.RolePrompt) + hint
	if dk := strings.TrimSpace(domainKnowledge); dk != "" && plan.Has(domain.Internal) {
		sysPrompt += "\n\n## Domain Knowledge\n" + dk
	}
	msgs := []llm.Message{{Role: "system", Content: sysPrompt}}
	msgs = append(msgs, llm.Message{Role: "system", Content: evidencePlanInstruction(plan)})
	msgs = append(msgs, conversation.Instructions...)

	if len(conversation.RecentDialogue) > 0 {
		if dialogue, err := json.Marshal(conversation.RecentDialogue); err == nil {
			msgs = append(msgs, llm.Message{
				Role: "system",
				Content: prompts.MustRender(prompts.AgentQARecentDialogue, struct {
					Dialogue string
				}{Dialogue: string(dialogue)}),
			})
		}
	}
	if conversation.RetrievedHistory != "" {
		msgs = append(msgs, llm.Message{
			Role: "system",
			Content: prompts.MustRender(prompts.AgentQARetrievedHistory, struct {
				History string
			}{History: conversation.RetrievedHistory}),
		})
	}
	if conversation.HistoricalContext != "" {
		msgs = append(msgs, llm.Message{
			Role: "system",
			Content: prompts.MustRender(prompts.AgentQAHistoricalContext, struct {
				Context string
			}{Context: conversation.HistoricalContext}),
		})
	}
	recent := withoutAnswerContractMessages(conversation.Recent)
	msgs = append(msgs, ReplayableTailMessages(recent, historyLimit)...)

	if rc != nil && rc.Text != "" {
		msgs = append(msgs, llm.Message{
			Role: "system",
			Content: prompts.MustRender(prompts.AgentQAPreRetrievedEvidence, struct {
				HitCount int
				Evidence string
			}{HitCount: rc.HitCount, Evidence: rc.Text}),
		})
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: question})
	return msgs
}

func agentPromptForPlan(plan domain.EvidencePlan) string {
	if plan.Direct() || plan.Sources == domain.Memory {
		return directAgentSystemPrompt
	}
	if plan.Sources == domain.Web {
		return webAgentSystemPrompt
	}
	return agentSystemPrompt
}

func evidencePlanInstruction(plan domain.EvidencePlan) string {
	return prompts.MustRender(prompts.AgentQAEvidencePlan, struct {
		Direct bool
		Plan   string
	}{Direct: plan.Direct(), Plan: plan.String()})
}

var directAgentSystemPrompt = withUserVisibleAnswer(promptWithRolePlaceholder(prompts.AgentQADirect))

// replayableTailMessages keeps only provider-valid tool call/result groups.
func ReplayableTailMessages(msgs []llm.Message, n int) []llm.Message {
	start := 0
	if n > 0 && len(msgs) > n {
		start = len(msgs) - n
		if msgs[start].Role == "tool" {
			for start > 0 && msgs[start-1].Role == "tool" {
				start--
			}
			if start > 0 && len(msgs[start-1].ToolCalls) > 0 {
				start--
			}
		}
		if start > 0 && len(msgs[start].ToolCalls) > 0 && msgs[start-1].Role == "user" {
			start--
		}
	}
	tail := msgs[start:]
	out := make([]llm.Message, 0, len(tail))
	for i := 0; i < len(tail); {
		message := tail[i]
		if message.Role == "tool" {
			i++
			continue
		}
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			out = append(out, message)
			i++
			continue
		}
		j := i + 1
		for j < len(tail) && tail[j].Role == "tool" {
			j++
		}
		if completeToolResultGroup(message.ToolCalls, tail[i+1:j]) {
			out = append(out, tail[i:j]...)
		}
		i = j
	}
	return out
}

func completeToolResultGroup(calls []llm.ToolCall, results []llm.Message) bool {
	if len(calls) != len(results) {
		return false
	}
	expected := make(map[string]string, len(calls))
	for _, call := range calls {
		if call.ID == "" || call.Function.Name == "" {
			return false
		}
		expected[call.ID] = call.Function.Name
	}
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		name, ok := expected[result.ToolCallID]
		if !ok || name != result.Name {
			return false
		}
		if _, duplicate := seen[result.ToolCallID]; duplicate {
			return false
		}
		seen[result.ToolCallID] = struct{}{}
	}
	return len(seen) == len(expected)
}

// contextChars sums visible content length for context-size logging.
func contextChars(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += len([]rune(m.Content))
	}
	return total
}

func messageChars(msgs []llm.Message) int {
	total := 0
	for _, message := range msgs {
		total += len([]rune(message.Content))
	}
	return total
}

func estimateMessagesTokens(messages []llm.Message) int {
	total := 0
	for _, message := range messages {
		total += tooloutput.EstimateTokens(message.Content) + 4
		for _, call := range message.ToolCalls {
			total += tooloutput.EstimateTokens(call.Function.Name) +
				tooloutput.EstimateTokens(call.Function.Arguments) + 4
		}
	}
	return total
}

func estimateInputTokens(messages []llm.Message, tools []llm.ToolDef) (int, error) {
	inputTokens := estimateMessagesTokens(messages)
	if len(tools) == 0 {
		return inputTokens, nil
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return 0, fmt.Errorf("encode tool definitions: %w", err)
	}
	return inputTokens + tooloutput.EstimateTokens(string(encoded)), nil
}

func (agent *Agent) outputTokenReserve() int {
	return max(agent.cfg.AnswerMaxTokens, agent.cfg.ConclusionMaxTokens)
}

func contextSafetyTokens(window int) int {
	return max(window/20, 1024)
}

func (agent *Agent) ensureInputBudget(messages []llm.Message, tools []llm.ToolDef) error {
	window := agent.cfg.ContextWindow
	if window <= 0 {
		return nil
	}
	inputTokens, err := estimateInputTokens(messages, tools)
	if err != nil {
		return fmt.Errorf("estimate context budget: %w", err)
	}
	outputReserve := agent.outputTokenReserve()
	safety := contextSafetyTokens(window)
	if inputTokens+outputReserve+safety > window {
		return fmt.Errorf(
			"QA context exceeds configured window before provider call: input=%d output_reserve=%d safety=%d window=%d; shorten the question or attachments",
			inputTokens, outputReserve, safety, window,
		)
	}
	return nil
}

var (
	agentToolPrompt      = prompts.Text(prompts.AgentQAToolPolicy)
	agentSystemPrompt    = systemPrompt + "\n\n" + agentToolPrompt
	webAgentSystemPrompt = withUserVisibleAnswer(promptWithRolePlaceholder(prompts.AgentQAWeb))
)
