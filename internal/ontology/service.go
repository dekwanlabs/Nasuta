package ontology

import (
	"context"
	"errors"
	"sort"
	"strings"
)

type RelationQuery struct {
	Entity      string
	EntityClass Class
	Predicates  []Predicate
	Direction   Direction
	MaxDepth    int
	MaxNodes    int
	MaxFanout   int
}

type RelationFact struct {
	ID         string            `json:"id"`
	Subject    EntityRef         `json:"subject"`
	Predicate  Predicate         `json:"predicate"`
	Object     EntityRef         `json:"object"`
	Qualifiers map[string]string `json:"qualifiers"`
	Confidence float64           `json:"confidence"`
	Evidence   []Evidence        `json:"evidence"`
	Depth      int               `json:"depth"`
}

type RelationResult struct {
	Root       *EntityRef     `json:"root,omitempty"`
	Candidates []EntityRef    `json:"candidates,omitempty"`
	Entities   []EntityRef    `json:"entities"`
	Facts      []RelationFact `json:"facts"`
	Truncated  bool           `json:"truncated"`
}

type DependencyQuery struct {
	Service   string
	Direction Direction
	MaxDepth  int
	MaxNodes  int
	MaxFanout int
}

type DependencyResult struct {
	Root       *EntityRef     `json:"root,omitempty"`
	Candidates []EntityRef    `json:"candidates,omitempty"`
	Upstream   []RelationFact `json:"upstream"`
	Downstream []RelationFact `json:"downstream"`
	Truncated  bool           `json:"truncated"`
}

type Service struct{ repository Repository }

func NewService(repository Repository) *Service {
	if repository == nil {
		return nil
	}
	return &Service{repository: repository}
}

func (service *Service) Stats(ctx context.Context) (Stats, error) {
	return service.repository.Stats(ctx)
}

func (service *Service) QueryRelations(ctx context.Context, query RelationQuery) (RelationResult, error) {
	query = canonicalRelationQuery(query)
	result, err := service.queryRelations(ctx, query)
	if errors.Is(err, ErrStaleSnapshot) {
		return service.queryRelations(ctx, query)
	}
	return result, err
}

func (service *Service) queryRelations(ctx context.Context, query RelationQuery) (RelationResult, error) {
	classes := []Class(nil)
	if query.EntityClass != "" {
		classes = []Class{query.EntityClass}
	}
	resolved, err := service.repository.Resolve(ctx, ResolveQuery{Text: query.Entity, Classes: classes, Limit: 20})
	if err != nil {
		return RelationResult{}, err
	}
	return service.queryResolvedRelations(ctx, resolved, query)
}

func (service *Service) queryResolvedRelations(ctx context.Context, resolved ResolveResult, query RelationQuery) (RelationResult, error) {
	result := RelationResult{Entities: []EntityRef{}, Facts: []RelationFact{}}
	if len(resolved.Entities) != 1 {
		result.Candidates = resolved.Entities
		return result, nil
	}
	root := resolved.Entities[0]
	result.Root = &root
	paths, truncated, err := FindBoundedPaths(ctx, service.repository, PathQuery{
		StartID: root.ID, Predicates: query.Predicates, Direction: query.Direction,
		MaxDepth: query.MaxDepth, MaxNodes: query.MaxNodes, MaxFanout: query.MaxFanout, Generation: resolved.Generation,
	})
	if err != nil {
		return RelationResult{}, err
	}
	result.Truncated = truncated
	return service.hydrateResult(ctx, resolved.Generation, result, paths)
}

func (service *Service) TraceDependencies(ctx context.Context, query DependencyQuery) (DependencyResult, error) {
	query.Service = strings.TrimSpace(query.Service)
	query.Direction = Direction(strings.ToLower(strings.TrimSpace(string(query.Direction))))
	result, err := service.traceDependencies(ctx, query)
	if errors.Is(err, ErrStaleSnapshot) {
		return service.traceDependencies(ctx, query)
	}
	return result, err
}

