package workflow

import (
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// attemptBudgetGate adapts the Run ledger to one Agent attempt while retaining
// enough accounting state to distinguish usage settled by the Runtime from
// usage returned by a custom Runtime that ignored the gate.
//
// The adapter is intentionally per-attempt rather than per-node: retries get a
// fresh adapter, while all adapters still delegate to the same Run ledger.
type attemptBudgetGate struct {
	account *budgetAccount
	phase   agentapi.RunBudgetPhase

	mu        sync.Mutex
	accounted Usage
}

var _ agentapi.RunBudgetPhasedGate = (*attemptBudgetGate)(nil)
var _ agentapi.RunBudgetPhasedAvailability = (*attemptBudgetGate)(nil)

func newAttemptBudgetGate(account *budgetAccount, phase agentapi.RunBudgetPhase) *attemptBudgetGate {
	if phase == "" {
		phase = agentapi.RunBudgetPhaseDefault
	}
	return &attemptBudgetGate{account: account, phase: phase}
}

func (gate *attemptBudgetGate) Check() error {
	if gate == nil || gate.account == nil {
		return nil
	}
	return gate.account.checkForPhase(gate.phase)
}

func (gate *attemptBudgetGate) Available() agentapi.Usage {
	if gate == nil || gate.account == nil {
		return unboundedAgentUsage()
	}
	return gate.account.availableForPhase(gate.phase)
}

func (gate *attemptBudgetGate) AvailableForPhase(phase agentapi.RunBudgetPhase) agentapi.Usage {
	if gate == nil || gate.account == nil {
		return unboundedAgentUsage()
	}
	return gate.account.availableForPhase(phase)
}

func (gate *attemptBudgetGate) ReserveCall(
	estimate agentapi.Usage,
) (agentapi.RunBudgetCallReservation, error) {
	return gate.ReserveCallForPhase(estimate, gate.phase)
}

func (gate *attemptBudgetGate) ReserveCallForPhase(
	estimate agentapi.Usage,
	phase agentapi.RunBudgetPhase,
) (agentapi.RunBudgetCallReservation, error) {
	if gate == nil || gate.account == nil {
		return nil, nil
	}
	reservation, err := gate.account.ReserveCallForPhase(estimate, phase)
	if err != nil || reservation == nil {
		return reservation, err
	}
	workflowReservation, ok := reservation.(*workflowCallReservation)
	if !ok {
		// The Workflow account currently always returns workflowCallReservation.
		// Keep the adapter safe if that implementation changes: the underlying
		// reservation remains authoritative even if local usage cannot be read.
		return reservation, nil
	}
	return &attemptCallReservation{gate: gate, inner: workflowReservation}, nil
}

// AccountedUsage reports physical model usage that has already entered the
// shared ledger through a successful or over-limit settlement. An over-limit
// settlement is included deliberately: the provider's actual usage remains
// authoritative and must not be recorded a second time by the coordinator.
func (gate *attemptBudgetGate) AccountedUsage() Usage {
	if gate == nil {
		return Usage{}
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return gate.accounted
}

func (gate *attemptBudgetGate) recordSettled(actual agentapi.Usage) {
	if gate == nil {
		return
	}
	usage := workflowUsageFromAgent(actual)
	gate.mu.Lock()
	gate.accounted = addUsage(gate.accounted, usage)
	gate.mu.Unlock()
}

type attemptCallReservation struct {
	gate  *attemptBudgetGate
	inner *workflowCallReservation
}

func (reservation *attemptCallReservation) Settle(actual agentapi.Usage) error {
	if reservation == nil || reservation.inner == nil {
		return nil
	}
	err := reservation.inner.Settle(actual)
	if settled, accounted := reservation.inner.settledUsage(); settled {
		reservation.gate.recordSettled(accounted)
	}
	return err
}

func (reservation *attemptCallReservation) Release() error {
	if reservation == nil || reservation.inner == nil {
		return nil
	}
	return reservation.inner.Release()
}
