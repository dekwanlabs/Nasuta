package budget

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

type fakeDurableRoot struct {
	limits         agentapi.RunLimits
	used           agentapi.Usage
	reserved       agentapi.Usage
	leaseOwner     string
	leaseExpiresAt time.Time
}

type fakeDurableReservation struct {
	DurableReservation
	rootID string
	state  string
	used   agentapi.Usage
}

type fakeDurableBackend struct {
	mu sync.Mutex

	roots          map[string]*fakeDurableRoot
	reservations   map[string]*fakeDurableReservation
	ensureErr      error
	rootReadErr    error
	taskReadErr    error
	reserveErr     error
	settleErr      error
	releaseErr     error
	releaseTaskErr error
}

func newFakeDurableBackend() *fakeDurableBackend {
	return &fakeDurableBackend{
		roots:        make(map[string]*fakeDurableRoot),
		reservations: make(map[string]*fakeDurableReservation),
	}
}

func (backend *fakeDurableBackend) EnsureRoot(rootID string, limits agentapi.RunLimits) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.ensureErr != nil {
		return backend.ensureErr
	}
	if existing := backend.roots[rootID]; existing != nil {
		if !sameFakeLimits(existing.limits, limits) {
			return fmt.Errorf("root limits conflict")
		}
		return nil
	}
	backend.roots[rootID] = &fakeDurableRoot{limits: limits}
	return nil
}

func (backend *fakeDurableBackend) AcquireLease(rootID, owner string, now time.Time, ttl time.Duration) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	root := backend.roots[rootID]
	if root == nil {
		return errors.New("root not found")
	}
	expired := root.leaseOwner != "" && !root.leaseExpiresAt.After(now)
	switch {
	case root.leaseOwner == owner && !expired:
	case root.leaseOwner == "":
		if root.reserved != (agentapi.Usage{}) {
			return ErrLeaseHasReservations
		}
	case expired:
		fakeReclaimReservations(backend, rootID)
	default:
		return ErrLeaseHeld
	}
	root.leaseOwner = owner
	root.leaseExpiresAt = now.Add(ttl)
	return nil
}

func (backend *fakeDurableBackend) RenewLease(rootID, owner string, now time.Time, ttl time.Duration) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	root := backend.roots[rootID]
	if root == nil {
		return errors.New("root not found")
	}
	if root.leaseOwner != owner {
		return ErrLeaseOwnerMismatch
	}
	if !root.leaseExpiresAt.After(now) {
		return ErrLeaseNotActive
	}
	root.leaseExpiresAt = now.Add(ttl)
	return nil
}

func (backend *fakeDurableBackend) ReleaseLease(rootID, owner string, _ time.Time) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	root := backend.roots[rootID]
	if root == nil {
		return errors.New("root not found")
	}
	if root.leaseOwner == "" {
		return nil
	}
	if root.leaseOwner != owner {
		return ErrLeaseOwnerMismatch
	}
	for _, reservation := range backend.reservations {
		if reservation.rootID == rootID && (reservation.state == "open" || reservation.state == "active") {
			return ErrLeaseHasReservations
		}
	}
	if root.reserved != (agentapi.Usage{}) {
		return ErrLeaseHasReservations
	}
	root.leaseOwner = ""
	root.leaseExpiresAt = time.Time{}
	return nil
}

func (backend *fakeDurableBackend) ReclaimExpired(rootID string, now time.Time) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	root := backend.roots[rootID]
	if root == nil {
		return errors.New("root not found")
	}
	if root.leaseOwner == "" || root.leaseExpiresAt.After(now) {
		return nil
	}
	fakeReclaimReservations(backend, rootID)
	root.leaseOwner = ""
	root.leaseExpiresAt = time.Time{}
	return nil
}

func fakeReclaimReservations(backend *fakeDurableBackend, rootID string) {
	for _, reservation := range backend.reservations {
		if reservation.rootID == rootID && (reservation.state == "open" || reservation.state == "active") {
			reservation.state = "released"
		}
	}
	backend.roots[rootID].reserved = agentapi.Usage{}
}

func (backend *fakeDurableBackend) RootSnapshot(rootID string) (DurableRootSnapshot, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.rootReadErr != nil {
		return DurableRootSnapshot{}, backend.rootReadErr
	}
	root := backend.roots[rootID]
	if root == nil {
		return DurableRootSnapshot{}, errors.New("root not found")
	}
	return DurableRootSnapshot{Limits: root.limits, Used: root.used, Reserved: root.reserved}, nil
}

