package investigation

import (
	"errors"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

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

func TestAvailableForReservationIncludesExistingProtectionReserve(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{InputTokens: 100, OutputTokens: 100, TotalTokens: 200})
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := ledger.Reserve(StageComposition, "composition", BudgetVector{OutputTokens: 10, TotalTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	available := ledger.AvailableForReservation(reservation.ID)
	if available.InputTokens != 100 || available.OutputTokens != 90 || available.TotalTokens != 190 {
		t.Fatalf("available = %+v, want input=100 output=90 total=190", available)
	}
}
