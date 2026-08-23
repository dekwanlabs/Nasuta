package investigation

import (
	"context"
	"testing"
	"time"
)

func TestMemoryLeaseTokenChangesOnTakeoverAndFencesRenewal(t *testing.T) {
	store := NewMemoryLeaseStore()
	first, err := store.AcquireLeaseWithToken(context.Background(), "run-token", "owner-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == 0 {
		t.Fatal("first lease token is zero")
	}
	if err := store.ReleaseLease(context.Background(), "run-token", "owner-a"); err != nil {
		t.Fatal(err)
	}
	second, err := store.AcquireLeaseWithToken(context.Background(), "run-token", "owner-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Token <= first.Token {
		t.Fatalf("takeover token = %d, first token = %d", second.Token, first.Token)
	}
	if err := store.RenewLeaseWithToken(context.Background(), "run-token", "owner-a", first.Token, time.Minute); err == nil {
		t.Fatal("stale owner renewal succeeded")
	}
	if err := store.ValidateLeaseWithToken(context.Background(), "run-token", "owner-b", second.Token); err != nil {
		t.Fatalf("current owner validation failed: %v", err)
	}
}
