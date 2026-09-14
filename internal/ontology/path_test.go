package ontology

import (
	"context"
	"testing"
)

type pathRepository struct{ facts []Fact }

func (repository pathRepository) Neighbors(_ context.Context, query NeighborQuery) ([]Fact, bool, error) {
	frontier := make(map[string]struct{}, len(query.EntityIDs))
	for _, id := range query.EntityIDs {
		frontier[id] = struct{}{}
	}
	out := make([]Fact, 0)
	for _, fact := range repository.facts {
		_, subject := frontier[fact.SubjectID]
		_, object := frontier[fact.ObjectID]
		if (query.Direction != DirectionIncoming && subject) || (query.Direction != DirectionOutgoing && object) {
			out = append(out, fact)
		}
	}
	return out, false, nil
}

func TestFindBoundedPathsStopsAtCyclesAndPreservesDirectFacts(t *testing.T) {
	repository := pathRepository{facts: []Fact{
		{ID: "ab", SubjectID: "a", ObjectID: "b", Predicate: PredicateDependsOn},
		{ID: "bc", SubjectID: "b", ObjectID: "c", Predicate: PredicateDependsOn},
		{ID: "ca", SubjectID: "c", ObjectID: "a", Predicate: PredicateDependsOn},
	}}
	paths, truncated, err := FindBoundedPaths(context.Background(), repository, PathQuery{
		StartID: "a", Direction: DirectionOutgoing, MaxDepth: 5, MaxNodes: 10, MaxFanout: 10, Generation: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(paths) != 2 {
		t.Fatalf("paths=%#v truncated=%v", paths, truncated)
	}
	if len(paths[1].Facts) != 2 || paths[1].Facts[0].ID != "ab" || paths[1].Facts[1].ID != "bc" {
		t.Fatalf("path facts = %#v", paths[1].Facts)
	}
}

func TestFindBoundedPathsPreservesParallelFactsAtTheSameDepth(t *testing.T) {
	repository := pathRepository{facts: []Fact{
		{ID: "http", SubjectID: "orders", ObjectID: "payments", Predicate: PredicateDependsOn, Qualifiers: map[string]string{"protocol": "http"}},
		{ID: "kafka", SubjectID: "orders", ObjectID: "payments", Predicate: PredicateDependsOn, Qualifiers: map[string]string{"protocol": "kafka"}},
	}}
	paths, truncated, err := FindBoundedPaths(context.Background(), repository, PathQuery{
		StartID: "orders", Direction: DirectionOutgoing, MaxDepth: 2, MaxNodes: 10, MaxFanout: 10, Generation: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(paths) != 2 {
		t.Fatalf("paths=%#v truncated=%v", paths, truncated)
	}
	if paths[0].Facts[0].ID != "http" || paths[1].Facts[0].ID != "kafka" {
		t.Fatalf("parallel facts = %#v", paths)
	}
}

func TestFindBoundedPathsScopePrunesCrossBusinessFanout(t *testing.T) {
	repository := pathRepository{facts: []Fact{
		{ID: "ab", SubjectID: "a", ObjectID: "b", Predicate: PredicateDependsOn},
		{ID: "ah", SubjectID: "a", ObjectID: "hub", Predicate: PredicateDependsOn},
		{ID: "bc", SubjectID: "b", ObjectID: "c", Predicate: PredicateDependsOn},
		{ID: "ho", SubjectID: "hub", ObjectID: "other", Predicate: PredicateDependsOn},
	}}
	paths, _, err := FindBoundedPaths(context.Background(), repository, PathQuery{
		StartID: "a", Direction: DirectionOutgoing, MaxDepth: 3, MaxNodes: 10, MaxFanout: 10, Generation: "test",
		Scope: map[string]struct{}{"a": {}, "b": {}, "c": {}},
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, path := range paths {
		for _, fact := range path.Facts {
			seen[fact.ID] = true
		}
	}
	// Direct edges to both the in-scope node (b) and the cross-business hub are
	// kept; the in-scope node is expanded (bc), the hub's fan-out is pruned (ho).
	if !seen["ab"] || !seen["ah"] || !seen["bc"] {
		t.Fatalf("missing expected facts: %v", seen)
	}
	if seen["ho"] {
		t.Fatalf("cross-business fan-out was followed: %v", seen)
	}
}
