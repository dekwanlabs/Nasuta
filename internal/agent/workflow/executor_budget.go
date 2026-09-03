package workflow

import (
	"fmt"
	"math"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// budgetAccount is the shared ledger for one Workflow Run. It is deliberately
// shaped like the public Agent Runtime budget gate so every physical model
// call, including calls made by parallel child Runs, is admitted and settled
// against the same Run-level limits.
type budgetAccount struct {
	mu sync.Mutex

	budget   Budget
	usage    Usage
	reserved Usage
}

var _ agentapi.RunBudgetGate = (*budgetAccount)(nil)
var _ agentapi.RunBudgetUsageGate = (*budgetAccount)(nil)
var _ agentapi.RunBudgetAvailability = (*budgetAccount)(nil)
var _ agentapi.RunBudgetPhasedGate = (*budgetAccount)(nil)
var _ agentapi.RunBudgetPhasedAvailability = (*budgetAccount)(nil)

func newBudgetAccount(
	budget Budget,
	usage Usage,
) (*budgetAccount, error) {
	if err := validateUsage(usage); err != nil {
		return nil, fmt.Errorf("restore workflow usage: %w", err)
	}
	account := &budgetAccount{budget: budget, usage: usage}
	if err := account.checkUsage(); err != nil {
		return nil, fmt.Errorf("restore workflow usage: %w", err)
	}
	return account, nil
}

// Check implements agentapi.RunBudgetGate. It includes in-flight call
// reservations so a caller cannot start another physical call after the Run
// has already admitted all remaining capacity.
func (account *budgetAccount) Check() error {
	return account.checkForPhase(agentapi.RunBudgetPhaseDefault)
}

func (account *budgetAccount) checkForPhase(phase agentapi.RunBudgetPhase) error {
	if account == nil {
		return nil
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if err := account.checkCapacityForPhase(Usage{}, phase); err != nil {
		return budgetExceededError("workflow Run budget is exhausted: %v", err)
	}
	return nil
}

// CanStartAgent reports whether at least one more physical Agent model call
// can be admitted under the Run limits. Check intentionally only validates
// that the ledger has not overrun a hard limit; this stronger coordinator
// preflight is needed for custom Agent executors that do not reach the
// Runtime's ReserveCall gate. Tool-call and retry limits are excluded here:
// an Agent can still make a model call without another tool call or retry.
func (account *budgetAccount) CanStartAgent() bool {
	return account.CanStartAgentForPhase(agentapi.RunBudgetPhaseDefault)
}

// CanStartAgentForPhase is the coordinator preflight used for custom Agent
// executors that may not reach the Runtime's physical-call gate.
func (account *budgetAccount) CanStartAgentForPhase(phase agentapi.RunBudgetPhase) bool {
	if account == nil {
		return true
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.checkCapacityForPhase(Usage{}, phase) != nil {
		return false
	}
	available := account.availableLocked(phase)
	return available.InputTokens > 0 &&
		available.OutputTokens > 0 &&
		available.TotalTokens > 0 &&
		available.CostMicros > 0
}

// Available reports the remaining Run capacity after settled usage and
// in-flight estimates. Zero workflow limits retain the unlimited convention
// used by the Agent Runtime budget contracts.
func (account *budgetAccount) Available() agentapi.Usage {
	if account == nil {
		return unboundedAgentUsage()
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.availableLocked(agentapi.RunBudgetPhaseDefault)
}

// AvailableForPhase reports capacity after applying downstream phase reserves.
func (account *budgetAccount) AvailableForPhase(phase agentapi.RunBudgetPhase) agentapi.Usage {
	return account.availableForPhase(phase)
}

func (account *budgetAccount) availableForPhase(phase agentapi.RunBudgetPhase) agentapi.Usage {
	if account == nil {
		return unboundedAgentUsage()
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.availableLocked(phase)
}

// Usage returns the settled aggregate usage for this Workflow Run. In-flight
// reservations are intentionally excluded until their calls settle.
func (account *budgetAccount) Usage() Usage {
	if account == nil {
		return Usage{}
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	return account.usage
}

// ReserveCall atomically admits one physical provider call. The estimate is
// held until the provider reports usage or the caller releases it.
func (account *budgetAccount) ReserveCall(
	estimate agentapi.Usage,
) (agentapi.RunBudgetCallReservation, error) {
	return account.ReserveCallForPhase(estimate, agentapi.RunBudgetPhaseDefault)
}

// ReserveCallForPhase atomically admits a physical provider call while
// preserving the configured capacity for downstream workflow phases.
func (account *budgetAccount) ReserveCallForPhase(
	estimate agentapi.Usage,
	phase agentapi.RunBudgetPhase,
) (agentapi.RunBudgetCallReservation, error) {
	if account == nil {
		return nil, nil
	}
	estimate, err := normalizeAgentUsage(estimate)
	if err != nil {
		return nil, err
	}
	workflowEstimate := workflowUsageFromAgent(estimate)
	account.mu.Lock()
	defer account.mu.Unlock()
	if err := account.checkCapacityForPhase(workflowEstimate, phase); err != nil {
		return nil, budgetExceededError("reserve workflow model call: %v", err)
	}
	account.reserved = addUsage(account.reserved, workflowEstimate)
	return &workflowCallReservation{
		account:  account,
		estimate: estimate,
	}, nil
}

// RecordUsage accounts usage returned by a non-Agent node executor. Agent
// nodes normally settle at the physical model-call layer through ReserveCall;
// executeAttempt uses this method only for deterministic/custom executors that
// do not participate in the shared gate.
func (account *budgetAccount) RecordUsage(actual Usage) error {
	if account == nil || actual.IsZero() {
		return nil
	}
	if err := validateUsage(actual); err != nil {
		return err
	}
	actual = normalizeWorkflowUsage(actual)
	account.mu.Lock()
	defer account.mu.Unlock()
	account.usage = addUsage(account.usage, actual)
	if err := account.checkUsage(); err != nil {
		return budgetExceededError("record workflow node usage: %v", err)
	}
	return nil
}

// ConsumeRetry charges one retry admission to the same Run budget. It is
// called immediately before a retry attempt starts, so a retry can never
// silently obtain a fresh copy of the Workflow budget.
func (account *budgetAccount) ConsumeRetry() error {
	if account == nil {
		return nil
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	additional := Usage{Retries: 1}
	if err := account.checkCapacity(additional); err != nil {
		return fmt.Errorf(
			"%w: %w: workflow retry budget is exhausted: %v",
			ErrNoAffordableTask, ErrBudgetExhausted, err,
		)
	}
	account.usage = addUsage(account.usage, additional)
	return nil
}

func (account *budgetAccount) availableLocked(phase agentapi.RunBudgetPhase) agentapi.Usage {
	allocated := addUsage(account.usage, account.reserved)
	available := agentapi.Usage{
		InputTokens:  remaining(account.budget.MaxInputTokens, allocated.InputTokens),
		OutputTokens: remaining(account.budget.MaxOutputTokens, allocated.OutputTokens),
		TotalTokens:  remaining(account.budget.MaxTotalTokens, allocated.TotalTokens),
		CostMicros:   remaining(account.budget.MaxCostMicros, allocated.CostMicros),
	}
	protected := account.protectedTokens(phase)
	// A zero limit means unlimited. The corresponding MaxInt64 sentinel must
	// not be reduced by a finite downstream reserve.
	if account.budget.MaxOutputTokens > 0 {
		available.OutputTokens = subtractProtected(available.OutputTokens, protected)
	}
	if account.budget.MaxTotalTokens > 0 {
		available.TotalTokens = subtractProtected(available.TotalTokens, protected)
	}
	return available
}

func (account *budgetAccount) protectedTokens(phase agentapi.RunBudgetPhase) int64 {
	if account == nil {
		return 0
	}
	switch phase {
	case agentapi.RunBudgetPhaseAnswer:
		return 0
	case agentapi.RunBudgetPhaseVerifier:
		return account.budget.ComposerReserveTokens
	default:
		return account.budget.VerifierReserveTokens + account.budget.ComposerReserveTokens
	}
}

func subtractProtected(available, protected int64) int64 {
	if available <= 0 || protected <= 0 {
		return maxInt64(0, available)
	}
	if protected >= available {
		return 0
	}
	return available - protected
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

func (account *budgetAccount) checkCapacityForPhase(additional Usage, phase agentapi.RunBudgetPhase) error {
	if err := account.checkCapacity(additional); err != nil {
		return err
	}
	available := account.availableLocked(phase)
	if additional.InputTokens > available.InputTokens ||
		additional.OutputTokens > available.OutputTokens ||
		additional.TotalTokens > available.TotalTokens ||
		additional.CostMicros > available.CostMicros {
		return fmt.Errorf("downstream protected capacity is reserved for phase %q", phase)
	}
	return nil
}

func (account *budgetAccount) checkUsage() error {
	if account == nil {
		return nil
	}
	copy := &budgetAccount{budget: account.budget, usage: account.usage}
	return copy.checkCapacity(Usage{})
}

type workflowCallReservation struct {
	mu sync.Mutex

	account  *budgetAccount
	estimate agentapi.Usage
	state    workflowReservationState
	actual   agentapi.Usage
}

type workflowReservationState uint8

const (
	workflowReservationOpen workflowReservationState = iota
	workflowReservationSettled
	workflowReservationReleased
)

func (reservation *workflowCallReservation) isSettled() bool {
	settled, _ := reservation.settledUsage()
	return settled
}

func (reservation *workflowCallReservation) settledUsage() (bool, agentapi.Usage) {
	if reservation == nil {
		return false, agentapi.Usage{}
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.state != workflowReservationSettled {
		return false, agentapi.Usage{}
	}
	return true, reservation.actual
}

func (reservation *workflowCallReservation) Settle(actual agentapi.Usage) error {
	if reservation == nil {
		return nil
	}
	actual, err := normalizeAgentUsage(actual)
	if err != nil {
		return err
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	switch reservation.state {
	case workflowReservationSettled:
		if reservation.actual == actual {
			return nil
		}
		return fmt.Errorf("%w: workflow model call settled twice with different usage", agentapi.ErrBudgetExceeded)
	case workflowReservationReleased:
		return fmt.Errorf("%w: workflow model call reservation already released", agentapi.ErrBudgetExceeded)
	}
	if reservation.account == nil {
		reservation.state = workflowReservationSettled
		reservation.actual = actual
		return nil
	}

	account := reservation.account
	account.mu.Lock()
	account.reserved = subtractUsage(
		account.reserved,
		workflowUsageFromAgent(reservation.estimate),
	)
	// Provider usage is authoritative. Even when it exceeds the admission
	// estimate, retain it in the aggregate so the hard-limit error and the
	// durable result expose the actual consumption.
	account.usage = addUsage(account.usage, workflowUsageFromAgent(actual))
	accountingErr := account.checkUsage()
	account.mu.Unlock()

	reservation.state = workflowReservationSettled
	reservation.actual = actual
	if accountingErr != nil {
		return budgetExceededError("settle workflow model call: %v", accountingErr)
	}
	return nil
}

func (reservation *workflowCallReservation) Release() error {
	if reservation == nil {
		return nil
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	switch reservation.state {
	case workflowReservationReleased, workflowReservationSettled:
		return nil
	}
	if reservation.account != nil {
		reservation.account.mu.Lock()
		reservation.account.reserved = subtractUsage(
			reservation.account.reserved,
			workflowUsageFromAgent(reservation.estimate),
		)
		reservation.account.mu.Unlock()
	}
	reservation.state = workflowReservationReleased
	return nil
}

func budgetExceededError(format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	return fmt.Errorf("%w: %w: %s", agentapi.ErrBudgetExceeded, ErrBudgetExhausted, message)
}

func workflowUsageFromAgent(usage agentapi.Usage) Usage {
	return Usage{
		InputTokens:     usage.InputTokens,
		OutputTokens:    usage.OutputTokens,
		ReasoningTokens: usage.ReasoningTokens,
		TotalTokens:     usage.TotalTokens,
		CostMicros:      usage.CostMicros,
	}
}

func normalizeAgentUsage(usage agentapi.Usage) (agentapi.Usage, error) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.ReasoningTokens < 0 ||
		usage.TotalTokens < 0 || usage.CostMicros < 0 {
		return agentapi.Usage{}, fmt.Errorf("%w: usage cannot be negative", agentapi.ErrBudgetExceeded)
	}
	if usage.TotalTokens == 0 {
		if usage.InputTokens > math.MaxInt64-usage.OutputTokens {
			return agentapi.Usage{}, fmt.Errorf("%w: usage total overflow", agentapi.ErrBudgetExceeded)
		}
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage, nil
}

func normalizeWorkflowUsage(usage Usage) Usage {
	if usage.TotalTokens == 0 && usage.InputTokens <= math.MaxInt64-usage.OutputTokens {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage
}

func unboundedAgentUsage() agentapi.Usage {
	return agentapi.Usage{
		InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64,
		TotalTokens: math.MaxInt64, CostMicros: math.MaxInt64,
	}
}

func remaining(limit, used int64) int64 {
	if limit <= 0 {
		return math.MaxInt64
	}
	if used >= limit {
		return 0
	}
	return limit - used
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
		InputTokens:     maxInt64(0, left.InputTokens-right.InputTokens),
		OutputTokens:    maxInt64(0, left.OutputTokens-right.OutputTokens),
		ReasoningTokens: maxInt64(0, left.ReasoningTokens-right.ReasoningTokens),
		TotalTokens:     maxInt64(0, left.TotalTokens-right.TotalTokens),
		ToolCalls:       maxInt64(0, left.ToolCalls-right.ToolCalls),
		CostMicros:      maxInt64(0, left.CostMicros-right.CostMicros),
		Retries:         maxInt64(0, left.Retries-right.Retries),
	}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
