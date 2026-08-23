package investigation

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Lease is the authority returned to a worker. Token changes whenever an
// expired lease is taken over so durable writes can reject late workers.
type Lease struct {
	RunID     string
	Owner     string
	Token     uint64
	ExpiresAt time.Time
}

// LeaseStore is an optional cross-process fencing boundary. Implementations
// must make Acquire atomic with respect to concurrent callers.
type LeaseStore interface {
	AcquireLease(context.Context, string, string, time.Duration) error
	RenewLease(context.Context, string, string, time.Duration) error
	ReleaseLease(context.Context, string, string) error
}

// FencingLeaseStore extends the legacy lease API with a monotonic takeover
// token. Callers use it when the durable run store supports conditional writes.
type FencingLeaseStore interface {
	LeaseStore
	AcquireLeaseWithToken(context.Context, string, string, time.Duration) (Lease, error)
	RenewLeaseWithToken(context.Context, string, string, uint64, time.Duration) error
	ValidateLeaseWithToken(context.Context, string, string, uint64) error
}

// LeaseValidator lets a run store reject writes from a worker that no longer
// owns the lease. It is optional for legacy test doubles.
type LeaseValidator interface {
	ValidateLease(context.Context, string, string) error
}

type memoryLease struct {
	owner     string
	token     uint64
	expiresAt time.Time
}

// MemoryLeaseStore is used by tests and single-process deployments.
type MemoryLeaseStore struct {
	mu         sync.Mutex
	leases     map[string]memoryLease
	nextTokens map[string]uint64
}

// NewMemoryLeaseStore creates an in-process lease store.
func NewMemoryLeaseStore() *MemoryLeaseStore {
	return &MemoryLeaseStore{
		leases:     make(map[string]memoryLease),
		nextTokens: make(map[string]uint64),
	}
}

func (store *MemoryLeaseStore) AcquireLease(
	ctx context.Context,
	runID string,
	owner string,
	ttl time.Duration,
) error {
	_, err := store.AcquireLeaseWithToken(ctx, runID, owner, ttl)
	return err
}

func (store *MemoryLeaseStore) AcquireLeaseWithToken(
	_ context.Context,
	runID string,
	owner string, ttl time.Duration,
) (Lease, error) {
	if store == nil {
		return Lease{}, fmt.Errorf("lease store is required")
	}
	if runID == "" || owner == "" {
		return Lease{}, fmt.Errorf("lease run id and owner are required")
	}
	if ttl <= 0 {
		return Lease{}, fmt.Errorf("lease ttl must be positive")
	}
	now := time.Now().UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	current, exists := store.leases[runID]
	if exists && current.expiresAt.After(now) && current.owner != owner {
		return Lease{}, fmt.Errorf("run %q is already leased", runID)
	}
	token := current.token
	if !exists || !current.expiresAt.After(now) {
		token = store.nextTokens[runID] + 1
		if token == 0 {
			token = 1
		}
	}
	store.nextTokens[runID] = token
	next := memoryLease{owner: owner, token: token, expiresAt: now.Add(ttl)}
	store.leases[runID] = next
	return Lease{RunID: runID, Owner: owner, Token: token, ExpiresAt: next.expiresAt}, nil
}

func (store *MemoryLeaseStore) RenewLease(
	ctx context.Context,
	runID string,
	owner string, ttl time.Duration,
) error {
	if fencing, ok := any(store).(FencingLeaseStore); ok {
		store.mu.Lock()
		current, exists := store.leases[runID]
		store.mu.Unlock()
		if exists {
			return fencing.RenewLeaseWithToken(ctx, runID, owner, current.token, ttl)
		}
	}
	return store.renewLease(runID, owner, 0, ttl)
}

func (store *MemoryLeaseStore) RenewLeaseWithToken(
	_ context.Context,
	runID string,
	owner string,
	token uint64,
	ttl time.Duration,
) error {
	return store.renewLease(runID, owner, token, ttl)
}

func (store *MemoryLeaseStore) renewLease(
	runID string,
	owner string,
	token uint64, ttl time.Duration,
) error {
	if store == nil {
		return fmt.Errorf("lease store is required")
	}
	if ttl <= 0 {
		return fmt.Errorf("lease ttl must be positive")
	}
	now := time.Now().UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	current, ok := store.leases[runID]
	if !ok || current.owner != owner || (token > 0 && current.token != token) || !current.expiresAt.After(now) {
		return fmt.Errorf("run %q lease is not owned by %q", runID, owner)
	}
	store.leases[runID] = memoryLease{owner: owner, token: current.token, expiresAt: now.Add(ttl)}
	return nil
}

func (store *MemoryLeaseStore) ReleaseLease(
	_ context.Context,
	runID string,
	owner string,
) error {
	if store == nil {
		return fmt.Errorf("lease store is required")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	current, ok := store.leases[runID]
	if !ok || current.owner != owner {
		return nil
	}
	store.nextTokens[runID] = current.token
	delete(store.leases, runID)
	return nil
}

func (store *MemoryLeaseStore) ValidateLease(
	_ context.Context, runID string, owner string,
) error {
	if store == nil {
		return fmt.Errorf("lease store is required")
	}
	return store.validate(runID, owner, 0)
}

func (store *MemoryLeaseStore) ValidateLeaseWithToken(
	_ context.Context, runID string, owner string, token uint64,
) error {
	if store == nil {
		return fmt.Errorf("lease store is required")
	}
	return store.validate(runID, owner, token)
}

func (store *MemoryLeaseStore) validate(runID, owner string, token uint64) error {
	now := time.Now().UTC()
	store.mu.Lock()
	defer store.mu.Unlock()
	current, ok := store.leases[runID]
	if !ok || current.owner != owner || (token > 0 && current.token != token) || !current.expiresAt.After(now) {
		return fmt.Errorf("run %q lease is not owned by %q", runID, owner)
	}
	return nil
}
