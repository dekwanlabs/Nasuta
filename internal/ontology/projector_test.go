package ontology

import (
	"reflect"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestProjectBuildsDeterministicEntitiesFactsAndEvidence(t *testing.T) {
	bundle := projectionFixture()
	first, err := Project(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Services[0], bundle.Services[1] = bundle.Services[1], bundle.Services[0]
	bundle.Dependencies[0], bundle.Dependencies[1] = bundle.Dependencies[1], bundle.Dependencies[0]
	second, err := Project(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("projection changed when input order changed")
	}

	classes := map[Class]int{}
	predicates := map[Predicate]int{}
	for _, entity := range first.Entities {
		classes[entity.Class]++
	}
	for _, fact := range first.Facts {
		predicates[fact.Predicate]++
	}
	if classes[ClassService] != 2 || classes[ClassAPIEndpoint] != 1 || classes[ClassRunbook] != 1 {
		t.Fatalf("entity classes = %#v", classes)
	}
	if predicates[PredicateExposes] != 1 || predicates[PredicateDependsOn] != 1 || predicates[PredicateDocumentedBy] != 1 {
		t.Fatalf("fact predicates = %#v", predicates)
	}
	for _, fact := range first.Facts {
		if fact.Predicate == PredicateDependsOn && len(fact.Evidence) != 2 {
			t.Fatalf("dependency evidence = %#v, want merged locations", fact.Evidence)
		}
	}
}

func TestProjectDoesNotInventServiceForUnresolvedRunbook(t *testing.T) {
	bundle := projectionFixture()
	bundle.Runbooks[0].ServiceName = "missing"
	snapshot, err := Project(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range snapshot.Facts {
		if fact.Predicate == PredicateDocumentedBy {
			t.Fatalf("unexpected documented_by fact: %#v", fact)
		}
	}
}

func projectionFixture() domain.IndexBundle {
	orders := domain.ServiceRecord{ServiceKey: "orders-key", ServiceName: "orders", Repo: "team/orders", ModulePath: ".", Language: "go", Confidence: 0.9}
	payments := domain.ServiceRecord{ServiceKey: "payments-key", ServiceName: "payments", Repo: "team/payments", ModulePath: ".", Language: "go", Confidence: 0.9}
	evidenceA := domain.Evidence{Path: "repos/team/orders/client.go", Line: 10, Kind: domain.SourceCodeScan}
	evidenceB := domain.Evidence{Path: "repos/team/orders/retry.go", Line: 20, Kind: domain.SourceCodeScan}
	return domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{{Repo: "team/orders", HeadSHA: "a"}, {Repo: "team/payments", HeadSHA: "b"}},
		Services:     []domain.ServiceRecord{orders, payments},
		Endpoints: []domain.EndpointRecord{{
			ServiceKey: orders.ServiceKey, ServiceName: orders.ServiceName, Repo: orders.Repo,
			Method: "POST", Path: "/orders", HandlerMethod: "CreateOrder", File: "repos/team/orders/handler.go",
			Line: 12, Source: domain.SourceCodeScan, Confidence: 0.9,
		}},
		Dependencies: []domain.DependencyEdge{
			{CallerServiceKey: orders.ServiceKey, TargetKind: domain.DependencyTargetService, TargetServiceKey: payments.ServiceKey, Type: domain.EdgeHTTP, Evidence: []domain.Evidence{evidenceA}, Confidence: 0.8},
			{CallerServiceKey: orders.ServiceKey, TargetKind: domain.DependencyTargetService, TargetServiceKey: payments.ServiceKey, Type: domain.EdgeHTTP, Evidence: []domain.Evidence{evidenceB}, Confidence: 0.9},
		},
		Runbooks: []domain.RunbookRecord{{ID: "orders-recovery", Repo: "docs", Title: "Orders recovery", Path: "runbooks/orders.md", Scope: "flow", ServiceName: "orders", Confidence: 1}},
	}
}
