package execution

import (
	"fmt"
	"math"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const maxConclusionGenerations = 3 + maxAnswerContractRetries

// ModelUsageCeiling bounds every model call the immutable execution policy permits.
type ModelUsageCeiling struct {
	Calls        int64
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	CostMicros   int64
}

// UsageCeiling includes continuation and bounded answer-repair calls.
func UsageCeiling(
	budget agentapi.BudgetPolicy,
	model agentapi.ModelPolicy,
) (ModelUsageCeiling, error) {
	if budget.MaxSteps <= 0 || budget.ContextTokens <= 0 || budget.MaxContinueRounds < 0 {
		return ModelUsageCeiling{}, fmt.Errorf("model usage ceiling requires valid agent budgets")
	}
	if model.MaxOutputTokens <= 0 {
		return ModelUsageCeiling{}, fmt.Errorf("model usage ceiling requires positive output tokens")
	}
	if model.InputPriceMicrosPerMillionTokens < 0 ||
		model.OutputPriceMicrosPerMillionTokens < 0 {
		return ModelUsageCeiling{}, fmt.Errorf("model prices cannot be negative")
	}
	if (model.InputPriceMicrosPerMillionTokens == 0) !=
		(model.OutputPriceMicrosPerMillionTokens == 0) {
		return ModelUsageCeiling{}, fmt.Errorf("model prices must be configured together")
	}

	generationCalls, err := checkedAdd(
		int64(budget.MaxContinueRounds),
		1,
		"continuation call count",
	)
	if err != nil {
		return ModelUsageCeiling{}, err
	}
	conclusionCalls, err := checkedMultiply(
		generationCalls,
		maxConclusionGenerations,
		"forced conclusion call count",
	)
	if err != nil {
		return ModelUsageCeiling{}, err
	}
	calls, err := checkedAdd(int64(budget.MaxSteps), conclusionCalls, "model call count")
	if err != nil {
		return ModelUsageCeiling{}, err
	}
	inputTokens, err := checkedMultiply(calls, int64(budget.ContextTokens), "input token ceiling")
	if err != nil {
		return ModelUsageCeiling{}, err
	}
	outputTokens, err := checkedMultiply(calls, int64(model.MaxOutputTokens), "output token ceiling")
	if err != nil {
		return ModelUsageCeiling{}, err
	}
	totalTokens, err := checkedAdd(inputTokens, outputTokens, "total token ceiling")
	if err != nil {
		return ModelUsageCeiling{}, err
	}
	costMicros, err := modelCostCeiling(calls, budget.ContextTokens, model)
	if err != nil {
		return ModelUsageCeiling{}, err
	}
	return ModelUsageCeiling{
		Calls: calls, InputTokens: inputTokens, OutputTokens: outputTokens,
		TotalTokens: totalTokens, CostMicros: costMicros,
	}, nil
}

func modelCostCeiling(
	calls int64,
	contextTokens int,
	model agentapi.ModelPolicy,
) (int64, error) {
	if model.InputPriceMicrosPerMillionTokens == 0 {
		return 0, nil
	}
	inputCost, err := tokenCostCeiling(
		int64(contextTokens),
		model.InputPriceMicrosPerMillionTokens,
	)
	if err != nil {
		return 0, fmt.Errorf("input cost ceiling: %w", err)
	}
	outputCost, err := tokenCostCeiling(
		int64(model.MaxOutputTokens),
		model.OutputPriceMicrosPerMillionTokens,
	)
	if err != nil {
		return 0, fmt.Errorf("output cost ceiling: %w", err)
	}
	perCall, err := checkedAdd(inputCost, outputCost, "per-call cost ceiling")
	if err != nil {
		return 0, err
	}
	return checkedMultiply(calls, perCall, "model cost ceiling")
}

func tokenCostCeiling(tokens, priceMicrosPerMillionTokens int64) (int64, error) {
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

func checkedAdd(left, right int64, name string) (int64, error) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, fmt.Errorf("%s overflow", name)
	}
	return left + right, nil
}

func checkedMultiply(left, right int64, name string) (int64, error) {
	if left < 0 || right < 0 || left != 0 && right > math.MaxInt64/left {
		return 0, fmt.Errorf("%s overflow", name)
	}
	return left * right, nil
}
