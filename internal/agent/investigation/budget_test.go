package investigation

import (
	"errors"
	"sync"
	"testing"
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
