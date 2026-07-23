package ontology

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/platform"
)

var ErrUnavailable = errors.New("ontology unavailable")
var ErrStaleSnapshot = errors.New("ontology snapshot changed")

type Direction string

const (
	DirectionOutgoing Direction = "outgoing"
	DirectionIncoming Direction = "incoming"
	DirectionBoth     Direction = "both"
)

type ResolveQuery struct {
	Text    string
	Classes []Class
	Limit   int
}

type ResolveResult struct {
	Generation string
	Entities   []EntityRef
}

type EntityQuery struct {
	IDs        []string
	Generation string
}

type NeighborQuery struct {
	EntityIDs  []string
	Predicates []Predicate
	Direction  Direction
	Limit      int
	Generation string
}

type PathQuery struct {
	StartID    string
	TargetID   string
	Predicates []Predicate
	Direction  Direction
	MaxDepth   int
	MaxNodes   int
	MaxFanout  int
	Generation string
}

type Path struct {
	EntityIDs []string `json:"entity_ids"`
	Facts     []Fact   `json:"facts"`
}

type EntityRef struct {
	ID    string `json:"id"`
	Class Class  `json:"class"`
	Name  string `json:"name"`
}

type Stats struct {
	Generation  string            `json:"generation"`
	Entities    int               `json:"entities"`
	Facts       int               `json:"facts"`
	Evidence    int               `json:"evidence"`
	ByClass     map[Class]int     `json:"by_class"`
	ByPredicate map[Predicate]int `json:"by_predicate"`
}

type WorkspaceSnapshot struct {
	Structure domain.IndexBundle
	Ontology  Snapshot
}

type Repository interface {
	Resolve(context.Context, ResolveQuery) (ResolveResult, error)
	EntitiesByID(context.Context, EntityQuery) ([]EntityRef, error)
	Neighbors(context.Context, NeighborQuery) ([]Fact, bool, error)
	Stats(context.Context) (Stats, error)
}

type Publisher interface {
	PublishWorkspace(context.Context, WorkspaceSnapshot) (string, error)
}

type Backend interface {
	Repository
	Publisher
	Close() error
}

func (snapshot WorkspaceSnapshot) Generation() (string, error) {
	digest := sha256.New()
	repositories := append([]domain.RepositoryRecord(nil), snapshot.Structure.Repositories...)
	for i := range repositories {
		repositories[i].IndexedAt = 0
	}
	sort.Slice(repositories, func(i, j int) bool { return repositories[i].Repo < repositories[j].Repo })
	services := append([]domain.ServiceRecord(nil), snapshot.Structure.Services...)
	sort.Slice(services, func(i, j int) bool { return services[i].ServiceKey < services[j].ServiceKey })
	endpoints := append([]domain.EndpointRecord(nil), snapshot.Structure.Endpoints...)
	sort.Slice(endpoints, func(i, j int) bool {
		return endpointGenerationKey(endpoints[i]) < endpointGenerationKey(endpoints[j])
	})
	dependencies := append([]domain.DependencyEdge(nil), snapshot.Structure.Dependencies...)
	sort.Slice(dependencies, func(i, j int) bool {
		return dependencyGenerationKey(dependencies[i]) < dependencyGenerationKey(dependencies[j])
	})
	runbooks := append([]domain.RunbookRecord(nil), snapshot.Structure.Runbooks...)
	sort.Slice(runbooks, func(i, j int) bool {
		return runbooks[i].Repo+"\x00"+runbooks[i].ID < runbooks[j].Repo+"\x00"+runbooks[j].ID
	})
	entities := append([]Entity(nil), snapshot.Ontology.Entities...)
	sort.Slice(entities, func(i, j int) bool { return entities[i].ID < entities[j].ID })
	facts := append([]Fact(nil), snapshot.Ontology.Facts...)
	sort.Slice(facts, func(i, j int) bool { return facts[i].ID < facts[j].ID })

	payload := struct {
		Workspace             string
		Repositories          []domain.RepositoryRecord
		Services              []domain.ServiceRecord
		Endpoints             []domain.EndpointRecord
		Dependencies          []domain.DependencyEdge
		Runbooks              []domain.RunbookRecord
		OntologySchemaVersion int
		Entities              []Entity
		Facts                 []Fact
	}{
		Workspace: snapshot.Structure.Repo, Repositories: repositories, Services: services,
		Endpoints: endpoints, Dependencies: dependencies, Runbooks: runbooks,
		OntologySchemaVersion: snapshot.Ontology.SchemaVersion, Entities: entities, Facts: facts,
	}
	if err := json.NewEncoder(digest).Encode(payload); err != nil {
		return "", fmt.Errorf("encode workspace generation: %w", err)
	}
	return platform.UUIDFromString("workspace\x00" + hex.EncodeToString(digest.Sum(nil))), nil
}

