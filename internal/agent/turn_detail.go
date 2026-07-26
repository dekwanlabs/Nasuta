package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/log"
)

const (
	turnDetailTokenLimit = 2400
	turnUserTokenLimit   = 350
	turnCallsTokenLimit  = 500
	turnResultTokenLimit = 900
	turnAnswerTokenLimit = 450
)

type archivedTurnDetail struct {
	Version            int                  `json:"version"`
	Turn               int                  `json:"turn"`
	User               archivedText         `json:"user"`
	ToolCalls          []archivedToolCall   `json:"toolCalls"`
	OmittedToolCalls   int                  `json:"omittedToolCalls,omitempty"`
	ToolResults        []archivedToolResult `json:"toolResults"`
	OmittedToolResults int                  `json:"omittedToolResults,omitempty"`
	Assistant          archivedText         `json:"assistant"`
}

type archivedText struct {
	Content                 string `json:"content"`
	Coverage                string `json:"coverage"`
	OriginalEstimatedTokens int    `json:"originalEstimatedTokens"`
	RetainedEstimatedTokens int    `json:"retainedEstimatedTokens"`
}

type archivedToolCall struct {
	Name                    string          `json:"name"`
	Arguments               json.RawMessage `json:"arguments"`
	Coverage                string          `json:"coverage"`
	OriginalEstimatedTokens int             `json:"originalEstimatedTokens"`
	RetainedEstimatedTokens int             `json:"retainedEstimatedTokens"`
}

type archivedToolResult struct {
	Name                    string          `json:"name"`
	Content                 json.RawMessage `json:"content"`
	Coverage                string          `json:"coverage"`
	OriginalEstimatedTokens int             `json:"originalEstimatedTokens"`
	RetainedEstimatedTokens int             `json:"retainedEstimatedTokens"`
}

type turnDetailBudgets struct {
	user, calls, results, answer int
	toolItems                    int
}

func compressTurnDetail(turnNumber int, messages []llm.Message) (json.RawMessage, error) {
	budgets := turnDetailBudgets{
		user: turnUserTokenLimit, calls: turnCallsTokenLimit,
		results: turnResultTokenLimit, answer: turnAnswerTokenLimit, toolItems: -1,
	}
	for {
		detail := buildArchivedTurnDetail(turnNumber, messages, budgets)
		raw, err := json.Marshal(detail)
		if err != nil {
			return nil, fmt.Errorf("marshal archived turn %d: %w", turnNumber, err)
		}
		tokens := tooloutput.EstimateTokens(string(raw))
		if tokens <= turnDetailTokenLimit {
			return raw, nil
		}
		next := shrinkTurnDetailBudgets(budgets)
		if next == budgets {
			itemCount := max(len(detail.ToolCalls)+detail.OmittedToolCalls,
				len(detail.ToolResults)+detail.OmittedToolResults)
			if budgets.toolItems != 0 && itemCount > 0 {
				budgets.toolItems = reducedItemLimit(budgets.toolItems, itemCount)
				continue
			}
			log.Errorf("Failed to compress turn %d: tokens=%d calls=%d results=%d",
				turnNumber, tokens, len(detail.ToolCalls), len(detail.ToolResults))
			return nil, fmt.Errorf("archived turn %d uses %d tokens after bounded compression (limit: %d)",
				turnNumber, tokens, turnDetailTokenLimit)
		}
		budgets = next
	}
}

func shrinkTurnDetailBudgets(budgets turnDetailBudgets) turnDetailBudgets {
	return turnDetailBudgets{
		user: shrinkBudget(budgets.user, 80), calls: shrinkBudget(budgets.calls, 80),
		results: shrinkBudget(budgets.results, 160), answer: shrinkBudget(budgets.answer, 100),
		toolItems: budgets.toolItems,
	}
}

func shrinkBudget(value, minimum int) int {
	if value <= 0 {
		return 0
	}
	return max(minimum, value*3/4)
}

func reducedItemLimit(current, total int) int {
	if current < 0 || current > total {
		current = total
	}
	next := current * 3 / 4
	if next == current {
		next--
	}
	return max(0, next)
}

