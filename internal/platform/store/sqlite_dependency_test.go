package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/platform"
)

func TestReplaceAllPublishesCanonicalDependenciesAndEvidence(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	orders := testService("team/orders", ".", "orders")
	payments := testService("team/payments", ".", "payments")
	bundle := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{
			{Repo: orders.Repo, HeadSHA: "orders-sha", IndexedAt: time.Now().UnixMilli()},
			{Repo: payments.Repo, HeadSHA: "payments-sha", IndexedAt: time.Now().UnixMilli()},
		},
		Services: []domain.ServiceRecord{orders, payments},
		Dependencies: []domain.DependencyEdge{{
			CallerServiceKey: orders.ServiceKey,
			TargetKind:       domain.DependencyTargetService,
			TargetServiceKey: payments.ServiceKey,
			From:             orders.ServiceName,
			To:               payments.ServiceName,
			Type:             domain.EdgeHTTP,
			Evidence: []domain.Evidence{
				{Path: "repos/team/orders/src/client.go", Line: 10, Symbol: "CallPayments", Kind: domain.SourceCodeScan},
				{Path: "repos/team/orders/src/retry.go", Line: 20, Symbol: "RetryPayments", Kind: domain.SourceCodeScan},
			},
			Confidence: 0.9,
		}},
	}
	if err := db.ReplaceAll(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}

	edges, err := db.Edges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].From != "orders" || edges[0].To != "payments" {
		t.Fatalf("edges = %+v", edges)
	}
	if len(edges[0].Evidence) != 2 {
		t.Fatalf("evidence = %+v, want two locations", edges[0].Evidence)
	}

	replacement := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{{Repo: orders.Repo, HeadSHA: "orders-sha-2", IndexedAt: time.Now().UnixMilli()}},
		Services:     []domain.ServiceRecord{orders},
	}
	if err := db.ReplaceAll(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	edges, err = db.Edges(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Fatalf("stale dependency survived snapshot replacement: %+v", edges)
	}
	sha, err := db.GetIndexSHA(context.Background(), orders.Repo)
	if err != nil || sha != "orders-sha-2" {
		t.Fatalf("repository sha = %q, err=%v", sha, err)
	}
}

func testService(repo, module, name string) domain.ServiceRecord {
	return domain.ServiceRecord{
		ServiceKey:    platform.UUIDFromString(repo + "\x00" + module),
		ServiceName:   name,
		Repo:          repo,
		ModulePath:    module,
		Layer:         "server",
		Language:      "go",
		Tags:          []string{},
		Docs:          []string{},
		SourceOfTruth: []string{},
		Entrypoints:   []domain.Evidence{},
		Ports:         []int{},
		Confidence:    0.9,
	}
}
