//go:build integration

package run

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/platform/dbschema"
	_ "github.com/go-sql-driver/mysql"
)

func TestMySQLCrossInstanceWorkClaimAndFencing(t *testing.T) {
	db := startEphemeralMySQL(t)
	first, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	item := WorkItem{
		WorkID: "cross-instance-work", RunID: "parent-run", ParentRunID: "parent-run",
		DelegationID: "delegation-1", TaskIndex: 0, AttemptNo: 1,
		Kind: "delegation_child", Payload: []byte(`{"objective":"inspect"}`), State: WorkReady,
	}
	if err := first.EnqueueWorkItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	ttl := 10 * time.Second
	type claimResult struct {
		item WorkItem
		err  error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	claim := func(store *Store, owner string) {
		<-start
		claimed, err := store.ClaimWorkItemByID(context.Background(), item.WorkID, owner, now, ttl)
		results <- claimResult{item: claimed, err: err}
	}
	go claim(first, "worker-a")
	go claim(second, "worker-b")
	close(start)

	var winner WorkItem
	var successes, empty int
	for range 2 {
		result := <-results
		switch {
		case result.err == nil:
			successes++
			winner = result.item
		case errors.Is(result.err, sql.ErrNoRows):
			empty++
		default:
			t.Fatalf("claim error: %v", result.err)
		}
	}
	if successes != 1 || empty != 1 {
		t.Fatalf("claims: success=%d empty=%d", successes, empty)
	}
	if winner.LeaseFence != 1 || winner.LeaseOwner == "" {
		t.Fatalf("winner lease = owner %q fence %d", winner.LeaseOwner, winner.LeaseFence)
	}

	reclaimer := first
	if winner.LeaseOwner == "worker-a" {
		reclaimer = second
	}
	reclaimed, err := reclaimer.ClaimWorkItemByID(
		context.Background(), item.WorkID, "worker-reclaimer", now.Add(ttl+time.Second), ttl,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed.LeaseFence != winner.LeaseFence+1 {
		t.Fatalf("reclaimed fence=%d want=%d", reclaimed.LeaseFence, winner.LeaseFence+1)
	}
	if err := first.CompleteWorkItem(context.Background(), item.WorkID, winner.LeaseOwner, winner.LeaseFence, WorkSucceeded, ""); err == nil {
		t.Fatal("stale worker completion unexpectedly succeeded")
	}
	if err := reclaimer.CompleteWorkItem(context.Background(), item.WorkID, reclaimed.LeaseOwner, reclaimed.LeaseFence, WorkSucceeded, ""); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLConcurrentRecoveryHasSingleLeaseWinner(t *testing.T) {
	db := startEphemeralMySQL(t)
	first, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	limits := agentapi.RunLimits{Deadline: time.Now().UTC().Add(5 * time.Minute), MaxSteps: 4, MaxTotalTokens: 1000}
	gate, err := first.CreateWithDurableBudgetContext(context.Background(), Record{
		ID: "recovery-run", RunKind: KindAgent, Status: StatusRunning,
		Selection: agentapi.DefinitionSelection{}, RunLimits: limits,
		Question: "resume me", MaxSteps: limits.MaxSteps,
	}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := gate.(interface{ Close() }); ok {
		closer.Close()
	} else {
		t.Fatal("durable root does not expose heartbeat Close")
	}
	if err := first.SaveLogicalCheckpoint(context.Background(), LogicalCheckpoint{
		RunID: "recovery-run", StepNo: 2, Phase: "running", State: []byte(`{"version":1}`),
		LeaseOwner: first.LeaseOwner(), LeaseFence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE agent_run_budget_ledger SET lease_expires_at=UTC_TIMESTAMP()-INTERVAL 10 SECOND WHERE root_run_id=?`, "recovery-run"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	counts := make(chan int64, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, store := range []*Store{first, second} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			<-start
			count, err := store.RecoverInterrupted()
			counts <- count
			errs <- err
		}(store)
	}
	close(start)
	wg.Wait()
	close(counts)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var total int64
	for count := range counts {
		total += count
	}
	if total != 1 {
		t.Fatalf("recovery claims=%d want=1", total)
	}
	var owner, state, kind string
	var fence int64
	if err := db.QueryRow(`SELECT l.lease_owner,l.lease_fence,w.state,w.kind
		FROM agent_run_budget_ledger l JOIN agent_work_items w ON w.run_id=l.root_run_id
		WHERE l.root_run_id=?`, "recovery-run").Scan(&owner, &fence, &state, &kind); err != nil {
		t.Fatal(err)
	}
	if owner != first.LeaseOwner() && owner != second.LeaseOwner() {
		t.Fatalf("unexpected recovery owner %q", owner)
	}
	if fence != 2 || state != WorkReady || kind != WorkParentResume {
		t.Fatalf("recovery projection owner=%q fence=%d state=%q kind=%q", owner, fence, state, kind)
	}
}

func startEphemeralMySQL(t *testing.T) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("Docker-backed MySQL integration test disabled by -short")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker unavailable")
	}
	name := "nasuta-mysql-it-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, "run", "-d", "--rm", "--name", name,
		"-e", "MYSQL_ROOT_PASSWORD=nasuta", "-e", "MYSQL_DATABASE=nasuta",
		"-p", "127.0.0.1::3306", "mysql:8.0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("start mysql: %v: %s", err, strings.TrimSpace(string(output)))
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		_ = exec.CommandContext(cleanupCtx, docker, "rm", "-f", name).Run()
	})

	var address string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		output, err := exec.CommandContext(ctx, docker, "port", name, "3306/tcp").Output()
		if err == nil {
			address = strings.TrimSpace(string(output))
			if _, _, splitErr := net.SplitHostPort(address); splitErr == nil {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if address == "" {
		t.Fatal("mysql container port was not published")
	}
	dsn := fmt.Sprintf("root:nasuta@tcp(%s)/nasuta?parseTime=true&charset=utf8mb4", address)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(16)
	pingDeadline := time.Now().Add(90 * time.Second)
	for {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
		err = db.PingContext(pingCtx)
		pingCancel()
		if err == nil {
			break
		}
		if time.Now().After(pingDeadline) {
			logs, _ := exec.CommandContext(ctx, docker, "logs", name).CombinedOutput()
			t.Fatalf("mysql did not become ready: %v\n%s", err, logs)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err := dbschema.MigrateMySQL(db, dbschema.GroupQARun); err != nil {
		t.Fatal(err)
	}
	return db
}
