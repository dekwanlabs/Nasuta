package investigation

import (
	"errors"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestBudgetRestoreDropsStaleReservations(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100, OutputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{InputTokens: 80, OutputTokens: 80}); err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(StageExecution, "dead-process", BudgetVector{InputTokens: 60, OutputTokens: 40})
	if err != nil {
		t.Fatal(err)
	}
	call, err := ledger.ReserveCall(reservation.ID, BudgetVector{InputTokens: 30, OutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Settle(agentapi.Usage{InputTokens: 25, OutputTokens: 15}); err != nil {
		t.Fatal(err)
	}
	snapshot := ledger.Snapshot()

	restored, err := NewBudgetLedger(snapshot.Run.Limit)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	got := restored.Snapshot()
	if got.Run.Reserved != (BudgetVector{}) {
		t.Fatalf("stale run reservation survived restore: %+v", got.Run.Reserved)
	}
	if got.Stages[StageExecution].Reserved != (BudgetVector{}) {
		t.Fatalf("stale stage reservation survived restore: %+v", got.Stages[StageExecution].Reserved)
	}
	if _, err := restored.ReserveAdmission(StageExecution, "resumed-task", BudgetVector{}); err != nil {
		t.Fatalf("stale reservation blocked resumed task: %v", err)
	}
}

func TestBudgetReserveIncludesOutstandingGrant(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{ToolCalls: 6}); err != nil {
		t.Fatal(err)
	}

	first, err := ledger.Reserve(StageExecution, "first", BudgetVector{ToolCalls: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Reserve(StageExecution, "second", BudgetVector{ToolCalls: 3}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("second reservation error = %v, want ErrBudgetExceeded", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Snapshot().Run.Reserved.ToolCalls; got != 0 {
		t.Fatalf("reserved tool calls after release = %d", got)
	}
}

func TestBudgetConcurrentReserveDoesNotOvercommit(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 10})
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 32
	results := make(chan BudgetReservation, attempts)
	errorsCh := make(chan error, attempts)
	var group sync.WaitGroup
	for index := 0; index < attempts; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			reservation, reserveErr := ledger.Reserve(StageExecution, taskID(index), BudgetVector{ToolCalls: 1})
			if reserveErr != nil {
				errorsCh <- reserveErr
				return
			}
			results <- reservation
		}()
	}
	group.Wait()
	close(results)
	close(errorsCh)

	reservations := make([]BudgetReservation, 0, 10)
	for reservation := range results {
		reservations = append(reservations, reservation)
	}
	if len(reservations) != 10 {
		t.Fatalf("successful reservations = %d, want 10", len(reservations))
	}
	for _, reservation := range reservations {
		if err := reservation.Release(); err != nil {
			t.Fatal(err)
		}
	}
	for reserveErr := range errorsCh {
		if !errors.Is(reserveErr, ErrBudgetExceeded) {
			t.Fatalf("unexpected reserve error = %v", reserveErr)
		}
	}
	if snapshot := ledger.Snapshot(); snapshot.Run.Reserved.ToolCalls != 0 || snapshot.Run.Used.ToolCalls != 0 {
		t.Fatalf("budget after release = %#v", snapshot.Run)
	}
}

func TestBudgetSettleRejectsUsageAboveGrantAndCanRelease(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 3})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(StageExecution, "task", BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(BudgetVector{ToolCalls: 3}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("oversized settle error = %v, want ErrBudgetExceeded", err)
	}
	if got := ledger.Snapshot().Run.Reserved.ToolCalls; got != 2 {
		t.Fatalf("reservation was lost after rejected settle: %d", got)
	}
	if err := reservation.Release(); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Release(); err == nil {
		t.Fatal("second release unexpectedly succeeded")
	}
}

func taskID(index int) string {
	return "task_" + string(rune('a'+index))
}

