package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	exactAnswerContractPrefix = "[NASUTA_EXACT_ANSWER_CONTRACT] "
	maxAnswerContractRetries  = 2
)

var ErrAnswerContractViolation = errors.New("final answer violated an exact-output contract")

type exactAnswerContract struct {
	required []string
	seen     map[string]struct{}
}

func withoutAnswerContractMessages(messages []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role == "system" && strings.HasPrefix(message.Content, exactAnswerContractPrefix) {
			continue
		}
		out = append(out, message)
	}
	return out
}

func (contract *exactAnswerContract) Active() bool {
	return contract != nil && len(contract.required) > 0
}

func (contract *exactAnswerContract) Add(candidate tool.AnswerContract) {
	if contract == nil || len(candidate.RequiredLiterals) == 0 {
		return
	}
	if contract.seen == nil {
		contract.seen = make(map[string]struct{}, len(candidate.RequiredLiterals))
	}
	for _, literal := range candidate.RequiredLiterals {
		if literal == "" {
			continue
		}
		if _, exists := contract.seen[literal]; exists {
			continue
		}
		contract.seen[literal] = struct{}{}
		contract.required = append(contract.required, literal)
	}
}

func (contract *exactAnswerContract) Missing(answer string) []string {
	if !contract.Active() {
		return nil
	}
	missing := make([]string, 0)
	for _, literal := range contract.required {
		if !strings.Contains(answer, literal) {
			missing = append(missing, literal)
		}
	}
	return missing
}

func answerContractMessage(candidate tool.AnswerContract) (llm.Message, bool) {
	contract := &exactAnswerContract{}
	contract.Add(candidate)
	if !contract.Active() {
		return llm.Message{}, false
	}
	encoded, err := json.Marshal(tool.AnswerContract{RequiredLiterals: contract.required})
	if err != nil {
		return llm.Message{}, false
	}
	return llm.Message{
		Role: "system",
		Content: prompts.MustRender(prompts.AgentQAExactAnswerContract, struct {
			Prefix   string
			Contract string
		}{Prefix: exactAnswerContractPrefix, Contract: string(encoded)}),
	}, true
}

func answerContractRepairInstruction(missing []string) string {
	encoded, _ := json.Marshal(missing)
	return prompts.MustRender(prompts.AgentQAAnswerRepair, struct {
		Missing string
	}{Missing: string(encoded)})
}

func answerContractError(missing []string) error {
	return fmt.Errorf("%w: %d required values missing", ErrAnswerContractViolation, len(missing))
}

func (agent *Agent) validateOrRepairAnswer(ctx context.Context, messages []llm.Message, initial *llm.ChatStreamResult, contract *exactAnswerContract, maxTokens int, stream *StreamPipe) (*llm.ChatStreamResult, error) {
	if !contract.Active() || initial == nil {
		return initial, nil
	}
	candidate := initial
	missing := contract.Missing(candidate.Content)
	for attempt := 1; len(missing) > 0 && attempt <= maxAnswerContractRetries; attempt++ {
		log.WarnfCtx(ctx, "[agent] exact-answer validation rejected candidate: missing=%d retry=%d/%d", len(missing), attempt, maxAnswerContractRetries)
		repairMessages := append(append([]llm.Message{}, messages...),
			llm.Message{Role: "assistant", Content: candidate.Content},
			llm.Message{Role: "user", Content: answerContractRepairInstruction(missing)},
		)
		repaired, err := agent.generateWithContinue(ctx, repairMessages, maxTokens, stream)
		if err != nil {
			return repaired, fmt.Errorf("%w: retry %d failed: %v", ErrAnswerContractViolation, attempt, err)
		}
		candidate = repaired
		missing = contract.Missing(candidate.Content)
	}
	if len(missing) > 0 {
		log.ErrorfCtx(ctx, "[agent] exact-answer validation failed after %d retries: missing=%d", maxAnswerContractRetries, len(missing))
		return candidate, answerContractError(missing)
	}
	return candidate, nil
}
