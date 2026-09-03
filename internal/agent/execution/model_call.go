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
	if err := agent.checkModelCallBudget(ctx); err != nil {
		return nil, err
	}
	inputTokens, err := estimateInputTokens(messages, tools)
	if err != nil {
		return nil, fmt.Errorf("estimate model call input: %w", err)
	}
	maxTokens, callReservation, err := agent.reserveModelCallBudget(ctx, inputTokens, maxTokens)
	if err != nil {
		return nil, err
	}
	result, callErr := agent.llm.ChatWithToolsMaxWithParameters(
		ctx, messages, tools, h, maxTokens, agent.cfg.ModelParameters,
	)
	if callReservation == nil {
		return result, callErr
	}
	accountingErr := agent.accountModelCallUsage(result, callReservation)
	if accountingErr != nil {
		accountingErr = fmt.Errorf("account model call usage: %w", accountingErr)
	}
	if callErr != nil || accountingErr != nil {
		return result, errors.Join(callErr, accountingErr)
	}
	return result, nil
}

func (agent *Agent) checkModelCallBudget(ctx context.Context) error {
	if agent.cfg.BudgetCheck != nil {
		return agent.cfg.BudgetCheck()
	}
	if gate := agentapi.RunBudgetGateFromContext(ctx); gate != nil {
		return gate.Check()
	}
	return nil
}

func (agent *Agent) reserveModelCallBudget(ctx context.Context, inputTokens, maxTokens int) (int, agentapi.RunBudgetCallReservation, error) {
	gate := agentapi.RunBudgetUsageGateFromContext(ctx)
	if gate == nil {
		return maxTokens, nil, nil
	}
	phase := agentapi.RunBudgetPhaseFromContext(ctx)
	if llm.UsagePhaseFromContext(ctx) == llm.PhaseForcedConclusion {
		phase = agentapi.RunBudgetPhaseAnswer
	}
	effectiveMaxTokens, limitErr := agent.limitModelOutputForPhase(inputTokens, maxTokens, gate, phase)
	if limitErr != nil {
		return maxTokens, nil, limitErr
	}
	if effectiveMaxTokens != maxTokens {
		log.WarnfCtx(ctx, "[agent] shrinking model output budget requested=%d effective=%d input_tokens=%d",
			maxTokens, effectiveMaxTokens, inputTokens)
	}
	maxTokens = effectiveMaxTokens
	estimate, estimateErr := agent.modelCallEstimate(inputTokens, maxTokens)
	if estimateErr != nil {
		return maxTokens, nil, fmt.Errorf("estimate model call budget: %w", estimateErr)
	}
	var callReservation agentapi.RunBudgetCallReservation
	var err error
	if phased, ok := gate.(agentapi.RunBudgetPhasedGate); ok {
		callReservation, err = phased.ReserveCallForPhase(estimate, phase)
	} else {
		callReservation, err = gate.ReserveCall(estimate)
	}
	if err != nil {
		return maxTokens, nil, fmt.Errorf("reserve model call budget: %w", err)
	}
	return maxTokens, callReservation, nil
}

func (agent *Agent) accountModelCallUsage(result *llm.ChatStreamResult, callReservation agentapi.RunBudgetCallReservation) error {
	if result != nil && hasReportedUsage(result.Usage) {
		actual, usageErr := agent.usageForLedger(result.Usage)
		if usageErr != nil {
			return errors.Join(usageErr, callReservation.Release())
		}
		accountingErr := callReservation.Settle(actual)
		if accountingErr != nil {
			// A provider can report more usage than the admission estimate,
			// or the durable accounting write can fail before commit. Release
			// is idempotent after a committed settlement and prevents an
			// uncommitted reservation from leaking on the error path.
			return errors.Join(accountingErr, callReservation.Release())
		}
		return nil
	}
	return callReservation.Release()
}

func (agent *Agent) limitModelOutput(inputTokens, requested int, gate agentapi.RunBudgetUsageGate) (int, error) {
	return agent.limitModelOutputForPhase(inputTokens, requested, gate, agentapi.RunBudgetPhaseDefault)
}

func (agent *Agent) limitModelOutputForPhase(
	inputTokens, requested int,
	gate agentapi.RunBudgetUsageGate,
	phase agentapi.RunBudgetPhase,
) (int, error) {
	if requested <= 0 {
		return requested, nil
	}
	availability, ok := gate.(agentapi.RunBudgetAvailability)
	if !ok {
		return requested, nil
	}
	available := modelOutputAvailability(gate, phase, availability)
	input := int64(inputTokens)
	minimumOutput := modelOutputMinimum(gate)
	if input > available.InputTokens {
		return 0, fmt.Errorf("%w: input_tokens requested=%d available=%d", agentapi.ErrBudgetExceeded, input, available.InputTokens)
	}
	if available.OutputTokens <= 0 {
		if minimumOutput > 0 {
			return 0, fmt.Errorf("%w: protected output minimum=%d available=%d", agentapi.ErrBudgetExceeded, minimumOutput, available.OutputTokens)
		}
		return 0, fmt.Errorf("%w: output_tokens requested=%d available=%d", ErrModelCallBudgetExhausted, requested, available.OutputTokens)
	}
	maxOutput := minInt(requested, saturatingInt(available.OutputTokens))
	if available.TotalTokens > 0 {
		remaining := available.TotalTokens - input
		if remaining <= 0 {
			return 0, fmt.Errorf("%w: total_tokens input=%d available=%d", agentapi.ErrBudgetExceeded, input, available.TotalTokens)
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
		return 0, fmt.Errorf("%w: no output_tokens remain", agentapi.ErrBudgetExceeded)
	}
	if minimumOutput > 0 && int64(maxOutput) < minimumOutput {
		return 0, fmt.Errorf("%w: protected output minimum=%d effective=%d", agentapi.ErrBudgetExceeded, minimumOutput, maxOutput)
	}
	return maxOutput, nil
}

func modelOutputAvailability(
	gate agentapi.RunBudgetUsageGate,
	phase agentapi.RunBudgetPhase,
	availability agentapi.RunBudgetAvailability,
) agentapi.Usage {
	if phased, ok := gate.(agentapi.RunBudgetPhasedAvailability); ok {
		return phased.AvailableForPhase(phase)
	}
	return availability.Available()
}

func modelOutputMinimum(gate agentapi.RunBudgetUsageGate) int64 {
	if minimum, ok := gate.(agentapi.RunBudgetMinimum); ok {
		return minimum.MinimumOutputTokens()
	}
	return 0
}

func (agent *Agent) capOutputByCost(inputTokens, maxOutput int, availableCost int64) (int, error) {
	base, err := agent.modelCallEstimate(inputTokens, 0)
	if err != nil {
		return 0, fmt.Errorf("estimate model call cost: %w", err)
	}
	if base.CostMicros > availableCost {
		return 0, fmt.Errorf("%w: cost_micros input=%d available=%d", agentapi.ErrBudgetExceeded, base.CostMicros, availableCost)
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
		return 0, fmt.Errorf("%w: no output cost budget remains", agentapi.ErrBudgetExceeded)
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
