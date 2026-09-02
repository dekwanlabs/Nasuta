package budget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// DurableBackend is the persistence boundary for a Root budget. Implementations
// must make Reserve, Settle and Release atomic with the root ledger so multiple
// processes cannot admit more work than the configured limits.
//
// The interface intentionally has no context parameter because the public
// RunBudget interfaces are synchronous. Backends should use a bounded,
// cancellation-independent context for accounting writes: a cancelled model
// request must not leave a reservation permanently open.
type DurableBackend interface {
	EnsureRoot(rootRunID string, limits agentapi.RunLimits) error
	RootSnapshot(rootRunID string) (DurableRootSnapshot, error)
	TaskSnapshot(rootRunID, reservationID string) (DurableTaskSnapshot, error)
	Reserve(rootRunID string, reservation DurableReservation) error
	SettleCall(rootRunID, reservationID string, actual agentapi.Usage) error
	ReleaseCall(rootRunID, reservationID string) error
	ReleaseTask(rootRunID, reservationID string) error
}

// DurableReservation is one persisted root, task or physical-call admission.
type DurableReservation struct {
	ID       string
	ParentID string
	Kind     string
	Phase    agentapi.RunBudgetPhase
	Estimate agentapi.Usage
	Grant    agentapi.Usage
}

// DurableRootSnapshot is the authoritative persisted root view.
type DurableRootSnapshot struct {
	Limits   agentapi.RunLimits
	Used     agentapi.Usage
	Reserved agentapi.Usage
}

// DurableTaskSnapshot is the authoritative persisted task view.
type DurableTaskSnapshot struct {
	Grant    agentapi.Usage
	Used     agentapi.Usage
	InFlight agentapi.Usage
	Released bool
}

// SettlementError reports whether a durable settlement was committed before
// an accounting error was returned. This lets callers safely release a
// reservation after a pre-commit failure without accidentally treating a
// committed overrun as still open.
type SettlementError struct {
	Err       error
	Committed bool
}

func (err *SettlementError) Error() string {
	if err == nil || err.Err == nil {
		return "durable budget settlement error"
	}
	return err.Err.Error()
}

