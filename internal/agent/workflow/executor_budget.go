package workflow

import (
	"fmt"
)

func newBudgetAccount(
	budget Budget,
	usage Usage,
) (*budgetAccount, error) {
	if err := validateUsage(usage); err != nil {
		return nil, fmt.Errorf("restore workflow usage: %w", err)
	}
	return &budgetAccount{budget: budget, usage: usage}, nil
}

func (account *budgetAccount) Reserve(
	node NodeDefinition,
	attempt int,
) (Usage, error) {
	reservation := Usage{
		InputTokens:  node.Budget.MaxInputTokens,
		OutputTokens: node.Budget.MaxOutputTokens,
		TotalTokens:  node.Budget.MaxTotalTokens,
		ToolCalls:    node.Budget.MaxToolCalls,
		CostMicros:   node.Budget.MaxCostMicros,
	}
	if attempt > 1 {
		reservation.Retries = 1
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if err := account.checkCapacity(reservation); err != nil {
		return Usage{}, fmt.Errorf(
			"%w for node %q attempt %d: %v",
			ErrNoAffordableTask,
			node.ID,
			attempt,
			err,
		)
	}
	account.reserved = addUsage(account.reserved, reservation)
	return reservation, nil
}

func (account *budgetAccount) Release(reservation Usage) {
	account.mu.Lock()
	account.reserved = subtractUsage(account.reserved, reservation)
	account.mu.Unlock()
}

func (account *budgetAccount) Settle(
	reservation Usage,
	actual *Usage,
	nodeBudget NodeBudget,
) error {
	account.mu.Lock()
	defer account.mu.Unlock()
	account.reserved = subtractUsage(account.reserved, reservation)
	actual.Retries = reservation.Retries
	account.usage = addUsage(account.usage, *actual)
	if err := nodeUsageWithinBudget(*actual, nodeBudget); err != nil {
		return fmt.Errorf("%w: %v", ErrBudgetExhausted, err)
	}
	if err := account.checkUsage(); err != nil {
		return fmt.Errorf("%w: %v", ErrBudgetExhausted, err)
	}
	return nil
}

func (account *budgetAccount) Usage() Usage {
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.usage
}

func (account *budgetAccount) checkCapacity(additional Usage) error {
	checks := []struct {
		name     string
		limit    int64
		used     int64
		reserved int64
		add      int64
	}{
		{"input tokens", account.budget.MaxInputTokens, account.usage.InputTokens, account.reserved.InputTokens, additional.InputTokens},
		{"output tokens", account.budget.MaxOutputTokens, account.usage.OutputTokens, account.reserved.OutputTokens, additional.OutputTokens},
		{"total tokens", account.budget.MaxTotalTokens, account.usage.TotalTokens, account.reserved.TotalTokens, additional.TotalTokens},
		{"tool calls", account.budget.MaxToolCalls, account.usage.ToolCalls, account.reserved.ToolCalls, additional.ToolCalls},
		{"cost", account.budget.MaxCostMicros, account.usage.CostMicros, account.reserved.CostMicros, additional.CostMicros},
		{"retries", account.budget.MaxRetries, account.usage.Retries, account.reserved.Retries, additional.Retries},
	}
	for _, check := range checks {
		if exceedsBudget(check.limit, check.used, check.reserved, check.add) {
			return fmt.Errorf("%s limit %d is exhausted", check.name, check.limit)
		}
	}
	return nil
}

func (account *budgetAccount) checkUsage() error {
	return (&budgetAccount{
		budget: account.budget,
		usage:  account.usage,
	}).checkCapacity(Usage{})
}

func nodeUsageWithinBudget(usage Usage, budget NodeBudget) error {
	checks := []struct {
		name  string
		limit int64
		value int64
	}{
		{"input tokens", budget.MaxInputTokens, usage.InputTokens},
		{"output tokens", budget.MaxOutputTokens, usage.OutputTokens},
		{"total tokens", budget.MaxTotalTokens, usage.TotalTokens},
		{"tool calls", budget.MaxToolCalls, usage.ToolCalls},
		{"cost", budget.MaxCostMicros, usage.CostMicros},
	}
	for _, check := range checks {
		if check.limit > 0 && check.value > check.limit {
			return fmt.Errorf("%s usage %d exceeds node reservation %d", check.name, check.value, check.limit)
		}
	}
	return nil
}

func validateUsage(usage Usage) error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningTokens < 0 ||
		usage.TotalTokens < 0 || usage.ToolCalls < 0 || usage.CostMicros < 0 ||
		usage.Retries < 0 {
		return fmt.Errorf("usage cannot be negative")
	}
	return nil
}

func exceedsBudget(limit, used, reserved, additional int64) bool {
	if limit == 0 {
		return false
	}
	if used > limit || reserved > limit-used {
		return true
	}
	return additional > limit-used-reserved
}

func addUsage(left, right Usage) Usage {
	return Usage{
		InputTokens:     left.InputTokens + right.InputTokens,
		OutputTokens:    left.OutputTokens + right.OutputTokens,
		ReasoningTokens: left.ReasoningTokens + right.ReasoningTokens,
		TotalTokens:     left.TotalTokens + right.TotalTokens,
		ToolCalls:       left.ToolCalls + right.ToolCalls,
		CostMicros:      left.CostMicros + right.CostMicros,
		Retries:         left.Retries + right.Retries,
	}
}

func subtractUsage(left, right Usage) Usage {
	return Usage{
		InputTokens:     left.InputTokens - right.InputTokens,
		OutputTokens:    left.OutputTokens - right.OutputTokens,
		ReasoningTokens: left.ReasoningTokens - right.ReasoningTokens,
		TotalTokens:     left.TotalTokens - right.TotalTokens,
		ToolCalls:       left.ToolCalls - right.ToolCalls,
		CostMicros:      left.CostMicros - right.CostMicros,
		Retries:         left.Retries - right.Retries,
	}
}
