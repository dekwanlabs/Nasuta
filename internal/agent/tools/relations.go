package tools

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/ontology"
)

// TraceRelationsResult is the shared typed entry point for the MCP tool and
// the dashboard HTTP fallback. Keeping the ontology query here prevents the
// two transports from drifting in defaults, bounds, or error handling.
func (srv *Service) TraceRelationsResult(ctx context.Context, query ontology.RelationQuery) (ontology.RelationResult, error) {
	if srv.ontology == nil {
		return ontology.RelationResult{}, fmt.Errorf("ontology unavailable")
	}
	if query.Direction == "" {
		query.Direction = ontology.DirectionOutgoing
	}
	if query.MaxDepth <= 0 {
		query.MaxDepth = 2
	}
	if query.MaxDepth > 5 {
		query.MaxDepth = 5
	}
	if query.MaxNodes <= 0 {
		query.MaxNodes = 50
	}
	if query.MaxNodes > 500 {
		query.MaxNodes = 500
	}
	if query.MaxFanout <= 0 {
		query.MaxFanout = 20
	}
	if query.MaxFanout > 100 {
		query.MaxFanout = 100
	}
	return srv.ontology.QueryRelations(ctx, query)
}
