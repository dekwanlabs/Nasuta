package definition

import (
	"errors"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	redaction "github.com/dekwanlabs/nasuta/platform/redact"
)

// finalizer owns the single terminal-outcome responsibility for one active
// run. Execute computes the durable outcome; the finalizer validates run
// state, merges preparation evidence, applies scenario/run errors, and
// publishes the terminal through either the fenced durable path or the hub.
// It is intentionally separate from compile/execute/recovery so the terminal
// transition has one owner and one call path.
type finalizer struct {
	run *activeRun
}

func (f finalizer) finish(runError *agentapi.RunError) error {
	outcome, err := f.prepareFinishOutcome(runError)
	if err != nil {
		return err
	}
	if f.run.ownsTrace {
		f.run.trace.Close()
	}
	completionErr := f.persistFinishOutcome(outcome)
	releaseErr := f.releaseFinishLease()
	return errors.Join(completionErr, releaseErr)
}

// prepareFinishOutcome validates the run state, finalizes the outcome (merging
// preparation evidence and applying runError), and marks the run finished.
func (f finalizer) prepareFinishOutcome(runError *agentapi.RunError) (agentrun.Outcome, error) {
	run := f.run
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.finished {
		return agentrun.Outcome{}, fmt.Errorf("definition run %q is already finished", run.start.RunID)
	}
	if !run.executed && runError == nil {
		return agentrun.Outcome{}, fmt.Errorf("definition run %q has not executed", run.start.RunID)
	}
	outcome := run.outcome
	if !run.outcomeSet {
		outcome = mergePreparationOutcome(outcome, run.preparationEvidence)
	}
	if runError != nil {
		outcome = applyRunError(outcome, runError, run.start.Policy.RedactSensitive)
	}
	run.finished = true
	return outcome, nil
}

func applyRunError(outcome agentrun.Outcome, runError *agentapi.RunError, redact bool) agentrun.Outcome {
	code := strings.TrimSpace(runError.Code)
	if code == "" {
		code = "scenario_failed"
	}
	message := strings.TrimSpace(runError.Message)
	if message == "" {
		message = code
	}
	if redact {
		message = redaction.RedactSensitiveText(message)
	}
	outcome.Status = agentrun.StatusFailed
	outcome.ErrorCode = code
	outcome.Err = errors.New(message)
	if outcome.Evidence.Status == "" {
		outcome.Evidence.Status = agentrun.EvidenceUnavailable
	}
	return outcome
}

// persistFinishOutcome publishes the outcome, preferring the fenced durable
// completion path when the budget is durable and exposes lease information.
func (f finalizer) persistFinishOutcome(outcome agentrun.Outcome) error {
	completedByLease, fencedCompletion, completionErr := f.completeFencedIfDurable(outcome)
	if completedByLease {
		f.run.runtime.hub.ProjectTerminal(f.run.start.RunID, outcome)
	} else if !fencedCompletion {
		f.run.runtime.hub.Complete(f.run.start.RunID, outcome)
	}
	return completionErr
}

func (f finalizer) completeFencedIfDurable(outcome agentrun.Outcome) (completedByLease, fencedCompletion bool, completionErr error) {
	run := f.run
	if run.runtime.runStore == nil || !run.runtime.runStore.DurableBudgetEnabled() {
		return false, false, nil
	}
	root, ok := run.budget.(interface{ LeaseInfo() (string, int64, error) })
	if !ok {
		return false, false, nil
	}
	fencedCompletion = true
	owner, fence, leaseErr := root.LeaseInfo()
	if leaseErr != nil {
		return false, fencedCompletion, fmt.Errorf("read durable run lease: %w", leaseErr)
	}
	if completeErr := run.runtime.runStore.CompleteFenced(run.start.RunID, owner, fence, outcome); completeErr != nil {
		// Never fall back to the unfenced Hub.Complete path. A stale
		// owner must not publish or overwrite a result after reclamation.
		return false, fencedCompletion, fmt.Errorf("persist fenced run outcome: %w", completeErr)
	}
	return true, fencedCompletion, nil
}

func (f finalizer) releaseFinishLease() error {
	if lease, ok := f.run.budget.(interface{ Close() }); ok {
		lease.Close()
	}
	if lease, ok := f.run.budget.(interface{ ReleaseLease() error }); ok {
		if err := lease.ReleaseLease(); err != nil {
			return fmt.Errorf("release durable budget lease for run %q: %w", f.run.start.RunID, err)
		}
	}
	return nil
}
