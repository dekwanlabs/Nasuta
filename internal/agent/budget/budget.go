// Package budget implements the in-process hierarchical budget ledger used by
// parent and delegated Agent Runs. The ledger is intentionally transport and
// persistence agnostic: durable admission remains the responsibility of the
// delegation store, while this package closes the physical-provider-call gap.
package budget

import (
	"fmt"
	"math"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// Root is the shared ledger for one logical parent Run. Child grants remain
// reserved until their task finishes, so a parent cannot consume capacity that
// was already admitted to a child. Actual provider usage is settled exactly
// once through call reservations.
type Root struct {
	mu sync.Mutex

	limits agentapi.RunLimits
	used   agentapi.Usage
	// directInFlight contains physical calls made directly by the parent.
	directInFlight agentapi.Usage
	tasks          map[*Task]struct{}
}

// Task is an admitted child budget carved out of a Root. It is deliberately not
// itself a RunBudgetTaskGate: a delegated child may make model calls, but it
// cannot recursively admit another child through this handle.
type Task struct {
	root  *Root
	grant agentapi.Usage
	used  agentapi.Usage
	// inFlight contains physical calls whose estimates are still outstanding.
	inFlight agentapi.Usage
	released bool
}

type callReservation struct {
	mu sync.Mutex

	root     *Root
	task     *Task
	estimate agentapi.Usage
	state    reservationState
	actual   agentapi.Usage
}

type reservationState uint8

const (
	reservationOpen reservationState = iota
	reservationSettled
	reservationReleased
)

// NewRoot creates a shared ledger from the effective limits of one parent Run.
// Zero aggregate limits retain the existing unlimited semantics. A zero total
// limit therefore does not make the answer reserve meaningful by itself.
func NewRoot(limits agentapi.RunLimits) *Root {
	return &Root{limits: limits, tasks: make(map[*Task]struct{})}
}

// Limits returns the immutable limits used to construct the ledger.
func (root *Root) Limits() agentapi.RunLimits {
	if root == nil {
		return agentapi.RunLimits{}
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	return root.limits
}

// Check verifies that settled and in-flight accounting remains within the
// root hard limits. It is safe to call concurrently with reservations.
func (root *Root) Check() error {
	if root == nil {
		return nil
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	return root.checkLocked(agentapi.RunBudgetPhaseAnswer)
}

// Available reports capacity available to non-answer calls. The protected
// parent-answer reserve is removed from output/total capacity.
func (root *Root) Available() agentapi.Usage {
	return root.AvailableForPhase(agentapi.RunBudgetPhaseDefault)
}

// AvailableForPhase reports capacity available to a physical parent call.
// Answer calls may consume the protected parent-answer reserve; all other
// calls must leave it intact.
func (root *Root) AvailableForPhase(phase agentapi.RunBudgetPhase) agentapi.Usage {
	if root == nil {
		return unboundedUsage()
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	return root.availableLocked(phase)
}

// ReserveCall reserves one direct (normally parent) physical provider call.
func (root *Root) ReserveCall(estimate agentapi.Usage) (agentapi.RunBudgetCallReservation, error) {
	return root.ReserveCallForPhase(estimate, agentapi.RunBudgetPhaseDefault)
}

// ReserveCallForPhase reserves a direct physical call while applying the
// answer-reserve policy for the selected phase.
func (root *Root) ReserveCallForPhase(
	estimate agentapi.Usage,
	phase agentapi.RunBudgetPhase,
) (agentapi.RunBudgetCallReservation, error) {
	if root == nil {
		return nil, nil
	}
	estimate, err := normalizeUsage(estimate)
	if err != nil {
		return nil, err
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	available := root.availableLocked(phase)
	if err := requireWithin(estimate, available, "direct model call"); err != nil {
		return nil, err
	}
	root.directInFlight = addUsage(root.directInFlight, estimate)
	return &callReservation{root: root, estimate: estimate}, nil
}

// ReserveTask admits a bounded child budget from the root. Only the child's
// output budget is reserved upfront; input and total stay unbounded per-child
// and are charged against the shared ledger by each physical call.
func (root *Root) ReserveTask(grant agentapi.Usage) (agentapi.RunBudgetTaskReservation, error) {
	if root == nil {
		return nil, nil
	}
	if grant.InputTokens < 0 || grant.OutputTokens < 0 || grant.TotalTokens < 0 || grant.CostMicros < 0 {
		return nil, fmt.Errorf("%w: usage cannot be negative", agentapi.ErrBudgetExceeded)
	}
	// Deliberately skip the TotalTokens normalization: a zero total means
	// "unbounded per-child", not "fill from input+output".
	root.mu.Lock()
	defer root.mu.Unlock()
	available := root.availableLocked(agentapi.RunBudgetPhaseDefault)
	if err := requireWithin(grant, available, "child task"); err != nil {
		return nil, err
	}
	task := &Task{root: root, grant: grant}
	root.tasks[task] = struct{}{}
	return task, nil
}

// Used returns settled provider usage. It excludes in-flight reservations.
func (root *Root) Used() agentapi.Usage {
	if root == nil {
		return agentapi.Usage{}
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	return root.used
}

// Available returns the remaining capacity inside this admitted child grant.
// Child calls never consume the parent's protected answer reserve.
func (task *Task) Available() agentapi.Usage {
	if task == nil || task.root == nil {
		return unboundedUsage()
	}
	task.root.mu.Lock()
	defer task.root.mu.Unlock()
	return task.availableLocked()
}

func (task *Task) Check() error {
	if task == nil || task.root == nil {
		return nil
	}
	task.root.mu.Lock()
	defer task.root.mu.Unlock()
	if task.released {
		return fmt.Errorf("%w: child task budget released", agentapi.ErrBudgetExceeded)
	}
	used := addUsage(task.used, task.inFlight)
	// Only bounded dimensions are checked. Input and total are zero for a child
	// (no per-child cumulative quota); the shared batch ledger bounds them.
	if task.grant.OutputTokens > 0 && used.OutputTokens > task.grant.OutputTokens {
		return fmt.Errorf("%w: child task usage output=%d exceeds %d", agentapi.ErrBudgetExceeded, used.OutputTokens, task.grant.OutputTokens)
	}
	if task.grant.CostMicros > 0 && used.CostMicros > task.grant.CostMicros {
		return fmt.Errorf("%w: child task usage cost=%d exceeds %d", agentapi.ErrBudgetExceeded, used.CostMicros, task.grant.CostMicros)
	}
	return nil
}

func (task *Task) ReserveCall(estimate agentapi.Usage) (agentapi.RunBudgetCallReservation, error) {
	if task == nil || task.root == nil {
		return nil, nil
	}
	estimate, err := normalizeUsage(estimate)
	if err != nil {
		return nil, err
	}
	task.root.mu.Lock()
	defer task.root.mu.Unlock()
	if task.released {
		return nil, fmt.Errorf("%w: child task budget released", agentapi.ErrBudgetExceeded)
	}
	available := task.availableLocked()
	if err := requireWithin(estimate, available, "child model call"); err != nil {
		return nil, err
	}
	task.inFlight = addUsage(task.inFlight, estimate)
	return &callReservation{root: task.root, task: task, estimate: estimate}, nil
}

// Release returns all unused task capacity to the root. It is idempotent.
func (task *Task) Release() error {
	if task == nil || task.root == nil {
		return nil
	}
	task.root.mu.Lock()
	defer task.root.mu.Unlock()
	if task.released {
		return nil
	}
	if !isZeroUsage(task.inFlight) {
		return fmt.Errorf("%w: cannot release child task with in-flight model calls", agentapi.ErrBudgetExceeded)
	}
	task.released = true
	delete(task.root.tasks, task)
	return nil
}

func (reservation *callReservation) Settle(actual agentapi.Usage) error {
	if reservation == nil {
		return nil
	}
	actual, err := normalizeUsage(actual)
	if err != nil {
		return err
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.state == reservationSettled {
		if reservation.actual == actual {
			return nil
		}
		return fmt.Errorf("%w: model call settled twice with different usage", agentapi.ErrBudgetExceeded)
	}
	if reservation.state == reservationReleased {
		return fmt.Errorf("%w: model call reservation already released", agentapi.ErrBudgetExceeded)
	}
	if reservation.root == nil {
		reservation.state = reservationSettled
		reservation.actual = actual
		return nil
	}
	root := reservation.root
	root.mu.Lock()
	defer root.mu.Unlock()
	var accountingErr error
	if reservation.task != nil {
		task := reservation.task
		if task.released {
			return fmt.Errorf("%w: child task budget released", agentapi.ErrBudgetExceeded)
		}
		task.inFlight = subtractUsage(task.inFlight, reservation.estimate)
		task.used = addUsage(task.used, actual)
		if err := requireWithin(actual, reservation.estimate, "reported child model usage"); err != nil {
			accountingErr = err
		}
	} else {
		root.directInFlight = subtractUsage(root.directInFlight, reservation.estimate)
		if err := requireWithin(actual, reservation.estimate, "reported model usage"); err != nil {
			accountingErr = err
		}
	}
	root.used = addUsage(root.used, actual)
	reservation.state = reservationSettled
	reservation.actual = actual
	if err := root.checkLocked(agentapi.RunBudgetPhaseAnswer); err != nil {
		if accountingErr == nil {
			accountingErr = err
		}
	}
	return accountingErr
}

func (reservation *callReservation) Release() error {
	if reservation == nil {
		return nil
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.state == reservationReleased {
		return nil
	}
	if reservation.state == reservationSettled {
		return nil
	}
	if reservation.root != nil {
		reservation.root.mu.Lock()
		defer reservation.root.mu.Unlock()
		if reservation.task != nil {
			reservation.task.inFlight = subtractUsage(reservation.task.inFlight, reservation.estimate)
		} else {
			reservation.root.directInFlight = subtractUsage(
				reservation.root.directInFlight,
				reservation.estimate,
			)
		}
	}
	reservation.state = reservationReleased
	return nil
}

func (root *Root) availableLocked(phase agentapi.RunBudgetPhase) agentapi.Usage {
	allocated := root.used
	allocated = addUsage(allocated, root.directInFlight)
	for task := range root.tasks {
		allocated = addUsage(allocated, taskReserved(task))
	}
	available := remainingUsage(root.limits, allocated)
	if phase != agentapi.RunBudgetPhaseAnswer && root.limits.ParentAnswerReserve > 0 {
		reserve := root.limits.ParentAnswerReserve
		if available.TotalTokens != math.MaxInt64 {
			available.TotalTokens = max(0, available.TotalTokens-reserve)
		}
		if available.OutputTokens != math.MaxInt64 {
			available.OutputTokens = max(0, available.OutputTokens-reserve)
		}
	}
	return available
}

// taskReserved reports the root capacity a child still holds. A full grant
// reserves its input, output and total. An output-only grant (total zero) is
// the delegation shape: the child has no cumulative input quota, so its output
// is the only reserved dimension, reflected in the total for the root ledger.
func taskReserved(task *Task) agentapi.Usage {
	return TaskReserved(task.grant, task.used)
}

// TaskReserved reports the root capacity a child grant still holds after usage.
// A zero total means "output is the only reserved dimension", reflected into
// total so the root ledger still bounds child output against MaxTotalTokens.
func TaskReserved(grant, used agentapi.Usage) agentapi.Usage {
	unconsumed := SubtractUsage(grant, used)
	if grant.TotalTokens == 0 {
		unconsumed.TotalTokens = unconsumed.OutputTokens
	}
	return unconsumed
}

func (root *Root) checkLocked(_ agentapi.RunBudgetPhase) error {
	allocated := root.used
	allocated = addUsage(allocated, root.directInFlight)
	for task := range root.tasks {
		allocated = addUsage(allocated, taskReserved(task))
	}
	if root.limits.MaxInputTokens > 0 && allocated.InputTokens > root.limits.MaxInputTokens {
		return fmt.Errorf("%w: root input tokens %d exceed %d", agentapi.ErrBudgetExceeded, allocated.InputTokens, root.limits.MaxInputTokens)
	}
	if root.limits.MaxTotalTokens > 0 && allocated.TotalTokens > root.limits.MaxTotalTokens {
		return fmt.Errorf("%w: root total tokens %d exceed %d", agentapi.ErrBudgetExceeded, allocated.TotalTokens, root.limits.MaxTotalTokens)
	}
	if root.limits.MaxCostMicros > 0 && allocated.CostMicros > root.limits.MaxCostMicros {
		return fmt.Errorf("%w: root cost %d exceed %d", agentapi.ErrBudgetExceeded, allocated.CostMicros, root.limits.MaxCostMicros)
	}
	return nil
}

func (task *Task) availableLocked() agentapi.Usage {
	return Remaining(task.grant, addUsage(task.used, task.inFlight))
}

// Remaining reports grant capacity after subtracting usage, dimension by
// dimension. A zero grant dimension means "unbounded per-child" and stays
// unbounded, unlike SubtractUsage which clamps at zero.
func Remaining(grant, used agentapi.Usage) agentapi.Usage {
	return agentapi.Usage{
		InputTokens:  remaining(grant.InputTokens, used.InputTokens),
		OutputTokens: remaining(grant.OutputTokens, used.OutputTokens),
		TotalTokens:  remaining(grant.TotalTokens, used.TotalTokens),
		CostMicros:   remaining(grant.CostMicros, used.CostMicros),
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

func remainingUsage(limits agentapi.RunLimits, used agentapi.Usage) agentapi.Usage {
	return agentapi.Usage{
		InputTokens: remaining(limits.MaxInputTokens, used.InputTokens),
		// MaxOutputTokens is a per-provider-call ceiling, not a cumulative
		// budget. Root output capacity is therefore derived from MaxTotalTokens.
		OutputTokens: remaining(limits.MaxTotalTokens, used.TotalTokens),
		TotalTokens:  remaining(limits.MaxTotalTokens, used.TotalTokens),
		CostMicros:   remaining(limits.MaxCostMicros, used.CostMicros),
	}
}

// RequireWithin rejects usage that exceeds any available budget dimension.
func RequireWithin(request, available agentapi.Usage, subject string) error {
	if request.InputTokens > available.InputTokens ||
		request.OutputTokens > available.OutputTokens ||
		request.TotalTokens > available.TotalTokens ||
		request.CostMicros > available.CostMicros {
		return fmt.Errorf(
			"%w: %s input=%d output=%d total=%d cost=%d available_input=%d available_output=%d available_total=%d available_cost=%d",
			agentapi.ErrBudgetExceeded, subject,
			request.InputTokens, request.OutputTokens, request.TotalTokens, request.CostMicros,
			available.InputTokens, available.OutputTokens, available.TotalTokens, available.CostMicros,
		)
	}
	return nil
}

func requireWithin(request, available agentapi.Usage, subject string) error {
	return RequireWithin(request, available, subject)
}

// RunLimitsEqual reports whether two durable run budgets describe the same limits.
func RunLimitsEqual(left, right agentapi.RunLimits) bool {
	return left.Deadline.Equal(right.Deadline) &&
		left.MaxSteps == right.MaxSteps &&
		left.MaxToolCalls == right.MaxToolCalls &&
		left.MaxInputTokens == right.MaxInputTokens &&
		left.MaxContextTokens == right.MaxContextTokens &&
		left.MaxOutputTokens == right.MaxOutputTokens &&
		left.MaxTotalTokens == right.MaxTotalTokens &&
		left.MaxCostMicros == right.MaxCostMicros &&
		left.ParentAnswerReserve == right.ParentAnswerReserve
}

// NormalizeUsage validates a public usage value and fills TotalTokens from
// input and output when the caller left it unset. It is shared by the
// in-process, durable, and workflow budget ledgers so a negative or
// overflowing usage fails identically everywhere.
func NormalizeUsage(usage agentapi.Usage) (agentapi.Usage, error) {
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

func normalizeUsage(usage agentapi.Usage) (agentapi.Usage, error) {
	return NormalizeUsage(usage)
}

// NormalizeGrant validates a child task grant without deriving a total. A zero
// TotalTokens is a meaningful "unbounded per-child" signal for delegation
// grants, unlike NormalizeUsage which fills it from input+output.
func NormalizeGrant(grant agentapi.Usage) (agentapi.Usage, error) {
	if grant.InputTokens < 0 || grant.OutputTokens < 0 || grant.ReasoningTokens < 0 ||
		grant.TotalTokens < 0 || grant.CostMicros < 0 {
		return agentapi.Usage{}, fmt.Errorf("%w: usage cannot be negative", agentapi.ErrBudgetExceeded)
	}
	return grant, nil
}

// AddUsage sums two public usage values with saturating arithmetic.
func AddUsage(left, right agentapi.Usage) agentapi.Usage {
	return agentapi.Usage{
		InputTokens:     saturatingAdd(left.InputTokens, right.InputTokens),
		OutputTokens:    saturatingAdd(left.OutputTokens, right.OutputTokens),
		ReasoningTokens: saturatingAdd(left.ReasoningTokens, right.ReasoningTokens),
		TotalTokens:     saturatingAdd(left.TotalTokens, right.TotalTokens),
		CostMicros:      saturatingAdd(left.CostMicros, right.CostMicros),
	}
}

func addUsage(left, right agentapi.Usage) agentapi.Usage {
	return AddUsage(left, right)
}

// SubtractUsage clamps each usage dimension at zero.
func SubtractUsage(left, right agentapi.Usage) agentapi.Usage {
	return agentapi.Usage{
		InputTokens:     max(0, left.InputTokens-right.InputTokens),
		OutputTokens:    max(0, left.OutputTokens-right.OutputTokens),
		ReasoningTokens: max(0, left.ReasoningTokens-right.ReasoningTokens),
		TotalTokens:     max(0, left.TotalTokens-right.TotalTokens),
		CostMicros:      max(0, left.CostMicros-right.CostMicros),
	}
}

func subtractUsage(left, right agentapi.Usage) agentapi.Usage {
	return SubtractUsage(left, right)
}

// IsZeroUsage reports whether a public usage value contributes nothing.
func IsZeroUsage(usage agentapi.Usage) bool {
	return usage == (agentapi.Usage{})
}

func isZeroUsage(usage agentapi.Usage) bool {
	return IsZeroUsage(usage)
}

func unboundedUsage() agentapi.Usage {
	return agentapi.Usage{
		InputTokens: math.MaxInt64, OutputTokens: math.MaxInt64,
		TotalTokens: math.MaxInt64, CostMicros: math.MaxInt64,
	}
}

func saturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func max(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
