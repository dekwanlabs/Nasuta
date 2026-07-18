package knowledge

import (
	"context"

	types "github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/internal/platform/graph"
)

// API is the stable read-only facade available to built-in and scenario tools.
type API interface {
	SearchCode(context.Context, CodeSearchQuery) (CodeSearchResult, error)
	TraceDependencies(context.Context, DependencyQuery) (DependencyResult, error)
	SearchRunbooks(context.Context, RunbookQuery) (RunbookSearchResult, error)
	SearchServices(context.Context, ServiceSearchQuery) (ServiceSearchResult, error)
}

type CodeSearchQuery struct {
	Query string
	Lang  string
	Limit int
}

type CodeSearchResult = types.SearchResult[types.CodeSearchHit]

type DependencyQuery struct {
	Service   string
	Direction string
	Depth     int
}

type DependencyResult = graph.Result

type RunbookQuery struct {
	Query string
	Limit int
}

type RunbookSearchResult = types.SearchResult[types.RunbookSearchHit]

type ServiceSearchQuery struct {
	Query string
	Limit int
}

type ServiceSearchResult = types.SearchResult[types.ServiceRecord]
