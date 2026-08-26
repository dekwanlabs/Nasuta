package execution

import (
	"context"
	"errors"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/log"
)

// ErrModelCallBudgetExhausted means no output budget remains for another physical call.
// Callers may use a deterministic or evidence-preserving recovery path instead.
var ErrModelCallBudgetExhausted = errors.New("model call budget exhausted")

func (agent *Agent) callModel(
	ctx context.Context,
	messages []llm.Message,
	tools []llm.ToolDef,
	h llm.StreamHandler,
	maxTokens int,
) (*llm.ChatStreamResult, error) {
	if agent.cfg.BudgetCheck != nil {
		if err := agent.cfg.BudgetCheck(); err != nil {
			return nil, err
		}
	} else if gate := agentapi.RunBudgetGateFromContext(ctx); gate != nil {
		if err := gate.Check(); err != nil {
			return nil, err
		}
	}
	inputTokens, err := estimateInputTokens(messages, tools)
	if err != nil {
		return nil, fmt.Errorf("estimate model call input: %w", err)
	}
	var callReservation agentapi.RunBudgetCallReservation
	if gate := agentapi.RunBudgetUsageGateFromContext(ctx); gate != nil {
		effectiveMaxTokens, limitErr := agent.limitModelOutput(inputTokens, maxTokens, gate)
		if limitErr != nil {
			return nil, limitErr
		}
		if effectiveMaxTokens != maxTokens {
			log.WarnfCtx(ctx, "[agent] shrinking model output budget requested=%d effective=%d input_tokens=%d",
				maxTokens, effectiveMaxTokens, inputTokens)
		}
		maxTokens = effectiveMaxTokens
		estimate, estimateErr := agent.modelCallEstimate(inputTokens, maxTokens)
		if estimateErr != nil {
			return nil, fmt.Errorf("estimate model call budget: %w", estimateErr)
		}
		callReservation, err = gate.ReserveCall(estimate)
		if err != nil {
			return nil, fmt.Errorf("reserve model call budget: %w", err)
		}
	}
	result, callErr := agent.llm.ChatWithToolsMaxWithParameters(
		ctx, messages, tools, h, maxTokens, agent.cfg.ModelParameters,
	)
	if callReservation == nil {
		return result, callErr
	}
	var accountingErr error
	if result != nil && hasReportedUsage(result.Usage) {
		actual, usageErr := agent.usageForLedger(result.Usage)
		if usageErr != nil {
			accountingErr = errors.Join(usageErr, callReservation.Release())
		} else {
			accountingErr = callReservation.Settle(actual)
		}
	} else {
		accountingErr = callReservation.Release()
	}
	if accountingErr != nil {
		accountingErr = fmt.Errorf("account model call usage: %w", accountingErr)
	}
	if callErr != nil || accountingErr != nil {
		return result, errors.Join(callErr, accountingErr)
	}
	return result, nil
}

func (agent *Agent) limitModelOutput(inputTokens, requested int, gate agentapi.RunBudgetUsageGate) (int, error) {
	if requested <= 0 {
		return requested, nil
	}
	availability, ok := gate.(agentapi.RunBudgetAvailability)
	if !ok {
		return requested, nil
	}
	available := availability.Available()
	input := int64(inputTokens)
	if input > available.InputTokens {
		return 0, fmt.Errorf("model call budget exceeded: input_tokens requested=%d available=%d", input, available.InputTokens)
	}
	if available.OutputTokens <= 0 {
		return 0, fmt.Errorf("%w: output_tokens requested=%d available=%d", ErrModelCallBudgetExhausted, requested, available.OutputTokens)
	}
	maxOutput := minInt(requested, saturatingInt(available.OutputTokens))
	if available.TotalTokens > 0 {
		remaining := available.TotalTokens - input
		if remaining <= 0 {
			return 0, fmt.Errorf("model call budget exceeded: total_tokens input=%d available=%d", input, available.TotalTokens)
		}
		maxOutput = minInt(maxOutput, saturatingInt(remaining))
	}
	if available.CostMicros > 0 {
		var costErr error
		maxOutput, costErr = agent.capOutputByCost(inputTokens, maxOutput, available.CostMicros)
		if costErr != nil {
			return 0, costErr
		}
	}
	if maxOutput <= 0 {
		return 0, fmt.Errorf("model call budget exceeded: no output_tokens remain")
	}
	return maxOutput, nil
}