func (backend *fakeDurableBackend) TaskSnapshot(rootID, reservationID string) (DurableTaskSnapshot, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.taskReadErr != nil {
		return DurableTaskSnapshot{}, backend.taskReadErr
	}
	task := backend.reservations[reservationID]
	if task == nil || task.rootID != rootID || task.Kind != "task" {
		return DurableTaskSnapshot{}, errors.New("task not found")
	}
	snapshot := DurableTaskSnapshot{Grant: task.Grant, Used: task.used, Released: task.state != "active"}
	for _, call := range backend.reservations {
		if call.rootID == rootID && call.ParentID == reservationID && call.Kind == "call" && call.state == "open" {
			snapshot.InFlight = addUsage(snapshot.InFlight, call.Estimate)
		}
	}
	return snapshot, nil
}

func (backend *fakeDurableBackend) Reserve(rootID string, requested DurableReservation) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.reserveErr != nil {
		return backend.reserveErr
	}
	root := backend.roots[rootID]
	if root == nil {
		return errors.New("root not found")
	}
	if existing := backend.reservations[requested.ID]; existing != nil {
		if existing.rootID != rootID || existing.ParentID != requested.ParentID || existing.Kind != requested.Kind || existing.Phase != requested.Phase || existing.Estimate != requested.Estimate || existing.Grant != requested.Grant {
			return errors.New("reservation conflict")
		}
		if existing.state == "released" {
			return errors.New("reservation released")
		}
		return nil
	}
	if requested.Kind == "task" {
		available := fakeBudgetAvailable(root.limits, root.used, root.reserved, agentapi.RunBudgetPhaseDefault)
		if err := requireWithin(requested.Grant, available, "task"); err != nil {
			return err
		}
		root.reserved = addUsage(root.reserved, requested.Grant)
	} else if requested.ParentID == "" {
		available := fakeBudgetAvailable(root.limits, root.used, root.reserved, requested.Phase)
		if err := requireWithin(requested.Estimate, available, "call"); err != nil {
			return err
		}
		root.reserved = addUsage(root.reserved, requested.Estimate)
	} else {
		task := backend.reservations[requested.ParentID]
		if task == nil || task.rootID != rootID || task.Kind != "task" || task.state != "active" {
			return fmt.Errorf("%w: task unavailable", agentapi.ErrBudgetExceeded)
		}
		inFlight := agentapi.Usage{}
		for _, call := range backend.reservations {
			if call.rootID == rootID && call.ParentID == requested.ParentID && call.Kind == "call" && call.state == "open" {
				inFlight = addUsage(inFlight, call.Estimate)
			}
		}
		if err := requireWithin(requested.Estimate, subtractUsage(task.Grant, addUsage(task.used, inFlight)), "child call"); err != nil {
			return err
		}
	}
	state := "open"
	if requested.Kind == "task" {
		state = "active"
	}
	backend.reservations[requested.ID] = &fakeDurableReservation{DurableReservation: requested, rootID: rootID, state: state}
	return nil
}

func (backend *fakeDurableBackend) SettleCall(rootID, reservationID string, actual agentapi.Usage) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.settleErr != nil {
		return backend.settleErr
	}
	call := backend.reservations[reservationID]
	if call == nil || call.rootID != rootID || call.Kind != "call" {
		return errors.New("call not found")
	}
	if call.state == "settled" {
		if call.used == actual {
			return nil
		}
		return errors.New("settlement conflict")
	}
	if call.state != "open" {
		return errors.New("call not open")
	}
	root := backend.roots[rootID]
	accountingErr := requireWithin(actual, call.Estimate, "reported usage")
	root.used = addUsage(root.used, actual)
	if call.ParentID == "" {
		root.reserved = subtractUsage(root.reserved, call.Estimate)
	} else {
		task := backend.reservations[call.ParentID]
		if task == nil || task.state != "active" {
			return errors.New("task released")
		}
		task.used = addUsage(task.used, actual)
		root.reserved = subtractUsage(root.reserved, actual)
	}
	call.used = actual
	call.state = "settled"
	if accountingErr == nil {
		accountingErr = checkFakeRoot(root)
	}
	if accountingErr != nil {
		return &SettlementError{Err: accountingErr, Committed: true}
	}
	return nil
}

