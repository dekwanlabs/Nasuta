package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/ontology"
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
	if err := db.ReplaceStructure(context.Background(), "first", bundle); err != nil {
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
	if err := db.ReplaceStructure(context.Background(), "replacement", replacement); err != nil {
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

func TestReplaceWorkspacePublishesStructureAndOntologyGeneration(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	service := testService("team/orders", ".", "orders")
	bundle := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{{Repo: service.Repo, HeadSHA: "sha", IndexedAt: time.Now().UnixMilli()}},
		Services:     []domain.ServiceRecord{service},
	}
	snapshot, err := ontology.Project(bundle)
	if err != nil {
		t.Fatal(err)
	}
	generation := ontology.GenerationFor(bundle.Repositories)
	if err := db.ReplaceWorkspace(context.Background(), generation, bundle, snapshot); err != nil {
		t.Fatal(err)
	}

	gotGeneration, schemaVersion, err := db.WorkspaceGeneration(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotGeneration != generation || schemaVersion != ontology.CurrentSchemaVersion {
		t.Fatalf("generation=%q schema=%d", gotGeneration, schemaVersion)
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	var entities, facts int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM ontology_entities`).Scan(&entities); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM ontology_facts`).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if entities != 2 || facts != 1 {
		t.Fatalf("entities=%d facts=%d, want 2/1", entities, facts)
	}
}

func TestReplaceWorkspaceRejectsInvalidOntology(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := testService("team/orders", ".", "orders")
	bundle := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{{Repo: service.Repo, HeadSHA: "sha", IndexedAt: time.Now().UnixMilli()}},
		Services:     []domain.ServiceRecord{service},
	}
	snapshot := ontology.Snapshot{
		SchemaVersion: ontology.CurrentSchemaVersion,
		Entities: []ontology.Entity{{
			ID: "invalid", Class: ontology.ClassService, Key: service.ServiceKey,
			Name: service.ServiceName, Confidence: service.Confidence,
		}},
	}

	if err := db.ReplaceWorkspace(context.Background(), "invalid-ontology", bundle, snapshot); err == nil {
		t.Fatal("ReplaceWorkspace accepted ontology without projected entities")
	}
}

func TestReplaceAllFailureKeepsPublishedSnapshot(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	service := testService("team/orders", ".", "orders")
	current := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{{Repo: service.Repo, HeadSHA: "current-sha", IndexedAt: time.Now().UnixMilli()}},
		Services:     []domain.ServiceRecord{service},
	}
	if err := db.ReplaceStructure(context.Background(), "current", current); err != nil {
		t.Fatal(err)
	}

	invalid := current
	invalid.Repositories = []domain.RepositoryRecord{{Repo: service.Repo, HeadSHA: "", IndexedAt: time.Now().UnixMilli()}}
	if err := db.ReplaceStructure(context.Background(), "invalid", invalid); err == nil {
		t.Fatal("invalid replacement unexpectedly succeeded")
	}

	sha, err := db.GetIndexSHA(context.Background(), service.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if sha != "current-sha" {
		t.Fatalf("published sha = %q, want current-sha", sha)
	}
}

func TestServiceForPathUsesLongestModulePrefix(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := "team/mono"
	root := testService(repo, ".", "mono")
	api := testService(repo, "apps/api", "api")
	admin := testService(repo, "apps/api/admin", "admin")
	bundle := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{{Repo: repo, HeadSHA: "sha", IndexedAt: time.Now().UnixMilli()}},
		Services:     []domain.ServiceRecord{root, api, admin},
	}
	if err := db.ReplaceStructure(context.Background(), "modules", bundle); err != nil {
		t.Fatal(err)
	}

	service, err := db.ServiceForPath(context.Background(), "repos/team/mono/apps/api/admin/src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if service.ServiceName != "admin" {
		t.Fatalf("service=%q, want longest-prefix admin module", service.ServiceName)
	}
	service, err = db.ServiceForPath(context.Background(), "repos/team/mono/README.md")
	if err != nil || service.ServiceName != "mono" {
		t.Fatalf("root service=%q err=%v", service.ServiceName, err)
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
