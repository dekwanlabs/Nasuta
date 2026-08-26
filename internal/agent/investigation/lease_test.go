package investigation

import (
	"context"
	"errors"
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

func TestMemoryLeaseStoreReportsHeldLease(t *testing.T) {
	store := NewMemoryLeaseStore()
	if _, err := store.AcquireLeaseWithToken(t.Context(), "run-held", "owner-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireLeaseWithToken(t.Context(), "run-held", "owner-b", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("acquire error = %v, want ErrLeaseHeld", err)
	}
}

func TestMemoryLeaseStoreStaleTokenCannotReleaseCurrentOwner(t *testing.T) {
	store := NewMemoryLeaseStore()
	first, err := store.AcquireLeaseWithToken(t.Context(), "run-release", "owner", 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := store.AcquireLeaseWithToken(t.Context(), "run-release", "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Token == first.Token {
		t.Fatalf("takeover token did not advance: first=%d second=%d", first.Token, second.Token)
	}
	if err := store.ReleaseLeaseWithToken(t.Context(), "run-release", "owner", first.Token); err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateLeaseWithToken(t.Context(), "run-release", "owner", second.Token); err != nil {
		t.Fatalf("stale release removed current lease: %v", err)
	}
}