func buildArchivedTurnDetail(turnNumber int, messages []llm.Message, budgets turnDetailBudgets) archivedTurnDetail {
	var users, answers strings.Builder
	toolCalls := make([]llm.ToolCall, 0)
	toolResults := make([]llm.Message, 0)
	for _, message := range messages {
		switch {
		case message.Role == "user":
			appendSectionText(&users, message.Content)
		case message.Role == "tool":
			toolResults = append(toolResults, message)
		case message.Role == "assistant" && len(message.ToolCalls) > 0:
			toolCalls = append(toolCalls, message.ToolCalls...)
			appendSectionText(&answers, message.Content)
		case message.Role == "assistant":
			appendSectionText(&answers, message.Content)
		}
	}
	toolCalls, omittedCalls := boundedEdges(toolCalls, budgets.toolItems)
	toolResults, omittedResults := boundedEdges(toolResults, budgets.toolItems)

	archivedCalls := make([]archivedToolCall, 0, len(toolCalls))
	callBudget := dividedBudget(budgets.calls, len(toolCalls))
	for _, call := range toolCalls {
		originalTokens := tooloutput.EstimateTokens(call.Function.Arguments)
		arguments := tooloutput.TruncateContent(call.Function.Arguments, callBudget)
		retainedTokens := tooloutput.EstimateTokens(arguments)
		coverage := "full"
		if retainedTokens < originalTokens {
			coverage = "partial"
		}
		archivedCalls = append(archivedCalls, archivedToolCall{
			Name: call.Function.Name, Arguments: canonicalJSONValue(arguments), Coverage: coverage,
			OriginalEstimatedTokens: originalTokens, RetainedEstimatedTokens: retainedTokens,
		})
	}

	archivedResults := make([]archivedToolResult, 0, len(toolResults))
	resultBudget := dividedBudget(budgets.results, len(toolResults))
	question := tooloutput.TruncateContent(users.String(), budgets.user)
	for _, result := range toolResults {
		compressed := tooloutput.Compress(tooloutput.Request{
			Question: question, Content: result.Content, MaxTokens: resultBudget,
		})
		content := compressed.Content
		if compressed.FallbackReason != "" {
			content = tooloutput.TruncateContent(result.Content, resultBudget)
		}
		coverage := compressed.ChunkCoverage
		if coverage == "" {
			coverage = "full"
		}
		archivedResults = append(archivedResults, archivedToolResult{
			Name: result.Name, Content: canonicalJSONValue(content),
			Coverage: coverage, OriginalEstimatedTokens: compressed.OriginalTokens,
			RetainedEstimatedTokens: tooloutput.EstimateTokens(content),
		})
	}

	return archivedTurnDetail{
		Version: 1, Turn: turnNumber,
		User:      boundedArchivedText(users.String(), budgets.user),
		ToolCalls: archivedCalls, OmittedToolCalls: omittedCalls,
		ToolResults: archivedResults, OmittedToolResults: omittedResults,
		Assistant: boundedArchivedText(answers.String(), budgets.answer),
	}
}

func boundedEdges[T any](values []T, limit int) ([]T, int) {
	if limit < 0 || len(values) <= limit {
		return values, 0
	}
	if limit == 0 {
		return nil, len(values)
	}
	head := (limit + 1) / 2
	tail := limit - head
	out := make([]T, 0, limit)
	out = append(out, values[:head]...)
	if tail > 0 {
		out = append(out, values[len(values)-tail:]...)
	}
	return out, len(values) - len(out)
}

func boundedArchivedText(value string, budget int) archivedText {
	originalTokens := tooloutput.EstimateTokens(value)
	content := tooloutput.TruncateContent(value, budget)
	retainedTokens := tooloutput.EstimateTokens(content)
	coverage := "full"
	if retainedTokens < originalTokens {
		coverage = "partial"
	}
	return archivedText{
		Content: content, Coverage: coverage,
		OriginalEstimatedTokens: originalTokens, RetainedEstimatedTokens: retainedTokens,
	}
}

func canonicalJSONValue(value string) json.RawMessage {
	trimmed := strings.TrimSpace(value)
	if json.Valid([]byte(trimmed)) {
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, []byte(trimmed)); err == nil {
			return json.RawMessage(compacted.String())
		}
	}
	raw, _ := json.Marshal(value)
	return raw
}

func dividedBudget(total, count int) int {
	if count <= 0 {
		return total
	}
	return total / count
}

func appendSectionText(dst *strings.Builder, value string) {
	if value == "" {
		return
	}
	if dst.Len() > 0 {
		dst.WriteByte('\n')
	}
	dst.WriteString(value)
}
