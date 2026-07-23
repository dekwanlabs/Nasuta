package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/platform"
)

func TestBackendPublishesAndQueriesBoundedOntology(t *testing.T) {
	backend := testBackend(t)
	ctx := context.Background()

	resolved, err := backend.Resolve(ctx, ontology.ResolveQuery{Text: "orders", Classes: []ontology.Class{ontology.ClassService}, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Entities) != 1 || resolved.Entities[0].Name != "orders" {
		t.Fatalf("resolved entities = %#v", resolved.Entities)
	}

	facts, truncated, err := backend.Neighbors(ctx, ontology.NeighborQuery{
		EntityIDs: []string{resolved.Entities[0].ID}, Predicates: []ontology.Predicate{ontology.PredicateDependsOn},
		Direction: ontology.DirectionOutgoing, Limit: 10, Generation: resolved.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(facts) != 1 || len(facts[0].Evidence) != 1 {
		t.Fatalf("facts=%#v truncated=%v", facts, truncated)
	}

	paths, truncated, err := ontology.FindBoundedPaths(ctx, backend, ontology.PathQuery{
		StartID: resolved.Entities[0].ID, Predicates: []ontology.Predicate{ontology.PredicateDependsOn},
		Direction: ontology.DirectionOutgoing, MaxDepth: 3, MaxNodes: 20, MaxFanout: 10, Generation: resolved.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(paths) != 1 || len(paths[0].Facts) != 1 {
		t.Fatalf("paths=%#v truncated=%v", paths, truncated)
	}

	stats, err := backend.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Entities != 4 || stats.Facts != 3 || stats.ByPredicate[ontology.PredicateDependsOn] != 1 {
		t.Fatalf("stats = %#v", stats)
	}

	result, err := ontology.NewService(backend).QueryRelations(ctx, ontology.RelationQuery{
		Entity: "orders", EntityClass: ontology.ClassService,
		Predicates: []ontology.Predicate{ontology.PredicateDependsOn}, Direction: ontology.DirectionOutgoing,
		MaxDepth: 3, MaxNodes: 20, MaxFanout: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Root == nil || result.Root.Name != "orders" || len(result.Entities) != 1 || result.Entities[0].Name != "payments" {
		t.Fatalf("relation result = %#v", result)
	}
	if len(result.Facts) != 1 || result.Facts[0].Subject.Name != "orders" || result.Facts[0].Object.Name != "payments" {
		t.Fatalf("relation facts = %#v", result.Facts)
	}
}

func TestBackendReportsTruncationAtStorageLimit(t *testing.T) {
	backend := testBackend(t)
	ordersID := platform.UUIDFromString("team/orders\x00.")
	stats, err := backend.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	facts, truncated, err := backend.Neighbors(context.Background(), ontology.NeighborQuery{
		EntityIDs: []string{ordersID}, Direction: ontology.DirectionBoth, Limit: 1, Generation: stats.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(facts) != 1 {
		t.Fatalf("facts=%d truncated=%v", len(facts), truncated)
	}
}

func TestBackendRejectsReadsFromReplacedGeneration(t *testing.T) {
	backend := testBackend(t)
	resolved, err := backend.Resolve(context.Background(), ontology.ResolveQuery{
		Text: "orders", Classes: []ontology.Class{ontology.ClassService}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	orders := ontologyTestService("team/orders", "orders")
	orders.Owner = "new-owner"
	bundle := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{{
			Repo: orders.Repo, HeadSHA: "orders-sha", IndexedAt: time.Now().UnixMilli(),
		}},
		Services: []domain.ServiceRecord{orders},
	}
	snapshot, err := ontology.Project(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.PublishWorkspace(context.Background(), ontology.WorkspaceSnapshot{
		Structure: bundle, Ontology: snapshot,
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err = backend.Neighbors(context.Background(), ontology.NeighborQuery{
		EntityIDs: []string{resolved.Entities[0].ID}, Direction: ontology.DirectionOutgoing,
		Limit: 10, Generation: resolved.Generation,
	})
	if !errors.Is(err, ontology.ErrStaleSnapshot) {
		t.Fatalf("old generation read error = %v", err)
	}
}

func testBackend(t *testing.T) *Backend {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	orders := ontologyTestService("team/orders", "orders")
	payments := ontologyTestService("team/payments", "payments")
	bundle := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{
			{Repo: orders.Repo, HeadSHA: "orders-sha", IndexedAt: time.Now().UnixMilli()},
			{Repo: payments.Repo, HeadSHA: "payments-sha", IndexedAt: time.Now().UnixMilli()},
		},
		Services: []domain.ServiceRecord{orders, payments},
		Dependencies: []domain.DependencyEdge{{
			CallerServiceKey: orders.ServiceKey, TargetKind: domain.DependencyTargetService,
			TargetServiceKey: payments.ServiceKey, From: orders.ServiceName, To: payments.ServiceName,
			Type: domain.EdgeHTTP, Confidence: 0.9,
			Evidence: []domain.Evidence{{Path: "repos/team/orders/client.go", Line: 8, Kind: domain.SourceCodeScan}},
		}},
	}
	snapshot, err := ontology.Project(bundle)
	if err != nil {
		t.Fatal(err)
	}
	backend := New(db)
	workspace := ontology.WorkspaceSnapshot{Structure: bundle, Ontology: snapshot}
	if _, err := backend.PublishWorkspace(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	return backend
}

func ontologyTestService(repo, name string) domain.ServiceRecord {
	return domain.ServiceRecord{
		ServiceKey: platform.UUIDFromString(repo + "\x00."), ServiceName: name, Repo: repo, ModulePath: ".",
		Layer: "server", Language: "go", Confidence: 0.9,
		Tags: []string{}, Docs: []string{}, SourceOfTruth: []string{}, Entrypoints: []domain.Evidence{}, Ports: []int{},
	}
}
