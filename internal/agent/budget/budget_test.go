package budget

import (
	"errors"
	"sync"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestRootReservesChildrenAndProtectsAnswerReserve(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 100, ParentAnswerReserve: 20})
	first, err := root.ReserveTask(agentapi.Usage{InputTokens: 20, OutputTokens: 30, TotalTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.ReserveTask(agentapi.Usage{InputTokens: 20, OutputTokens: 31, TotalTokens: 51}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("second task error = %v, want budget exceeded", err)
	}
	available := root.Available()
	if available.TotalTokens != 30 {
		t.Fatalf("default total availability = %d, want 30", available.TotalTokens)
	}
	if available.OutputTokens != 30 {
		t.Fatalf("default output availability = %d, want 30", available.OutputTokens)
	}
	answerAvailable := root.AvailableForPhase(agentapi.RunBudgetPhaseAnswer)
	if answerAvailable.TotalTokens != 50 {
		t.Fatalf("answer total availability = %d, want 50", answerAvailable.TotalTokens)
	}
	if first.Available().TotalTokens != 50 {
		t.Fatalf("child total availability = %d, want 50", first.Available().TotalTokens)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if got := root.Available().TotalTokens; got != 80 {
		t.Fatalf("released total availability = %d, want 80", got)
	}
}

func TestTaskCallSettlementReleasesUnusedGrantAndCountsActualUsage(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 100})
	taskReservation, err := root.ReserveTask(agentapi.Usage{
		InputTokens: 20, OutputTokens: 40, TotalTokens: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	call, err := taskReservation.ReserveCall(agentapi.Usage{
		InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := taskReservation.Available().TotalTokens; got != 30 {
		t.Fatalf("in-flight child availability = %d, want 30", got)
	}
	if err := call.Settle(agentapi.Usage{InputTokens: 8, OutputTokens: 12, TotalTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if got := root.Used().TotalTokens; got != 20 {
		t.Fatalf("root used total = %d, want 20", got)
	}
	if got := taskReservation.Available().TotalTokens; got != 40 {
		t.Fatalf("settled child availability = %d, want 40", got)
	}
	if err := taskReservation.Release(); err != nil {
		t.Fatal(err)
	}
	if got := root.Available().TotalTokens; got != 80 {
		t.Fatalf("post-release total availability = %d, want 80", got)
	}
}

func TestDirectCallMustLeaveAnswerReserveUntilAnswerPhase(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 100, ParentAnswerReserve: 25})
	if _, err := root.ReserveCall(agentapi.Usage{InputTokens: 70, OutputTokens: 6, TotalTokens: 76}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("default call error = %v, want budget exceeded", err)
	}
	call, err := root.ReserveCall(agentapi.Usage{InputTokens: 60, OutputTokens: 10, TotalTokens: 70})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Settle(agentapi.Usage{InputTokens: 60, OutputTokens: 10, TotalTokens: 70}); err != nil {
		t.Fatal(err)
	}
	if got := root.Available().TotalTokens; got != 5 {
		t.Fatalf("remaining default total = %d, want 5", got)
	}
	answerCall, err := root.ReserveCallForPhase(
		agentapi.Usage{InputTokens: 5, OutputTokens: 20, TotalTokens: 25},
		agentapi.RunBudgetPhaseAnswer,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := answerCall.Settle(agentapi.Usage{InputTokens: 5, OutputTokens: 20, TotalTokens: 25}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.ReserveCall(agentapi.Usage{TotalTokens: 6}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("default call after reserve error = %v, want budget exceeded", err)
	}
}

func TestUnlimitedDimensionsUseLargeAvailability(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{})
	available := root.Available()
	if available.InputTokens <= 0 || available.OutputTokens <= 0 || available.TotalTokens <= 0 || available.CostMicros <= 0 {
		t.Fatalf("unlimited availability = %+v", available)
	}
	call, err := root.ReserveCall(agentapi.Usage{InputTokens: 100, OutputTokens: 100, TotalTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestReportedUsageOverrunIsAccountedAndFails(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 100})
	call, err := root.ReserveCall(agentapi.Usage{InputTokens: 10, OutputTokens: 10, TotalTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Settle(agentapi.Usage{InputTokens: 20, OutputTokens: 20, TotalTokens: 40}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("overrun settle error = %v, want budget exceeded", err)
	}
	if got := root.Used().TotalTokens; got != 40 {
		t.Fatalf("overrun usage was not accounted: %d", got)
	}
}

func TestConcurrentReservationsDoNotOversubscribeRoot(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 100})
	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	attempted := 0
	admitted := 0
	reservations := make([]agentapi.RunBudgetTaskReservation, 0, workers)
	done := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reservation, err := root.ReserveTask(agentapi.Usage{TotalTokens: 10})
			mu.Lock()
			attempted++
			if err == nil {
				admitted++
				reservations = append(reservations, reservation)
			}
			if attempted == workers {
				close(done)
			}
			mu.Unlock()
		}()
	}
	<-done
	wg.Wait()
	if admitted != 10 {
		t.Fatalf("admitted = %d, want 10", admitted)
	}
	for _, reservation := range reservations {
		if err := reservation.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRootCheckDoesNotTreatAnswerReserveAsUsage(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 10, ParentAnswerReserve: 20})
	if err := root.Check(); err != nil {
		t.Fatalf("empty root check = %v, want nil", err)
	}
	if got := root.Available().TotalTokens; got != 0 {
		t.Fatalf("default availability = %d, want 0", got)
	}
	if got := root.AvailableForPhase(agentapi.RunBudgetPhaseAnswer).TotalTokens; got != 10 {
		t.Fatalf("answer availability = %d, want 10", got)
	}
}

func TestTaskReleaseRejectsInFlightCallAndIsIdempotent(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 100})
	task, err := root.ReserveTask(agentapi.Usage{TotalTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	call, err := task.ReserveCall(agentapi.Usage{TotalTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := task.Release(); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("release with in-flight call = %v, want budget exceeded", err)
	}
	if err := call.Release(); err != nil {
		t.Fatal(err)
	}
	if err := task.Release(); err != nil {
		t.Fatal(err)
	}
	if err := task.Release(); err != nil {
		t.Fatalf("second task release = %v, want nil", err)
	}
}

func TestCallSettlementAndReleaseAreIdempotent(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 100})
	call, err := root.ReserveCall(agentapi.Usage{InputTokens: 10, OutputTokens: 10, TotalTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	actual := agentapi.Usage{InputTokens: 5, OutputTokens: 10, TotalTokens: 15}
	if err := call.Settle(actual); err != nil {
		t.Fatal(err)
	}
	if err := call.Settle(actual); err != nil {
		t.Fatalf("same usage second settle = %v, want nil", err)
	}
	if err := call.Settle(agentapi.Usage{TotalTokens: 16}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("different usage second settle = %v, want budget exceeded", err)
	}
	if err := call.Release(); err != nil {
		t.Fatalf("release after settle = %v, want nil", err)
	}
	if got := root.Used().TotalTokens; got != actual.TotalTokens {
		t.Fatalf("settled usage = %d, want %d", got, actual.TotalTokens)
	}

	released, err := root.ReserveCall(agentapi.Usage{TotalTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := released.Release(); err != nil {
		t.Fatal(err)
	}
	if err := released.Release(); err != nil {
		t.Fatalf("second call release = %v, want nil", err)
	}
	if err := released.Settle(agentapi.Usage{TotalTokens: 10}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("settle after release = %v, want budget exceeded", err)
	}
}

func TestTaskGrantEnforcesEveryUsageDimension(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{
		MaxInputTokens: 100, MaxTotalTokens: 100, MaxCostMicros: 100,
	})
	task, err := root.ReserveTask(agentapi.Usage{
		InputTokens: 10, OutputTokens: 20, TotalTokens: 25, CostMicros: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		usage agentapi.Usage
	}{
		{name: "input", usage: agentapi.Usage{InputTokens: 11, TotalTokens: 11}},
		{name: "output", usage: agentapi.Usage{OutputTokens: 21, TotalTokens: 21}},
		{name: "total", usage: agentapi.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 26}},
		{name: "cost", usage: agentapi.Usage{TotalTokens: 1, CostMicros: 6}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := task.ReserveCall(tc.usage); !errors.Is(err, agentapi.ErrBudgetExceeded) {
				t.Fatalf("reserve error = %v, want budget exceeded", err)
			}
		})
	}
	call, err := task.ReserveCall(agentapi.Usage{
		InputTokens: 10, OutputTokens: 15, TotalTokens: 25, CostMicros: 5,
	})
	if err != nil {
		t.Fatalf("exact bounded call rejected: %v", err)
	}
	if err := call.Release(); err != nil {
		t.Fatal(err)
	}
	if err := task.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestRootEnforcesInputAndCostLimits(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{
		MaxInputTokens: 10, MaxTotalTokens: 100, MaxCostMicros: 5,
	})
	if _, err := root.ReserveCall(agentapi.Usage{InputTokens: 11, TotalTokens: 11}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("input limit error = %v, want budget exceeded", err)
	}
	if _, err := root.ReserveCall(agentapi.Usage{TotalTokens: 1, CostMicros: 6}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("cost limit error = %v, want budget exceeded", err)
	}
	call, err := root.ReserveCall(agentapi.Usage{
		InputTokens: 10, OutputTokens: 10, TotalTokens: 20, CostMicros: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Settle(agentapi.Usage{
		InputTokens: 10, OutputTokens: 10, TotalTokens: 20, CostMicros: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if err := root.Check(); err != nil {
		t.Fatalf("root check at exact limits = %v", err)
	}
	if _, err := root.ReserveCall(agentapi.Usage{TotalTokens: 1, CostMicros: 1}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("post-settlement cost error = %v, want budget exceeded", err)
	}
}

func TestConcurrentParentCallsAndChildGrantsDoNotOversubscribeRoot(t *testing.T) {
	type releaser interface {
		Release() error
	}

	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 100})
	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	reservations := make([]releaser, 0, workers)
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func(child bool) {
			defer wg.Done()
			var reservation releaser
			var err error
			if child {
				reservation, err = root.ReserveTask(agentapi.Usage{TotalTokens: 10})
			} else {
				reservation, err = root.ReserveCall(agentapi.Usage{TotalTokens: 10})
			}
			if err != nil {
				return
			}
			mu.Lock()
			admitted++
			reservations = append(reservations, reservation)
			mu.Unlock()
		}(index%2 == 0)
	}
	wg.Wait()
	if admitted != 10 {
		t.Fatalf("admitted = %d, want 10", admitted)
	}
	if err := root.Check(); err != nil {
		t.Fatalf("root check with mixed reservations = %v", err)
	}
	for _, reservation := range reservations {
		if err := reservation.Release(); err != nil {
			t.Fatal(err)
		}
	}
	if got := root.Available().TotalTokens; got != 100 {
		t.Fatalf("availability after release = %d, want 100", got)
	}
}

func TestChildUsageCannotConsumeParentAnswerReserve(t *testing.T) {
	root := NewRoot(agentapi.RunLimits{MaxTotalTokens: 100, ParentAnswerReserve: 20})
	if _, err := root.ReserveTask(agentapi.Usage{TotalTokens: 81}); !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("oversized child grant error = %v, want budget exceeded", err)
	}
	task, err := root.ReserveTask(agentapi.Usage{TotalTokens: 80})
	if err != nil {
		t.Fatal(err)
	}
	call, err := task.ReserveCall(agentapi.Usage{TotalTokens: 80})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Settle(agentapi.Usage{TotalTokens: 80}); err != nil {
		t.Fatal(err)
	}
	if got := root.Available().TotalTokens; got != 0 {
		t.Fatalf("default availability after child usage = %d, want 0", got)
	}
	if got := root.AvailableForPhase(agentapi.RunBudgetPhaseAnswer).TotalTokens; got != 20 {
		t.Fatalf("answer availability after child usage = %d, want 20", got)
	}
	if err := task.Release(); err != nil {
		t.Fatal(err)
	}
	answer, err := root.ReserveCallForPhase(
		agentapi.Usage{TotalTokens: 20},
		agentapi.RunBudgetPhaseAnswer,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := answer.Settle(agentapi.Usage{TotalTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if err := root.Check(); err != nil {
		t.Fatalf("root check after answer settlement = %v", err)
	}
}
