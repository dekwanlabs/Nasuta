package ontology

import (
	"context"
	"fmt"
	"testing"
)

type retryRepository struct {
	resolveCalls int
}

func (repository *retryRepository) Resolve(_ context.Context, query ResolveQuery) (ResolveResult, error) {
	repository.resolveCalls++
	if query.Text != "Orders" || len(query.Classes) != 1 || query.Classes[0] != ClassService {
		return ResolveResult{}, fmt.Errorf("unexpected resolve query: %+v", query)
	}
	generation := "g1"
	if repository.resolveCalls == 2 {
		generation = "g2"
	}
	return ResolveResult{
		Generation: generation,
		Entities:   []EntityRef{{ID: "orders", Class: ClassService, Name: "orders"}},
	}, nil
}

func (repository *retryRepository) EntitiesByID(_ context.Context, query EntityQuery) ([]EntityRef, error) {
	if query.Generation != "g2" {
		return nil, ErrStaleSnapshot
	}
	return []EntityRef{{ID: "payments", Class: ClassService, Name: "payments"}}, nil
}

func (repository *retryRepository) Neighbors(_ context.Context, query NeighborQuery) ([]Fact, bool, error) {
	if query.Generation == "g1" {
		return nil, false, ErrStaleSnapshot
	}
	if len(query.Predicates) != 1 || query.Predicates[0] != PredicateDependsOn || query.Direction != DirectionOutgoing {
		return nil, false, fmt.Errorf("unexpected neighbor query: %+v", query)
	}
	return []Fact{{
		ID: "dependency", SubjectID: "orders", Predicate: PredicateDependsOn, ObjectID: "payments",
	}}, false, nil
}

func (*retryRepository) Stats(context.Context) (Stats, error) {
	panic("relation queries must not load stats")
}

func TestQueryRelationsCanonicalizesOnceAndRetriesStaleSnapshot(t *testing.T) {
	repository := &retryRepository{}
	predicates := []Predicate{" DEPENDS_ON ", PredicateDependsOn}
	result, err := NewService(repository).QueryRelations(context.Background(), RelationQuery{
		Entity: " Orders ", EntityClass: " SERVICE ", Predicates: predicates, Direction: " OUTGOING ",
		MaxDepth: 2, MaxNodes: 20, MaxFanout: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if repository.resolveCalls != 2 {
		t.Fatalf("resolve calls = %d, want 2", repository.resolveCalls)
	}
	if len(result.Facts) != 1 || result.Facts[0].Object.Name != "payments" || result.Facts[0].Depth != 1 {
		t.Fatalf("relation result = %#v", result)
	}
	if predicates[0] != " DEPENDS_ON " {
		t.Fatalf("caller predicates were mutated: %#v", predicates)
	}
}
