package ontology

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

type EntityRef struct {
	ID    string `json:"id"`
	Class Class  `json:"class"`
	Name  string `json:"name"`
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
	Direct     bool              `json:"direct"`
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

func (service *Service) QueryRelations(ctx context.Context, query RelationQuery) (RelationResult, error) {
	if service == nil || service.repository == nil {
		return RelationResult{}, ErrUnavailable
	}
	result, err := service.queryRelations(ctx, query)
	if errors.Is(err, ErrStaleSnapshot) {
		return service.queryRelations(ctx, query)
	}
	return result, err
}

func (service *Service) queryRelations(ctx context.Context, query RelationQuery) (RelationResult, error) {
	stats, err := service.repository.Stats(ctx)
	if err != nil {
		return RelationResult{}, err
	}
	classes := []Class(nil)
	if query.EntityClass != "" {
		if _, ok := classSchema[query.EntityClass]; !ok {
			return RelationResult{}, fmt.Errorf("unsupported ontology class %q", query.EntityClass)
		}
		classes = []Class{query.EntityClass}
	}
	for _, predicate := range query.Predicates {
		if _, ok := relationSchema[predicate]; !ok {
			return RelationResult{}, fmt.Errorf("unsupported ontology predicate %q", predicate)
		}
	}
	resolved, err := service.repository.Resolve(ctx, ResolveQuery{Text: query.Entity, Classes: classes, Limit: 20, Generation: stats.Generation})
	if err != nil {
		return RelationResult{}, err
	}
	result := RelationResult{Entities: []EntityRef{}, Facts: []RelationFact{}}
	if len(resolved) != 1 {
		result.Candidates = entityRefs(resolved)
		return result, nil
	}
	root := entityRef(resolved[0])
	result.Root = &root
	paths, truncated, err := service.repository.FindPaths(ctx, PathQuery{
		StartID: root.ID, Predicates: query.Predicates, Direction: query.Direction,
		MaxDepth: query.MaxDepth, MaxNodes: query.MaxNodes, MaxFanout: query.MaxFanout, Generation: stats.Generation,
	})
	if err != nil {
		return RelationResult{}, err
	}
	result.Truncated = truncated
	return service.hydrateResult(ctx, stats.Generation, result, paths)
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
	entities, err := service.repository.EntitiesByID(ctx, EntityQuery{IDs: ids, Generation: generation})
	if err != nil {
		return RelationResult{}, err
	}
	byID := make(map[string]EntityRef, len(entities)+1)
	if result.Root != nil {
		byID[result.Root.ID] = *result.Root
	}
	for _, entity := range entities {
		ref := entityRef(entity)
		byID[entity.ID] = ref
		result.Entities = append(result.Entities, ref)
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
			Depth: item.depth, Direct: true,
		})
	}
	return result, nil
}

type factDepth struct {
	fact  Fact
	depth int
}

func entityRefs(entities []Entity) []EntityRef {
	refs := make([]EntityRef, 0, len(entities))
	for _, entity := range entities {
		refs = append(refs, entityRef(entity))
	}
	return refs
}

func entityRef(entity Entity) EntityRef {
	return EntityRef{ID: entity.ID, Class: entity.Class, Name: entity.Name}
}
