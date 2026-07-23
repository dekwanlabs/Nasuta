package ontology

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/platform"
)

var ErrUnavailable = errors.New("ontology unavailable")

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

type NeighborQuery struct {
	EntityIDs  []string
	Predicates []Predicate
	Direction  Direction
	Limit      int
}

type PathQuery struct {
	StartID    string
	TargetID   string
	Predicates []Predicate
	Direction  Direction
	MaxDepth   int
	MaxNodes   int
	MaxFanout  int
}

type Path struct {
	EntityIDs []string `json:"entity_ids"`
	Facts     []Fact   `json:"facts"`
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
	Generation string
	Structure  domain.IndexBundle
	Ontology   Snapshot
}

type Repository interface {
	Resolve(context.Context, ResolveQuery) ([]Entity, error)
	Neighbors(context.Context, NeighborQuery) ([]Fact, bool, error)
	FindPaths(context.Context, PathQuery) ([]Path, bool, error)
	Stats(context.Context) (Stats, error)
	Close() error
}

type Publisher interface {
	PublishWorkspace(context.Context, WorkspaceSnapshot) error
}

type Backend interface {
	Repository
	Publisher
}

func GenerationFor(repositories []domain.RepositoryRecord) string {
	seeds := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		seeds = append(seeds, repository.Repo+"\x00"+repository.HeadSHA)
	}
	sort.Strings(seeds)
	return platform.UUIDFromString("workspace\x00" + strings.Join(seeds, "\x00"))
}

func ValidateResolveQuery(query ResolveQuery) error {
	if query.Text == "" {
		return fmt.Errorf("ontology resolve text is required")
	}
	if query.Limit < 1 || query.Limit > 20 {
		return fmt.Errorf("ontology resolve limit %d is outside [1,20]", query.Limit)
	}
	return nil
}

func ValidateNeighborQuery(query NeighborQuery) error {
	if len(query.EntityIDs) == 0 {
		return fmt.Errorf("ontology neighbor entity IDs are required")
	}
	if query.Limit < 1 || query.Limit > 200 {
		return fmt.Errorf("ontology neighbor limit %d is outside [1,200]", query.Limit)
	}
	return validateDirection(query.Direction)
}

func ValidatePathQuery(query PathQuery) error {
	if query.StartID == "" {
		return fmt.Errorf("ontology path start ID is required")
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
	return validateDirection(query.Direction)
}

func validateDirection(direction Direction) error {
	switch direction {
	case DirectionOutgoing, DirectionIncoming, DirectionBoth:
		return nil
	default:
		return fmt.Errorf("unsupported ontology direction %q", direction)
	}
}
