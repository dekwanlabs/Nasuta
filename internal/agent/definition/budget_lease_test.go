package definition

import (
	"errors"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
)

type recordingBudgetLease struct {
	releases int
	err      error
}

func (lease *recordingBudgetLease) Check() error {
	return nil
}

func (lease *recordingBudgetLease) ReleaseLease() error {
	lease.releases++
	return lease.err
}

func TestActiveRunFinishReleasesDurableBudgetLease(t *testing.T) {
	lease := &recordingBudgetLease{}
	managed := &activeRun{
		runtime:    &Runtime{hub: agentrun.NewHub(nil)},
		start:      agentapi.RunStart{RunID: "root-run"},
		budget:     lease,
		executed:   true,
		outcomeSet: true,
		outcome:    agentrun.Outcome{Status: agentrun.StatusDone},
	}
	if err := managed.Finish(nil); err != nil {
		t.Fatal(err)
	}
	if lease.releases != 1 {
		t.Fatalf("lease releases = %d, want 1", lease.releases)
	}
}

func TestActiveRunFinishSurfacesDurableBudgetLeaseReleaseFailure(t *testing.T) {
	lease := &recordingBudgetLease{err: errors.New("open reservation")}
	managed := &activeRun{
		runtime:    &Runtime{hub: agentrun.NewHub(nil)},
		start:      agentapi.RunStart{RunID: "root-run"},
		budget:     lease,
		executed:   true,
		outcomeSet: true,
		outcome:    agentrun.Outcome{Status: agentrun.StatusDone},
	}
	if err := managed.Finish(nil); err == nil {
		t.Fatal("Finish hid durable lease release failure")
	}
	if lease.releases != 1 {
		t.Fatalf("lease releases = %d, want 1", lease.releases)
	}
}
