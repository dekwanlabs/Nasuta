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
		for _, entity := range entities {
			result.Entities = append(result.Entities, entity)
		}
	}
	byID := make(map[string]EntityRef, len(result.Entities)+1)
	if result.Root != nil {
		byID[result.Root.ID] = *result.Root
	}
	for _, entity := range result.Entities {
		byID[entity.ID] = entity
	}
	factIDs := make([]string, 0, len(facts))
	for id := range facts {
		factIDs = append(factIDs, id)
	}
	sort.Strings(factIDs)
	for _, id := range factIDs {
		item := facts[id]
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