func (backend *fakeDurableBackend) ReleaseCall(rootID, reservationID string) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.releaseErr != nil {
		return backend.releaseErr
	}
	call := backend.reservations[reservationID]
	if call == nil || call.rootID != rootID || call.Kind != "call" {
		return errors.New("call not found")
	}
	if call.state == "released" || call.state == "settled" {
		return nil
	}
	call.state = "released"
	if call.ParentID == "" {
		root := backend.roots[rootID]
		root.reserved = subtractUsage(root.reserved, call.Estimate)
	}
	return nil
}

func (backend *fakeDurableBackend) ReleaseTask(rootID, reservationID string) error {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.releaseTaskErr != nil {
		return backend.releaseTaskErr
	}
	task := backend.reservations[reservationID]
	if task == nil || task.rootID != rootID || task.Kind != "task" {
		return errors.New("task not found")
	}
	if task.state == "released" {
		return nil
	}
	for _, call := range backend.reservations {
		if call.rootID == rootID && call.ParentID == reservationID && call.Kind == "call" && call.state == "open" {
			return fmt.Errorf("%w: in-flight call", agentapi.ErrBudgetExceeded)
		}
	}
	root := backend.roots[rootID]
	root.reserved = subtractUsage(root.reserved, subtractUsage(task.Grant, task.used))
	task.state = "released"
	return nil
}

func fakeBudgetAvailable(limits agentapi.RunLimits, used, reserved agentapi.Usage, phase agentapi.RunBudgetPhase) agentapi.Usage {
	allocated := addUsage(used, reserved)
	available := agentapi.Usage{
		InputTokens:  fakeRemaining(limits.MaxInputTokens, allocated.InputTokens),
		OutputTokens: fakeRemaining(limits.MaxTotalTokens, allocated.TotalTokens),
		TotalTokens:  fakeRemaining(limits.MaxTotalTokens, allocated.TotalTokens),
		CostMicros:   fakeRemaining(limits.MaxCostMicros, allocated.CostMicros),
	}
	if phase != agentapi.RunBudgetPhaseAnswer && limits.ParentAnswerReserve > 0 {
		available.OutputTokens = fakeMax(0, available.OutputTokens-limits.ParentAnswerReserve)
		available.TotalTokens = fakeMax(0, available.TotalTokens-limits.ParentAnswerReserve)
	}
	return available
}

func fakeRemaining(limit, used int64) int64 {
	if limit <= 0 {
		return int64(^uint64(0) >> 1)
	}
	if used >= limit {
		return 0
	}
	return limit - used
}