func (err *SettlementError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

// DurableRoot is a process-independent implementation of the hierarchical
// budget interfaces. It keeps no accounting state locally; every admission and
// settlement is checked against the backend's locked ledger.
type DurableRoot struct {
	backend DurableBackend
	rootID  string
	limits  agentapi.RunLimits

	leaseMu         sync.Mutex
	leaseOwner      string
	leaseTTL        time.Duration
	fenceToken      int64
	leaseLost       error
	heartbeatCancel context.CancelFunc
}

// DurableTask is one persisted logical child grant. Retries should retain this
// handle instead of creating another task grant.
type DurableTask struct {
	backend DurableBackend
	rootID  string
	id      string
	owner   string
	fence   int64
}

type durableCallReservation struct {
	mu sync.Mutex

	backend DurableBackend
	rootID  string
	id      string
	owner   string
	fence   int64
	actual  agentapi.Usage
	state   reservationState
}

var durableReservationSeq atomic.Uint64

var (
	// ErrLeaseHeld means another live process owns the durable root lease.
	ErrLeaseHeld = errors.New("durable budget lease held by another owner")
	// ErrLeaseOwnerMismatch means a lease mutation was attempted by a non-owner.
	ErrLeaseOwnerMismatch = errors.New("durable budget lease owner mismatch")
	// ErrLeaseNotActive means the requested lease has already expired.
	ErrLeaseNotActive = errors.New("durable budget lease is not active")
	// ErrLeaseHasReservations prevents a normal lease release from hiding open work.
	ErrLeaseHasReservations = errors.New("durable budget lease still has active reservations")
	// ErrLeaseLost means this process no longer owns the fenced root lease.
	ErrLeaseLost = errors.New("durable budget lease was lost")
)

// DurableLeaseBackend is an optional extension used by production backends.
// Keeping it separate from DurableBackend preserves compatibility with light
// test backends while allowing root ownership and crash reclamation to evolve
// independently from reservation accounting.
type DurableLeaseBackend interface {
	AcquireLease(rootRunID, owner string, now time.Time, ttl time.Duration) error
	RenewLease(rootRunID, owner string, now time.Time, ttl time.Duration) error
	ReleaseLease(rootRunID, owner string, now time.Time) error
	ReclaimExpired(rootRunID string, now time.Time) error
}

// DurableLeaseTokenBackend extends lease ownership with a monotonically
// increasing fencing token. Every writer using a token is rejected after a
// lease is re-acquired by another owner, even if the old process is merely
// paused rather than cleanly stopped.
type DurableLeaseTokenBackend interface {
	DurableLeaseBackend
	AcquireLeaseWithFence(rootRunID, owner string, now time.Time, ttl time.Duration) (int64, error)
	RenewLeaseWithFence(rootRunID, owner string, fence int64, now time.Time, ttl time.Duration) error
	ReleaseLeaseWithFence(rootRunID, owner string, fence int64, now time.Time) error
	ReserveFenced(rootRunID, owner string, fence int64, reservation DurableReservation) error
	SettleCallFenced(rootRunID, owner string, fence int64, reservationID string, actual agentapi.Usage) error
	ReleaseCallFenced(rootRunID, owner string, fence int64, reservationID string) error
	ReleaseTaskFenced(rootRunID, owner string, fence int64, reservationID string) error
}

// DurableFencingCapability lets test/light backends opt out while production
// stores explicitly advertise that the fencing columns are installed.
type DurableFencingCapability interface{ FencingEnabled() bool }

func fencingEnabled(backend any) bool {
	if capability, ok := backend.(DurableFencingCapability); ok {
		return capability.FencingEnabled()
	}
	return true
}

// NewDurableRoot ensures the root ledger exists and returns a gate backed by it.
func NewDurableRoot(backend DurableBackend, rootRunID string, limits agentapi.RunLimits) (*DurableRoot, error) {
	if backend == nil {
		return nil, fmt.Errorf("durable budget backend is required")
	}
	if rootRunID == "" {
		return nil, fmt.Errorf("durable budget root run id is required")
	}
	if err := backend.EnsureRoot(rootRunID, limits); err != nil {
		return nil, fmt.Errorf("ensure durable budget root %q: %w", rootRunID, err)
	}
	return &DurableRoot{backend: backend, rootID: rootRunID, limits: limits}, nil
}

// NewDurableRootWithLease attaches a lease already created in the same
// transaction as the caller's domain record. It performs no persistence.
func NewDurableRootWithLease(backend DurableBackend, rootRunID string, limits agentapi.RunLimits, owner string, fence int64, ttl time.Duration) (*DurableRoot, error) {
	return NewDurableRootWithLeaseContext(context.Background(), backend, rootRunID, limits, owner, fence, ttl)
}

// NewDurableRootWithLeaseContext attaches a lease created by the caller's
// transaction and binds the heartbeat to the logical run context. The context
// is part of the ownership lifecycle: cancellation stops renewals even when a
// caller forgets to invoke ReleaseLease after a panic or transport failure.
func NewDurableRootWithLeaseContext(ctx context.Context, backend DurableBackend, rootRunID string, limits agentapi.RunLimits, owner string, fence int64, ttl time.Duration) (*DurableRoot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if backend == nil || rootRunID == "" || strings.TrimSpace(owner) == "" || fence <= 0 || ttl <= 0 {
		return nil, fmt.Errorf("invalid durable root lease attachment")
	}
	root := &DurableRoot{backend: backend, rootID: rootRunID, limits: limits, leaseOwner: owner, fenceToken: fence, leaseTTL: ttl}
	root.StartHeartbeat(ctx)
	return root, nil
}

// AcquireLease claims the durable root for one process owner. If the prior
// owner expired, the backend reclaims all open physical calls and active task
// grants in the same transaction before admitting the new owner.
func (root *DurableRoot) AcquireLease(owner string, now time.Time, ttl time.Duration) error {
	return root.AcquireLeaseContext(context.Background(), owner, now, ttl)
}

// AcquireLeaseContext claims the root and binds its heartbeat to ctx.
// Cancellation stops renewals, while the persisted lease remains reclaimable.
func (root *DurableRoot) AcquireLeaseContext(ctx context.Context, owner string, now time.Time, ttl time.Duration) error {
	if root == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("durable budget lease owner is required")
	}
	if ttl <= 0 {
		return fmt.Errorf("durable budget lease ttl must be positive")
	}
	backend, ok := root.backend.(DurableLeaseBackend)
	if !ok {
		return fmt.Errorf("durable budget backend does not support leases")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var fence int64
	var err error
	if fenced, ok := root.backend.(DurableLeaseTokenBackend); ok && fencingEnabled(root.backend) {
		fence, err = fenced.AcquireLeaseWithFence(root.rootID, owner, now.UTC(), ttl)
	} else {
		err = backend.AcquireLease(root.rootID, owner, now.UTC(), ttl)
	}
	if err != nil {
		return err
	}
	root.leaseMu.Lock()
	root.leaseOwner = owner
	root.leaseTTL = ttl
	root.fenceToken = fence
	root.leaseLost = nil
	root.leaseMu.Unlock()
	if fence > 0 {
		root.StartHeartbeat(ctx)
	}
	return nil
}

// RenewLease extends a lease previously acquired through this root handle.
func (root *DurableRoot) RenewLease(now time.Time) error {
	if root == nil {
		return nil
	}
	root.leaseMu.Lock()
	owner, ttl, fence, lost := root.leaseOwner, root.leaseTTL, root.fenceToken, root.leaseLost
	root.leaseMu.Unlock()
	if lost != nil {
		return lost
	}
	if owner == "" || ttl <= 0 {
		return ErrLeaseNotActive
	}
	backend, ok := root.backend.(DurableLeaseBackend)
	if !ok {
		return fmt.Errorf("durable budget backend does not support leases")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if fenced, ok := root.backend.(DurableLeaseTokenBackend); ok && fencingEnabled(root.backend) && fence > 0 {
		return fenced.RenewLeaseWithFence(root.rootID, owner, fence, now.UTC(), ttl)
	}
	return backend.RenewLease(root.rootID, owner, now.UTC(), ttl)
}

// Close stops local lease heartbeats without attempting a database release.
// It is useful on cancellation/crash paths where the lease must expire and be
// reclaimed by another owner rather than being released while reservations are
// still open.
func (root *DurableRoot) Close() {
	if root == nil {
		return
	}
	root.leaseMu.Lock()
	cancel := root.heartbeatCancel
	root.heartbeatCancel = nil
	root.leaseMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ReleaseLease relinquishes a normally completed root. The backend rejects
// release while reservations remain, leaving the lease to expire so reclaim
// can close them safely instead of silently losing accounting state.
func (root *DurableRoot) ReleaseLease() error {
	if root == nil {
		return nil
	}
	root.leaseMu.Lock()
	owner, fence, cancel := root.leaseOwner, root.fenceToken, root.heartbeatCancel
	root.heartbeatCancel = nil
	root.leaseMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if owner == "" {
		return nil
	}
	backend, ok := root.backend.(DurableLeaseBackend)
	if !ok {
		return fmt.Errorf("durable budget backend does not support leases")
	}
	var err error
	if fenced, ok := root.backend.(DurableLeaseTokenBackend); ok && fencingEnabled(root.backend) && fence > 0 {
		err = fenced.ReleaseLeaseWithFence(root.rootID, owner, fence, time.Now().UTC())
	} else {
		err = backend.ReleaseLease(root.rootID, owner, time.Now().UTC())
	}
	if err != nil {
		return err
	}
	root.leaseMu.Lock()
	if root.leaseOwner == owner {
		root.leaseOwner, root.leaseTTL, root.fenceToken, root.leaseLost = "", 0, 0, nil
	}
	root.leaseMu.Unlock()
	return nil
}

// StartHeartbeat renews the lease at roughly one third of its TTL. A failed
// renewal permanently fences this handle; callers must stop writing and let a
// later owner reclaim the expired lease rather than silently continuing.
func (root *DurableRoot) StartHeartbeat(ctx context.Context) {
	if root == nil {
		return
	}
	root.leaseMu.Lock()
	if root.heartbeatCancel != nil || root.fenceToken <= 0 || root.leaseTTL <= 0 {
		root.leaseMu.Unlock()
		return
	}
	owner, ttl := root.leaseOwner, root.leaseTTL
	beatCtx, cancel := context.WithCancel(ctx)
	root.heartbeatCancel = cancel
	root.leaseMu.Unlock()
	interval := ttl / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	if interval >= ttl {
		interval = ttl / 2
		if interval <= 0 {
			interval = time.Millisecond
		}
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-beatCtx.Done():
				return
			case now := <-t.C:
				if err := root.RenewLease(now); err != nil {
					root.leaseMu.Lock()
					if root.leaseOwner == owner {
						root.leaseLost = fmt.Errorf("%w: %v", ErrLeaseLost, err)
					}
					root.leaseMu.Unlock()
					return
				}
			}
		}
	}()
}

func (root *DurableRoot) leaseWriteState() (string, int64, error) {
	if root == nil {
		return "", 0, nil
	}
	root.leaseMu.Lock()
	defer root.leaseMu.Unlock()
	if root.leaseLost != nil {
		return "", 0, root.leaseLost
	}
	return root.leaseOwner, root.fenceToken, nil
}

// ReclaimExpired releases reservations only when the persisted root lease is
// expired. It is idempotent and never touches settled provider usage.
func (root *DurableRoot) ReclaimExpired(now time.Time) error {
	if root == nil {
		return nil
	}
	backend, ok := root.backend.(DurableLeaseBackend)
	if !ok {
		return fmt.Errorf("durable budget backend does not support leases")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return backend.ReclaimExpired(root.rootID, now.UTC())
}

// LeaseInfo returns the owner and fencing token captured by this handle.
func (root *DurableRoot) LeaseInfo() (string, int64, error) { return root.leaseWriteState() }

func (root *DurableRoot) Limits() agentapi.RunLimits {
	if root == nil {
		return agentapi.RunLimits{}
	}
	return root.limits
}

func (root *DurableRoot) Check() error {
	if root == nil {
		return nil
	}
	snapshot, err := root.backend.RootSnapshot(root.rootID)
	if err != nil {
		return fmt.Errorf("read durable budget root: %w", err)
	}
	allocated := addUsage(snapshot.Used, snapshot.Reserved)
	if snapshot.Limits.MaxInputTokens > 0 && allocated.InputTokens > snapshot.Limits.MaxInputTokens {
		return fmt.Errorf("%w: durable root input tokens %d exceed %d", agentapi.ErrBudgetExceeded, allocated.InputTokens, snapshot.Limits.MaxInputTokens)
	}
	if snapshot.Limits.MaxTotalTokens > 0 && allocated.TotalTokens > snapshot.Limits.MaxTotalTokens {
		return fmt.Errorf("%w: durable root total tokens %d exceed %d", agentapi.ErrBudgetExceeded, allocated.TotalTokens, snapshot.Limits.MaxTotalTokens)
	}
	if snapshot.Limits.MaxCostMicros > 0 && allocated.CostMicros > snapshot.Limits.MaxCostMicros {
		return fmt.Errorf("%w: durable root cost %d exceed %d", agentapi.ErrBudgetExceeded, allocated.CostMicros, snapshot.Limits.MaxCostMicros)
	}
	return nil
}

func (root *DurableRoot) Available() agentapi.Usage {
	return root.AvailableForPhase(agentapi.RunBudgetPhaseDefault)
}

func (root *DurableRoot) AvailableForPhase(phase agentapi.RunBudgetPhase) agentapi.Usage {
	if root == nil {
		return unboundedUsage()
	}
	snapshot, err := root.backend.RootSnapshot(root.rootID)
	if err != nil {
		return agentapi.Usage{}
	}
	available := remainingUsage(snapshot.Limits, addUsage(snapshot.Used, snapshot.Reserved))
	if phase != agentapi.RunBudgetPhaseAnswer && snapshot.Limits.ParentAnswerReserve > 0 {
		if available.TotalTokens != math.MaxInt64 {
			available.TotalTokens = max(0, available.TotalTokens-snapshot.Limits.ParentAnswerReserve)
		}
		if available.OutputTokens != math.MaxInt64 {
			available.OutputTokens = max(0, available.OutputTokens-snapshot.Limits.ParentAnswerReserve)
		}
	}
	return available
}

func (root *DurableRoot) Used() agentapi.Usage {
	if root == nil {
		return agentapi.Usage{}
	}
	snapshot, err := root.backend.RootSnapshot(root.rootID)
	if err != nil {
		return agentapi.Usage{}
	}
	return snapshot.Used
}

func (root *DurableRoot) ReserveTask(grant agentapi.Usage) (agentapi.RunBudgetTaskReservation, error) {
	if root == nil {
		return nil, nil
	}
	grant, err := normalizeUsage(grant)
	if err != nil {
		return nil, err
	}
	id := durableReservationID("task", root.rootID)
	owner, fence, err := root.leaseWriteState()
	if err != nil {
		return nil, err
	}
	reservation := DurableReservation{ID: id, Kind: "task", Grant: grant}
	if fenced, ok := root.backend.(DurableLeaseTokenBackend); ok && fencingEnabled(root.backend) && fence > 0 {
		err = fenced.ReserveFenced(root.rootID, owner, fence, reservation)
	} else {
		err = root.backend.Reserve(root.rootID, reservation)
	}
	if err != nil {
		return nil, err
	}
	return &DurableTask{backend: root.backend, rootID: root.rootID, id: id, owner: owner, fence: fence}, nil
}

func (root *DurableRoot) ReserveCall(estimate agentapi.Usage) (agentapi.RunBudgetCallReservation, error) {
	return root.ReserveCallForPhase(estimate, agentapi.RunBudgetPhaseDefault)
}

func (root *DurableRoot) ReserveCallForPhase(estimate agentapi.Usage, phase agentapi.RunBudgetPhase) (agentapi.RunBudgetCallReservation, error) {
	if root == nil {
		return nil, nil
	}
	estimate, err := normalizeUsage(estimate)
	if err != nil {
		return nil, err
	}
	id := durableReservationID("call", root.rootID)
	owner, fence, err := root.leaseWriteState()
	if err != nil {
		return nil, err
	}
	reservation := DurableReservation{ID: id, Kind: "call", Phase: phase, Estimate: estimate}
	if fenced, ok := root.backend.(DurableLeaseTokenBackend); ok && fencingEnabled(root.backend) && fence > 0 {
		err = fenced.ReserveFenced(root.rootID, owner, fence, reservation)
	} else {
		err = root.backend.Reserve(root.rootID, reservation)
	}
	if err != nil {
		return nil, err
	}
	return &durableCallReservation{backend: root.backend, rootID: root.rootID, id: id, owner: owner, fence: fence}, nil
}

func (task *DurableTask) Available() agentapi.Usage {
	if task == nil || task.backend == nil {
		return unboundedUsage()
	}
	snapshot, err := task.backend.TaskSnapshot(task.rootID, task.id)
	if err != nil || snapshot.Released {
		return agentapi.Usage{}
	}
	return subtractUsage(snapshot.Grant, addUsage(snapshot.Used, snapshot.InFlight))
}

func (task *DurableTask) Check() error {
	if task == nil || task.backend == nil {
		return nil
	}
	snapshot, err := task.backend.TaskSnapshot(task.rootID, task.id)
	if err != nil {
		return fmt.Errorf("read durable budget task: %w", err)
	}
	if snapshot.Released {
		return fmt.Errorf("%w: child task budget released", agentapi.ErrBudgetExceeded)
	}
	return requireWithin(addUsage(snapshot.Used, snapshot.InFlight), snapshot.Grant, "child task usage")
}

func (task *DurableTask) ReserveCall(estimate agentapi.Usage) (agentapi.RunBudgetCallReservation, error) {
	if task == nil || task.backend == nil {
		return nil, nil
	}
	estimate, err := normalizeUsage(estimate)
	if err != nil {
		return nil, err
	}
	id := durableReservationID("call", task.rootID)
	reservation := DurableReservation{ID: id, ParentID: task.id, Kind: "call", Phase: agentapi.RunBudgetPhaseDefault, Estimate: estimate}
	if fenced, ok := task.backend.(DurableLeaseTokenBackend); ok && fencingEnabled(task.backend) && task.fence > 0 {
		err = fenced.ReserveFenced(task.rootID, task.owner, task.fence, reservation)
	} else {
		err = task.backend.Reserve(task.rootID, reservation)
	}
	if err != nil {
		return nil, err
	}
	return &durableCallReservation{backend: task.backend, rootID: task.rootID, id: id, owner: task.owner, fence: task.fence}, nil
}

func (task *DurableTask) Release() error {
	if task == nil || task.backend == nil {
		return nil
	}
	if fenced, ok := task.backend.(DurableLeaseTokenBackend); ok && fencingEnabled(task.backend) && task.fence > 0 {
		return fenced.ReleaseTaskFenced(task.rootID, task.owner, task.fence, task.id)
	}
	return task.backend.ReleaseTask(task.rootID, task.id)
}

func (reservation *durableCallReservation) Settle(actual agentapi.Usage) error {
	if reservation == nil {
		return nil
	}
	actual, err := normalizeUsage(actual)
	if err != nil {
		return err
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.state == reservationSettled {
		if reservation.actual == actual {
			return nil
		}
		return fmt.Errorf("%w: model call settled twice with different usage", agentapi.ErrBudgetExceeded)
	}
	if reservation.state == reservationReleased {
		return fmt.Errorf("%w: model call reservation already released", agentapi.ErrBudgetExceeded)
	}
	var settleErr error
	if fenced, ok := reservation.backend.(DurableLeaseTokenBackend); ok && fencingEnabled(reservation.backend) && reservation.fence > 0 {
		settleErr = fenced.SettleCallFenced(reservation.rootID, reservation.owner, reservation.fence, reservation.id, actual)
	} else {
		settleErr = reservation.backend.SettleCall(reservation.rootID, reservation.id, actual)
	}
	if settleErr != nil {
		var committed *SettlementError
		if errors.As(settleErr, &committed) && committed.Committed {
			reservation.state = reservationSettled
			reservation.actual = actual
		}
		return settleErr
	}
	reservation.state = reservationSettled
	reservation.actual = actual
	return nil
}

func (reservation *durableCallReservation) Release() error {
	if reservation == nil {
		return nil
	}
	reservation.mu.Lock()
	defer reservation.mu.Unlock()
	if reservation.state == reservationReleased || reservation.state == reservationSettled {
		return nil
	}
	var err error
	if fenced, ok := reservation.backend.(DurableLeaseTokenBackend); ok && fencingEnabled(reservation.backend) && reservation.fence > 0 {
		err = fenced.ReleaseCallFenced(reservation.rootID, reservation.owner, reservation.fence, reservation.id)
	} else {
		err = reservation.backend.ReleaseCall(reservation.rootID, reservation.id)
	}
	if err != nil {
		return err
	}
	reservation.state = reservationReleased
	return nil
}

func durableReservationID(kind, rootID string) string {
	sequence := durableReservationSeq.Add(1)
	return fmt.Sprintf("%s-%s-%d-%d", kind, rootID, time.Now().UTC().UnixNano(), sequence)
}
