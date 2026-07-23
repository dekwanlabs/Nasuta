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
		StartID: "a", Direction: DirectionOutgoing, MaxDepth: 5, MaxNodes: 10, MaxFanout: 10,
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
