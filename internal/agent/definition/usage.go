package definition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

func (recorder *definitionUsageRecorder) RecordLLMCall(
	ctx context.Context,
	call llm.CallUsage,
) error {
	inputCost, err := tokenCostMicros(
		int64(call.Usage.InputTokens),
		recorder.inputPriceMicrosPerMillionTokens,
	)
	if err != nil {
		return fmt.Errorf("calculate input token cost: %w", err)
	}
	outputCost, err := tokenCostMicros(
		int64(call.Usage.OutputTokens),
		recorder.outputPriceMicrosPerMillionTokens,
	)
	if err != nil {
		return fmt.Errorf("calculate output token cost: %w", err)
	}
	if inputCost > math.MaxInt64-outputCost {
		return fmt.Errorf("calculate model cost: overflow")
	}
	callCost := inputCost + outputCost
	recorder.mu.Lock()
	if recorder.usage.CostMicros > math.MaxInt64-callCost {
		recorder.mu.Unlock()
		return fmt.Errorf("accumulate model cost: overflow")
	}
	recorder.usage.InputTokens += int64(call.Usage.InputTokens)
	recorder.usage.OutputTokens += int64(call.Usage.OutputTokens)
	recorder.usage.ReasoningTokens += int64(call.Usage.ReasoningTokens)
	recorder.usage.TotalTokens += int64(call.Usage.TotalTokens)
	recorder.usage.CostMicros += callCost
	recorder.mu.Unlock()
	if recorder.store != nil {
		return recorder.store.RecordLLMCall(ctx, call)
	}
	return nil
}

func tokenCostMicros(tokens, priceMicrosPerMillionTokens int64) (int64, error) {
	if tokens < 0 || priceMicrosPerMillionTokens < 0 {
		return 0, fmt.Errorf("tokens and price cannot be negative")
	}
	if tokens == 0 || priceMicrosPerMillionTokens == 0 {
		return 0, nil
	}
	if tokens > math.MaxInt64/priceMicrosPerMillionTokens {
		return 0, fmt.Errorf("token price multiplication overflow")
	}
	product := tokens * priceMicrosPerMillionTokens
	cost := product / 1_000_000
	if product%1_000_000 != 0 {
		cost++
	}
	return cost, nil
}

func (recorder *definitionUsageRecorder) Usage() agentapi.Usage {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.usage
}

func hashString(value string) string {
	return hashBytes([]byte(value))
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func validHex(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil
}