func TestBudgetTracksExplicitTotalTokens(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 10, OutputTokens: 10, TotalTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(StageExecution, "tokens", BudgetVector{TotalTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(BudgetVector{InputTokens: 4, OutputTokens: 3, TotalTokens: 7}); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Snapshot().Run.Used.TotalTokens; got != 7 {
		t.Fatalf("used total tokens = %d, want 7", got)
	}
}

func TestBudgetCallReservationReplacesEstimateWithActualUsage(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100, OutputTokens: 20, CostMicros: 100})
	if err != nil {
		t.Fatal(err)
	}
	task, err := ledger.ReserveAdmission(StageExecution, "task", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	call, err := ledger.ReserveCall(task.ID, BudgetVector{InputTokens: 40, OutputTokens: 15, CostMicros: 60})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Reserved.InputTokens != 40 || snapshot.Run.Reserved.OutputTokens != 15 || snapshot.Run.Reserved.CostMicros != 60 {
		t.Fatalf("reserved usage after call admission = %#v", snapshot.Run.Reserved)
	}
	if err := call.Settle(agentapi.Usage{InputTokens: 60, OutputTokens: 5, CostMicros: 20}); err != nil {
		t.Fatal(err)
	}
	snapshot = ledger.Snapshot()
	if snapshot.Run.Reserved.InputTokens != 60 || snapshot.Run.Reserved.OutputTokens != 5 || snapshot.Run.Reserved.CostMicros != 20 {
		t.Fatalf("reserved usage after call settle = %#v", snapshot.Run.Reserved)
	}
	if err := task.Settle(BudgetVector{InputTokens: 60, OutputTokens: 5, CostMicros: 20}); err != nil {
		t.Fatal(err)
	}
	if snapshot := ledger.Snapshot(); snapshot.Run.Reserved != (BudgetVector{}) || snapshot.Run.Used.InputTokens != 60 {
		t.Fatalf("final budget snapshot = %#v", snapshot.Run)
	}
}

func TestBudgetCallReservationUsesOuterGrantWithoutDoubleReservation(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100, OutputTokens: 100, TotalTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	task, err := ledger.Reserve(StageExecution, "hard-task", BudgetVector{
		InputTokens: 60, OutputTokens: 30, TotalTokens: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	if available := ledger.AvailableForReservation(task.ID); available != (BudgetVector{
		InputTokens: 60, OutputTokens: 30, TotalTokens: 90,
	}) {
		t.Fatalf("available inside outer grant = %+v", available)
	}
	call, err := ledger.ReserveCall(task.ID, BudgetVector{
		InputTokens: 50, OutputTokens: 20, TotalTokens: 70,
	})
	if err != nil {
		t.Fatalf("reserve call covered by outer grant: %v", err)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Reserved != (BudgetVector{InputTokens: 60, OutputTokens: 30, TotalTokens: 90}) {
		t.Fatalf("nested call double-reserved outer grant: %+v", snapshot.Run.Reserved)
	}
	if err := call.Settle(agentapi.Usage{InputTokens: 40, OutputTokens: 15, TotalTokens: 55}); err != nil {
		t.Fatal(err)
	}
	if snapshot := ledger.Snapshot(); snapshot.Run.Reserved != (BudgetVector{
		InputTokens: 60, OutputTokens: 30, TotalTokens: 90,
	}) {
		t.Fatalf("settled call changed covered reservation: %+v", snapshot.Run.Reserved)
	}
	if err := task.Settle(BudgetVector{InputTokens: 40, OutputTokens: 15, TotalTokens: 55}); err != nil {
		t.Fatal(err)
	}
	snapshot = ledger.Snapshot()
	if snapshot.Run.Reserved != (BudgetVector{}) || snapshot.Run.Used != (BudgetVector{
		InputTokens: 40, OutputTokens: 15, TotalTokens: 55,
	}) {
		t.Fatalf("final accounting = %+v", snapshot.Run)
	}
}

func TestBudgetMultipleCallsShareOneOuterGrant(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 200, OutputTokens: 100, TotalTokens: 300})
	if err != nil {
		t.Fatal(err)
	}
	task, err := ledger.Reserve(StageExecution, "multi-call", BudgetVector{
		InputTokens: 70, OutputTokens: 40, TotalTokens: 110,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ledger.ReserveCall(task.ID, BudgetVector{InputTokens: 30, OutputTokens: 20, TotalTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.ReserveCall(task.ID, BudgetVector{InputTokens: 30, OutputTokens: 15, TotalTokens: 45})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := ledger.Snapshot(); snapshot.Run.Reserved != (BudgetVector{
		InputTokens: 70, OutputTokens: 40, TotalTokens: 110,
	}) {
		t.Fatalf("parallel call reservations = %+v", snapshot.Run.Reserved)
	}
	if _, err := ledger.ReserveCall(task.ID, BudgetVector{InputTokens: 11}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("call beyond task grant = %v, want ErrBudgetExceeded", err)
	}
	if err := first.Settle(agentapi.Usage{InputTokens: 25, OutputTokens: 15, TotalTokens: 40}); err != nil {
		t.Fatal(err)
	}
	if err := second.Settle(agentapi.Usage{InputTokens: 35, OutputTokens: 10, TotalTokens: 45}); err != nil {
		t.Fatal(err)
	}
	if available := ledger.AvailableForReservation(task.ID); available != (BudgetVector{
		InputTokens: 10, OutputTokens: 15, TotalTokens: 25,
	}) {
		t.Fatalf("remaining outer grant = %+v", available)
	}
	if err := task.Settle(BudgetVector{InputTokens: 60, OutputTokens: 25, TotalTokens: 85}); err != nil {
		t.Fatal(err)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Reserved != (BudgetVector{}) || snapshot.Run.Used != (BudgetVector{
		InputTokens: 60, OutputTokens: 25, TotalTokens: 85,
	}) {
		t.Fatalf("multi-call final accounting = %+v", snapshot.Run)
	}
}

func TestBudgetOuterSettleCannotUndercountSettledCalls(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100, OutputTokens: 100, TotalTokens: 200, ToolCalls: 2, CostMicros: 100})
	if err != nil {
		t.Fatal(err)
	}
	task, err := ledger.ReserveAdmission(StageExecution, "underreported-task", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	call, err := ledger.ReserveCall(task.ID, BudgetVector{InputTokens: 40, OutputTokens: 20, TotalTokens: 60, CostMicros: 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Settle(agentapi.Usage{InputTokens: 35, OutputTokens: 15, TotalTokens: 50, CostMicros: 25}); err != nil {
		t.Fatal(err)
	}
	if err := task.Settle(BudgetVector{ToolCalls: 2}); err != nil {
		t.Fatal(err)
	}

	used := ledger.Snapshot().Run.Used
	want := (BudgetVector{InputTokens: 35, OutputTokens: 15, TotalTokens: 50, ToolCalls: 2, CostMicros: 25})
	if used != want {
		t.Fatalf("run usage = %+v, want settled call floor %+v", used, want)
	}
}

func TestBudgetSoftSettlementRecordsUnavoidableOverrun(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	task, err := ledger.ReserveAdmission(StageExecution, "overrun-task", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	call, err := ledger.ReserveCall(task.ID, BudgetVector{InputTokens: 40})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Settle(agentapi.Usage{InputTokens: 60}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("call overrun error = %v, want ErrBudgetExceeded", err)
	}
	if err := task.Settle(BudgetVector{}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("task overrun error = %v, want ErrBudgetExceeded", err)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Used.InputTokens != 60 || snapshot.Run.Reserved != (BudgetVector{}) {
		t.Fatalf("overrun accounting = %+v, want used=60 reserved=0", snapshot.Run)
	}
	if snapshot.Stages[StageExecution].Used.InputTokens != 60 || snapshot.Stages[StageExecution].Reserved != (BudgetVector{}) {
		t.Fatalf("stage overrun accounting = %+v", snapshot.Stages[StageExecution])
	}
}

func TestBudgetReleasedCallKeepsOuterGrantReserved(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	task, err := ledger.Reserve(StageExecution, "released-call", BudgetVector{InputTokens: 60})
	if err != nil {
		t.Fatal(err)
	}
	call, err := ledger.ReserveCall(task.ID, BudgetVector{InputTokens: 50})
	if err != nil {
		t.Fatal(err)
	}
	if err := call.Release(); err != nil {
		t.Fatal(err)
	}
	if snapshot := ledger.Snapshot(); snapshot.Run.Reserved.InputTokens != 60 {
		t.Fatalf("outer grant after call release = %+v", snapshot.Run.Reserved)
	}
	if err := task.Release(); err != nil {
		t.Fatal(err)
	}
	if snapshot := ledger.Snapshot(); snapshot.Run.Reserved != (BudgetVector{}) {
		t.Fatalf("reservation leaked after task release = %+v", snapshot.Run.Reserved)
	}
}

func TestBudgetCallReservationMakesInFlightUsageVisibleToSiblings(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ledger.ReserveAdmission(StageExecution, "first", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := ledger.ReserveAdmission(StageExecution, "second", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	firstCall, err := ledger.ReserveCall(first.ID, BudgetVector{InputTokens: 80})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.ReserveCall(second.ID, BudgetVector{InputTokens: 30}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("second in-flight call error = %v, want ErrBudgetExceeded", err)
	}
	if err := firstCall.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetReallocateAvailableTransfersOnlyUnusedBoundedCapacity(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{
		InputTokens: 100, OutputTokens: 100, ToolCalls: 10,
		Duration: 100 * time.Second, CostMicros: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StagePlanning, BudgetVector{
		InputTokens: 10, OutputTokens: 10, ToolCalls: 2,
		Duration: 10 * time.Second, CostMicros: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{
		InputTokens: 70, OutputTokens: 70, ToolCalls: 7,
		Duration: 70 * time.Second, CostMicros: 70,
	}); err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(StagePlanning, "plan", BudgetVector{
		InputTokens: 3, OutputTokens: 4, ToolCalls: 1,
		Duration: 3 * time.Second, CostMicros: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(BudgetVector{
		InputTokens: 3, OutputTokens: 4, ToolCalls: 1,
		Duration: 3 * time.Second, CostMicros: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.reallocateAvailable(StagePlanning, StageExecution); err != nil {
		t.Fatal(err)
	}
	snapshot := ledger.Snapshot()
	planning := snapshot.Stages[StagePlanning].Limit
	if planning != (BudgetVector{
		InputTokens: 3, OutputTokens: 4, ToolCalls: 1,
		Duration: 3 * time.Second, CostMicros: 5,
	}) {
		t.Fatalf("planning limit = %#v", planning)
	}
	execution := snapshot.Stages[StageExecution].Limit
	if execution != (BudgetVector{
		InputTokens: 77, OutputTokens: 76, ToolCalls: 8,
		Duration: 77 * time.Second, CostMicros: 75,
	}) {
		t.Fatalf("execution limit = %#v", execution)
	}
}

func TestBudgetZeroTaskDimensionStillUsesRunHardLimit(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 10, ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(StageExecution, "unprofiled", BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(BudgetVector{InputTokens: 10, ToolCalls: 1}); err != nil {
		t.Fatal(err)
	}

	second, err := ledger.Reserve(StageExecution, "second", BudgetVector{ToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Settle(BudgetVector{InputTokens: 1, ToolCalls: 1}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("over-limit unprofiled settle error = %v, want ErrBudgetExceeded", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAvailableForReservationIncludesOwnHardGrant(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100, OutputTokens: 100, TotalTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(StageComposition, "composition", BudgetVector{OutputTokens: 10, TotalTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	available := ledger.AvailableForReservation(reservation.ID)
	if available.InputTokens != 100 || available.OutputTokens != 10 || available.TotalTokens != 10 {
		t.Fatalf("available = %+v, want input=100 output=10 total=10", available)
	}
}

func TestBudgetAdmissionUsesRunLimitWhenStageAllocationIsSmaller(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 30, ToolCalls: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{OutputTokens: 10, ToolCalls: 3}); err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.ReserveAdmission(StageExecution, "agent", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	if available := ledger.AvailableForReservation(reservation.ID); available.OutputTokens != 30 {
		t.Fatalf("admission output available = %d, want shared run capacity", available.OutputTokens)
	}
	if err := reservation.Settle(BudgetVector{OutputTokens: 20, ToolCalls: 4}); err != nil {
		t.Fatalf("soft admission settle = %v, want stage overrun accepted within run limit", err)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Run.Used.OutputTokens != 20 || snapshot.Run.Used.ToolCalls != 4 {
		t.Fatalf("run usage = %+v", snapshot.Run.Used)
	}
	if snapshot.Stages[StageExecution].Used.OutputTokens != 20 || snapshot.Stages[StageExecution].Used.ToolCalls != 4 {
		t.Fatalf("execution usage = %+v", snapshot.Stages[StageExecution].Used)
	}
}

func TestBudgetAdmissionCallUsesRunLimitWhenStageAllocationIsSmaller(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{InputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.ReserveAdmission(StageExecution, "agent-call", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	call, err := ledger.ReserveCall(reservation.ID, BudgetVector{InputTokens: 20})
	if err != nil {
		t.Fatalf("soft call admission = %v, want shared run capacity", err)
	}
	if err := call.Settle(agentapi.Usage{InputTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(BudgetVector{InputTokens: 20}); err != nil {
		t.Fatal(err)
	}
}

func TestBudgetAdmissionRemainsAvailableAfterStageOverrun(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{OutputTokens: 40})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetStageLimit(StageExecution, BudgetVector{OutputTokens: 10}); err != nil {
		t.Fatal(err)
	}

	first, err := ledger.ReserveAdmission(StageExecution, "first-agent", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Settle(BudgetVector{OutputTokens: 20}); err != nil {
		t.Fatalf("first admission settle = %v", err)
	}

	second, err := ledger.ReserveAdmission(StageExecution, "second-agent", BudgetVector{})
	if err != nil {
		t.Fatalf("second admission after stage overrun = %v", err)
	}
	if err := second.Settle(BudgetVector{OutputTokens: 10}); err != nil {
		t.Fatalf("second admission settle = %v", err)
	}

	snapshot := ledger.Snapshot()
	if snapshot.Run.Used.OutputTokens != 30 || snapshot.Run.Reserved.OutputTokens != 0 {
		t.Fatalf("run accounting = %+v, want used=30 reserved=0", snapshot.Run)
	}
	if snapshot.Stages[StageExecution].Used.OutputTokens != 30 {
		t.Fatalf("execution usage = %+v, want output=30", snapshot.Stages[StageExecution].Used)
	}
	if _, err := ledger.Reserve(StageExecution, "hard-after-overrun", BudgetVector{}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("hard reservation after stage overrun = %v, want ErrBudgetExceeded", err)
	}
}

func TestBudgetPreflightRejectsAtPositiveLimit(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  30,
		ToolCalls:    2,
		Duration:     time.Second,
		CostMicros:   100,
	})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.ReserveAdmission(StageExecution, "spent", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	gateReservation, err := ledger.ReserveAdmission(StageExecution, "gate", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(BudgetVector{
		InputTokens:  10,
		OutputTokens: 20,
		TotalTokens:  30,
		ToolCalls:    2,
		Duration:     time.Second,
		CostMicros:   100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Check(); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("run preflight error = %v, want ErrBudgetExceeded", err)
	}
	if err := (reservationBudgetGate{ledger: ledger, id: gateReservation.ID}).Check(); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("reservation preflight error = %v, want ErrBudgetExceeded", err)
	}
}

func TestBudgetPreflightLeavesZeroLimitsUnbounded(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{CostMicros: 0})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.ReserveAdmission(StageExecution, "unbounded", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(BudgetVector{CostMicros: 100}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Check(); err != nil {
		t.Fatalf("zero cost limit rejected preflight: %v", err)
	}
}

func TestBudgetPreflightFallsBackToComponentTokens(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100, OutputTokens: 100, TotalTokens: 30})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.ReserveAdmission(StageExecution, "components", BudgetVector{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reservation.Settle(BudgetVector{InputTokens: 10, OutputTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Check(); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("component-token preflight error = %v, want ErrBudgetExceeded", err)
	}
}

func TestBudgetAdmissionProtectsVerifierAcrossTokenDimensions(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{
		InputTokens:  1000,
		OutputTokens: 100,
		TotalTokens:  1100,
	})
	if err != nil {
		t.Fatal(err)
	}

	verifier, err := ledger.ReserveAdmission(StageVerification, "verifier", BudgetVector{
		InputTokens:  400,
		OutputTokens: 40,
		TotalTokens:  440,
	})
	if err != nil {
		t.Fatalf("reserve verifier: %v", err)
	}

	// Investigator admission may use the capacity that remains after the
	// verifier floor, but cannot reserve any of the verifier's protected
	// input/output/total dimensions.
	investigator, err := ledger.ReserveAdmission(StageExecution, "investigator", BudgetVector{
		InputTokens:  600,
		OutputTokens: 60,
		TotalTokens:  660,
	})
	if err != nil {
		t.Fatalf("reserve investigator in remaining capacity: %v", err)
	}
	if _, err := ledger.ReserveAdmission(StageExecution, "investigator-overrun", BudgetVector{
		InputTokens: 1,
	}); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("investigator overrun reservation error = %v, want ErrBudgetExceeded", err)
	}

	// The verifier can still reserve its complete physical call after the
	// investigator has occupied every unprotected token. Its own grant is
	// excluded from the projected shared usage; a sibling cannot do the same.
	verifierCall, err := ledger.ReserveCall(verifier.ID, BudgetVector{
		InputTokens:  400,
		OutputTokens: 40,
		TotalTokens:  440,
	})
	if err != nil {
		t.Fatalf("reserve verifier protected call: %v", err)
	}
	if err := verifierCall.Settle(agentapi.Usage{
		InputTokens:  400,
		OutputTokens: 40,
		TotalTokens:  440,
	}); err != nil {
		t.Fatalf("settle verifier protected call: %v", err)
	}
	if err := verifier.Settle(BudgetVector{
		InputTokens:  400,
		OutputTokens: 40,
		TotalTokens:  440,
	}); err != nil {
		t.Fatalf("settle verifier reservation: %v", err)
	}
	if err := investigator.Release(); err != nil {
		t.Fatalf("release investigator reservation: %v", err)
	}

	snapshot := ledger.Snapshot()
	if snapshot.Run.Used != (BudgetVector{
		InputTokens:  400,
		OutputTokens: 40,
		TotalTokens:  440,
	}) {
		t.Fatalf("run usage after verifier settlement = %+v", snapshot.Run.Used)
	}
	if snapshot.Run.Reserved != (BudgetVector{}) {
		t.Fatalf("run reservations leaked = %+v", snapshot.Run.Reserved)
	}
}