func fakeMax(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func checkFakeRoot(root *fakeDurableRoot) error {
	allocated := addUsage(root.used, root.reserved)
	if root.limits.MaxTotalTokens > 0 && allocated.TotalTokens > root.limits.MaxTotalTokens {
		return fmt.Errorf("%w: root total exceeded", agentapi.ErrBudgetExceeded)
	}
	if root.limits.MaxInputTokens > 0 && allocated.InputTokens > root.limits.MaxInputTokens {
		return fmt.Errorf("%w: root input exceeded", agentapi.ErrBudgetExceeded)
	}
	if root.limits.MaxCostMicros > 0 && allocated.CostMicros > root.limits.MaxCostMicros {
		return fmt.Errorf("%w: root cost exceeded", agentapi.ErrBudgetExceeded)
	}
	return nil
}

func sameFakeLimits(left, right agentapi.RunLimits) bool {
	return left.Deadline.Equal(right.Deadline) && left.MaxSteps == right.MaxSteps && left.MaxToolCalls == right.MaxToolCalls && left.MaxInputTokens == right.MaxInputTokens && left.MaxContextTokens == right.MaxContextTokens && left.MaxOutputTokens == right.MaxOutputTokens && left.MaxTotalTokens == right.MaxTotalTokens && left.MaxCostMicros == right.MaxCostMicros && left.ParentAnswerReserve == right.ParentAnswerReserve
}

func TestDurableRootEnsuresRootAndRejectsLimitConflicts(t *testing.T) {
	backend := newFakeDurableBackend()
	limits := agentapi.RunLimits{MaxTotalTokens: 100, ParentAnswerReserve: 20, Deadline: time.Now().UTC().Truncate(time.Second)}
	root, err := NewDurableRoot(backend, "root-1", limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewDurableRoot(backend, "root-1", limits); err != nil {
		t.Fatalf("idempotent root ensure = %v", err)
	}
	if _, err := NewDurableRoot(backend, "root-1", agentapi.RunLimits{MaxTotalTokens: 101, ParentAnswerReserve: 20}); err == nil {
		t.Fatal("limit conflict was accepted")
	}
	if got := root.Available().TotalTokens; got != 80 {
		t.Fatalf("default available = %d, want 80", got)
	}
}

func TestDurableTaskGrantSettlementAndRelease(t *testing.T) {
	backend := newFakeDurableBackend()
	root, err := NewDurableRoot(backend, "root-1", agentapi.RunLimits{MaxTotalTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	task, err := root.ReserveTask(agentapi.Usage{TotalTokens: 60})
	if err != nil {
		t.Fatal(err)
	}
	call, err := task.ReserveCall(agentapi.Usage{TotalTokens: 30})
	if err != nil {
		t.Fatal(err)
	}
	if got := task.Available().TotalTokens; got != 30 {
		t.Fatalf("in-flight task availability = %d, want 30", got)
	}
	if err := call.Settle(agentapi.Usage{TotalTokens: 20}); err != nil {
		t.Fatal(err)
	}
	if got := root.Used().TotalTokens; got != 20 {
		t.Fatalf("root used = %d, want 20", got)
	}
	if got := task.Available().TotalTokens; got != 40 {
		t.Fatalf("settled task availability = %d, want 40", got)
	}
	if err := task.Release(); err != nil {
		t.Fatal(err)
	}
	if got := root.Available().TotalTokens; got != 80 {
		t.Fatalf("post-release root availability = %d, want 80", got)
	}
}

func TestDurableRootProtectsAnswerAndReusesTaskGrantAcrossRetry(t *testing.T) {
	backend := newFakeDurableBackend()
	root, err := NewDurableRoot(backend, "root-1", agentapi.RunLimits{MaxTotalTokens: 100, ParentAnswerReserve: 20})
	if err != nil {
		t.Fatal(err)
	}
	task, err := root.ReserveTask(agentapi.Usage{TotalTokens: 70})
	if err != nil {
		t.Fatal(err)
	}
	first, err := task.ReserveCall(agentapi.Usage{TotalTokens: 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := task.ReserveCall(agentapi.Usage{TotalTokens: 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Settle(agentapi.Usage{TotalTokens: 30}); err != nil {
		t.Fatal(err)
	}
	if got := root.Available().TotalTokens; got != 10 {
		t.Fatalf("default root availability = %d, want 10", got)
	}
	if got := root.AvailableForPhase(agentapi.RunBudgetPhaseAnswer).TotalTokens; got != 30 {
		t.Fatalf("answer root availability = %d, want 30", got)
	}
}

func TestDurableSettlementOverrunIsCommittedAndIdempotent(t *testing.T) {
	backend := newFakeDurableBackend()
	root, err := NewDurableRoot(backend, "root-1", agentapi.RunLimits{MaxTotalTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	call, err := root.ReserveCall(agentapi.Usage{TotalTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	err = call.Settle(agentapi.Usage{TotalTokens: 40})
	if !errors.Is(err, agentapi.ErrBudgetExceeded) {
		t.Fatalf("overrun error = %v, want budget exceeded", err)
	}
	if err := call.Settle(agentapi.Usage{TotalTokens: 40}); err != nil {
		t.Fatalf("same committed settlement = %v", err)
	}
	if got := root.Used().TotalTokens; got != 40 {
		t.Fatalf("overrun usage = %d, want 40", got)
	}
	if err := call.Release(); err != nil {
		t.Fatalf("release after committed overrun = %v", err)
	}
}

func TestDurableTaskReleaseRejectsInFlightAndBackendReadsFailClosed(t *testing.T) {
	backend := newFakeDurableBackend()
	root, err := NewDurableRoot(backend, "root-1", agentapi.RunLimits{MaxTotalTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	task, err := root.ReserveTask(agentapi.Usage{TotalTokens: 60})
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
	backend.rootReadErr = errors.New("ledger unavailable")
	if got := root.Available().TotalTokens; got != 0 {
		t.Fatalf("root fail-closed availability = %d, want 0", got)
	}
	if err := root.Check(); err == nil {
		t.Fatal("root check hid backend read failure")
	}
	backend.taskReadErr = errors.New("task ledger unavailable")
	if got := task.Available().TotalTokens; got != 0 {
		t.Fatalf("task fail-closed availability = %d, want 0", got)
	}
	if err := task.Check(); err == nil {
		t.Fatal("task check hid backend read failure")
	}
	_ = call.Release()
}

func TestDurableConcurrentTaskReservationsDoNotOversubscribe(t *testing.T) {
	backend := newFakeDurableBackend()
	root, err := NewDurableRoot(backend, "root-1", agentapi.RunLimits{MaxTotalTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	reservations := make([]agentapi.RunBudgetTaskReservation, 0, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			task, reserveErr := root.ReserveTask(agentapi.Usage{TotalTokens: 10})
			if reserveErr != nil {
				return
			}
			mu.Lock()
			admitted++
			reservations = append(reservations, task)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if admitted != 10 {
		t.Fatalf("admitted = %d, want 10", admitted)
	}
	for _, task := range reservations {
		if err := task.Release(); err != nil {
			t.Fatal(err)
		}
	}
}

type nonLeaseDurableBackend struct {
	DurableBackend
}

func TestDurableRootLeaseLifecycle(t *testing.T) {
	backend := newFakeDurableBackend()
	root, err := NewDurableRoot(backend, "root-lease", agentapi.RunLimits{MaxTotalTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := root.RenewLease(time.Time{}); !errors.Is(err, ErrLeaseNotActive) {
		t.Fatalf("renew before acquire = %v, want lease not active", err)
	}
	now := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	if err := root.AcquireLease("worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	owner := backend.roots["root-lease"].leaseOwner
	expiresAt := backend.roots["root-lease"].leaseExpiresAt
	backend.mu.Unlock()
	if owner != "worker-a" || !expiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("lease = (%q, %s), want worker-a until %s", owner, expiresAt, now.Add(time.Minute))
	}
	if err := root.RenewLease(now.Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	expiresAt = backend.roots["root-lease"].leaseExpiresAt
	backend.mu.Unlock()
	if !expiresAt.Equal(now.Add(90 * time.Second)) {
		t.Fatalf("renewed expiry = %s, want %s", expiresAt, now.Add(90*time.Second))
	}
	if err := root.ReleaseLease(); err != nil {
		t.Fatal(err)
	}
	if err := root.ReleaseLease(); err != nil {
		t.Fatalf("idempotent release = %v", err)
	}
}

func TestDurableRootLeaseFailsClosedWithoutBackendSupport(t *testing.T) {
	backend := newFakeDurableBackend()
	root, err := NewDurableRoot(nonLeaseDurableBackend{DurableBackend: backend}, "root-no-lease", agentapi.RunLimits{MaxTotalTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := root.AcquireLease("worker-a", time.Now(), time.Minute); err == nil {
		t.Fatal("lease acquire succeeded without backend support")
	}
	if err := root.ReclaimExpired(time.Now()); err == nil {
		t.Fatal("lease reclaim succeeded without backend support")
	}
}

func TestDurableRootReclaimsExpiredReservationsWithoutLosingSettledUsage(t *testing.T) {
	backend := newFakeDurableBackend()
	root, err := NewDurableRoot(backend, "root-reclaim", agentapi.RunLimits{MaxTotalTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 1, 10, 0, 0, 0, time.UTC)
	if err := root.AcquireLease("worker-a", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	settled, err := root.ReserveCall(agentapi.Usage{TotalTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := settled.Settle(agentapi.Usage{TotalTokens: 15}); err != nil {
		t.Fatal(err)
	}
	if _, err := root.ReserveCall(agentapi.Usage{TotalTokens: 30}); err != nil {
		t.Fatal(err)
	}
	if err := root.ReclaimExpired(now.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := root.Used().TotalTokens; got != 15 {
		t.Fatalf("settled usage = %d, want 15", got)
	}
	if got := root.Available().TotalTokens; got != 85 {
		t.Fatalf("available after reclaim = %d, want 85", got)
	}
	if err := root.ReclaimExpired(now.Add(3 * time.Minute)); err != nil {
		t.Fatalf("idempotent reclaim = %v", err)
	}
}