func (service *Service) traceDependencies(ctx context.Context, query DependencyQuery) (DependencyResult, error) {
	if err := validateDirection(query.Direction); err != nil {
		return DependencyResult{}, err
	}
	resolved, err := service.repository.Resolve(ctx, ResolveQuery{
		Text: query.Service, Classes: []Class{ClassService}, Limit: 20,
	})
	if err != nil {
		return DependencyResult{}, err
	}
	result := DependencyResult{
		Candidates: resolved.Entities, Upstream: []RelationFact{}, Downstream: []RelationFact{},
	}
	if len(resolved.Entities) != 1 {
		return result, nil
	}
	root := resolved.Entities[0]
	result.Root = &root
	result.Candidates = nil
	queryRelations := func(direction Direction) (RelationResult, error) {
		return service.queryResolvedRelations(ctx, resolved, RelationQuery{
			Entity: query.Service, EntityClass: ClassService,
			Predicates: []Predicate{PredicateDependsOn}, Direction: direction,
			MaxDepth: query.MaxDepth, MaxNodes: query.MaxNodes, MaxFanout: query.MaxFanout,
		})
	}
	if query.Direction != DirectionOutgoing {
		upstream, err := queryRelations(DirectionIncoming)
		if err != nil {
			return DependencyResult{}, err
		}
		result.Upstream = upstream.Facts
		result.Truncated = result.Truncated || upstream.Truncated
	}
	if query.Direction != DirectionIncoming {
		downstream, err := queryRelations(DirectionOutgoing)
		if err != nil {
			return DependencyResult{}, err
		}
		result.Downstream = downstream.Facts
		result.Truncated = result.Truncated || downstream.Truncated
	}
	return result, nil
}

func (service *Service) hydrateResult(ctx context.Context, generation string, result RelationResult, paths []Path) (RelationResult, error) {
	entityIDs := make(map[string]struct{})
	facts := make(map[string]factDepth)
	for _, path := range paths {
		for _, id := range path.EntityIDs {
			if result.Root == nil || id != result.Root.ID {
				entityIDs[id] = struct{}{}
			}
		}
		for index, fact := range path.Facts {
			depth := index + 1
			current, exists := facts[fact.ID]
			if !exists || depth < current.depth {
				facts[fact.ID] = factDepth{fact: fact, depth: depth}
			}
		}
	}
	ids := make([]string, 0, len(entityIDs))
	for id := range entityIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		entities, err := service.repository.EntitiesByID(ctx, EntityQuery{IDs: ids, Generation: generation})
		if err != nil {
			return RelationResult{}, err
		}
		result.Entities = append(result.Entities, entities...)
	}
	byID := make(map[string]EntityRef, len(result.Entities)+1)
	if result.Root != nil {
		byID[result.Root.ID] = *result.Root
	}
	for _, entity := range result.Entities {
		byID[entity.ID] = entity
	}
	orderedFacts := make([]factDepth, 0, len(facts))
	for _, fact := range facts {
		orderedFacts = append(orderedFacts, fact)
	}
	sort.Slice(orderedFacts, func(i, j int) bool {
		if orderedFacts[i].depth != orderedFacts[j].depth {
			return orderedFacts[i].depth < orderedFacts[j].depth
		}
		return orderedFacts[i].fact.ID < orderedFacts[j].fact.ID
	})
	for _, item := range orderedFacts {
		result.Facts = append(result.Facts, RelationFact{
			ID: item.fact.ID, Subject: byID[item.fact.SubjectID], Predicate: item.fact.Predicate,
			Object: byID[item.fact.ObjectID], Qualifiers: item.fact.Qualifiers,
			Confidence: item.fact.Confidence, Evidence: item.fact.Evidence,
			Depth: item.depth,
		})
	}
	return result, nil
}

func canonicalRelationQuery(query RelationQuery) RelationQuery {
	query.Entity = strings.TrimSpace(query.Entity)
	query.EntityClass = Class(strings.ToLower(strings.TrimSpace(string(query.EntityClass))))
	query.Direction = Direction(strings.ToLower(strings.TrimSpace(string(query.Direction))))
	predicates := make([]Predicate, 0, len(query.Predicates))
	seen := make(map[Predicate]struct{}, len(query.Predicates))
	for _, predicate := range query.Predicates {
		predicate = Predicate(strings.ToLower(strings.TrimSpace(string(predicate))))
		if _, duplicate := seen[predicate]; duplicate {
			continue
		}
		seen[predicate] = struct{}{}
		predicates = append(predicates, predicate)
	}
	query.Predicates = predicates
	return query
}

type factDepth struct {
	fact  Fact
	depth int
}