func endpointGenerationKey(endpoint domain.EndpointRecord) string {
	return strings.Join([]string{
		endpoint.ServiceKey, endpoint.Method, endpoint.Path, endpoint.File, endpoint.HandlerMethod,
	}, "\x00")
}

func dependencyGenerationKey(dependency domain.DependencyEdge) string {
	target := dependency.TargetServiceKey
	if dependency.TargetKind == domain.DependencyTargetExternal {
		target = dependency.ExternalTarget
	}
	return strings.Join([]string{
		dependency.CallerServiceKey, string(dependency.TargetKind), target, string(dependency.Type),
	}, "\x00")
}

func ValidateResolveQuery(query ResolveQuery) error {
	if query.Text == "" {
		return fmt.Errorf("ontology resolve text is required")
	}
	if strings.TrimSpace(query.Text) != query.Text {
		return fmt.Errorf("ontology resolve text must be canonical")
	}
	if query.Limit < 1 || query.Limit > 20 {
		return fmt.Errorf("ontology resolve limit %d is outside [1,20]", query.Limit)
	}
	return validateClasses(query.Classes)
}

func ValidateNeighborQuery(query NeighborQuery) error {
	if len(query.EntityIDs) < 1 || len(query.EntityIDs) > 200 {
		return fmt.Errorf("ontology neighbor entity ID count %d is outside [1,200]", len(query.EntityIDs))
	}
	if query.Generation == "" {
		return fmt.Errorf("ontology neighbor generation is required")
	}
	if query.Limit < 1 || query.Limit > 200 {
		return fmt.Errorf("ontology neighbor limit %d is outside [1,200]", query.Limit)
	}
	if err := validateDirection(query.Direction); err != nil {
		return err
	}
	return validatePredicates(query.Predicates)
}

func ValidateEntityQuery(query EntityQuery) error {
	if len(query.IDs) < 1 || len(query.IDs) > 200 {
		return fmt.Errorf("ontology entity ID count %d is outside [1,200]", len(query.IDs))
	}
	if query.Generation == "" {
		return fmt.Errorf("ontology entity generation is required")
	}
	return nil
}

func ValidatePathQuery(query PathQuery) error {
	if query.StartID == "" {
		return fmt.Errorf("ontology path start ID is required")
	}
	if query.Generation == "" {
		return fmt.Errorf("ontology path generation is required")
	}
	if query.MaxDepth < 1 || query.MaxDepth > 5 {
		return fmt.Errorf("ontology path depth %d is outside [1,5]", query.MaxDepth)
	}
	if query.MaxNodes < 1 || query.MaxNodes > 500 {
		return fmt.Errorf("ontology path node budget %d is outside [1,500]", query.MaxNodes)
	}
	if query.MaxFanout < 1 || query.MaxFanout > 100 {
		return fmt.Errorf("ontology path fanout %d is outside [1,100]", query.MaxFanout)
	}
	if err := validateDirection(query.Direction); err != nil {
		return err
	}
	return validatePredicates(query.Predicates)
}

func validateDirection(direction Direction) error {
	switch direction {
	case DirectionOutgoing, DirectionIncoming, DirectionBoth:
		return nil
	default:
		return fmt.Errorf("unsupported ontology direction %q", direction)
	}
}

func validateClasses(classes []Class) error {
	for _, class := range classes {
		if _, ok := classSchema[class]; !ok {
			return fmt.Errorf("unsupported ontology class %q", class)
		}
	}
	return nil
}

func validatePredicates(predicates []Predicate) error {
	for _, predicate := range predicates {
		if _, ok := relationSchema[predicate]; !ok {
			return fmt.Errorf("unsupported ontology predicate %q", predicate)
		}
	}
	return nil
}