func (agent *Agent) capOutputByCost(inputTokens, maxOutput int, availableCost int64) (int, error) {
	base, err := agent.modelCallEstimate(inputTokens, 0)
	if err != nil {
		return 0, fmt.Errorf("estimate model call cost: %w", err)
	}
	if base.CostMicros > availableCost {
		return 0, fmt.Errorf("model call budget exceeded: cost_micros input=%d available=%d", base.CostMicros, availableCost)
	}
	low, high := 0, maxOutput
	for low < high {
		mid := low + (high-low+1)/2
		estimate, estimateErr := agent.modelCallEstimate(inputTokens, mid)
		if estimateErr != nil {
			return 0, fmt.Errorf("estimate model call cost: %w", estimateErr)
		}
		if estimate.CostMicros <= availableCost {
			low = mid
		} else {
			high = mid - 1
		}
	}
	if low == 0 && maxOutput > 0 && base.CostMicros == availableCost {
		return 0, fmt.Errorf("model call budget exceeded: no output cost budget remains")
	}
	return low, nil
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func saturatingInt(value int64) int {
	maxInt := int64(^uint(0) >> 1)
	if value > maxInt {
		return int(maxInt)
	}
	return int(value)
}

func (agent *Agent) modelCallEstimate(inputTokens, outputTokens int) (agentapi.Usage, error) {
	input := int64(inputTokens)
	output := int64(outputTokens)
	if output < 0 {
		output = 0
	}
	total, err := checkedAdd(input, output, "model call token estimate")
	if err != nil {
		return agentapi.Usage{}, err
	}
	cost, err := modelUsageCost(input, output, agent.cfg.InputPriceMicrosPerMillionTokens, agent.cfg.OutputPriceMicrosPerMillionTokens)
	if err != nil {
		return agentapi.Usage{}, err
	}
	return agentapi.Usage{
		InputTokens: input, OutputTokens: output, TotalTokens: total, CostMicros: cost,
	}, nil
}

func (agent *Agent) usageForLedger(usage llm.Usage) (agentapi.Usage, error) {
	totalTokens := int64(usage.TotalTokens)
	if totalTokens == 0 {
		var err error
		totalTokens, err = checkedAdd(int64(usage.InputTokens), int64(usage.OutputTokens), "reported model call tokens")
		if err != nil {
			return agentapi.Usage{}, err
		}
	}
	cost, err := modelUsageCost(
		int64(usage.InputTokens), int64(usage.OutputTokens),
		agent.cfg.InputPriceMicrosPerMillionTokens, agent.cfg.OutputPriceMicrosPerMillionTokens,
	)
	if err != nil {
		return agentapi.Usage{}, err
	}
	return agentapi.Usage{
		InputTokens: int64(usage.InputTokens), OutputTokens: int64(usage.OutputTokens),
		ReasoningTokens: int64(usage.ReasoningTokens), TotalTokens: totalTokens, CostMicros: cost,
	}, nil
}

func modelUsageCost(inputTokens, outputTokens, inputPrice, outputPrice int64) (int64, error) {
	inputCost, err := tokenCostCeiling(inputTokens, inputPrice)
	if err != nil {
		return 0, fmt.Errorf("input token cost: %w", err)
	}
	outputCost, err := tokenCostCeiling(outputTokens, outputPrice)
	if err != nil {
		return 0, fmt.Errorf("output token cost: %w", err)
	}
	return checkedAdd(inputCost, outputCost, "model call cost")
}

func hasReportedUsage(usage llm.Usage) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 ||
		usage.ReasoningTokens > 0 || usage.TotalTokens > 0
}
