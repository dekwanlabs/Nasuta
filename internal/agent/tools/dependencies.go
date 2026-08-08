package tools

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/knowledge"
)

// TraceDependencies exposes evidence-backed ontology dependencies without tool JSON.
func (srv *Service) TraceDependencies(ctx context.Context, query knowledge.DependencyQuery) (knowledge.DependencyResult, error) {
	if query.Direction == "" {
		query.Direction = "both"
	}
	depth := clampInt(query.Depth, 1, 5)
	result, err := srv.TraceDeps(ctx, query.Service, query.Direction, depth)
	if err != nil {
		return knowledge.DependencyResult{}, err
	}
	return toDependencyResult(result), nil
}

func (srv *Service) TraceDeps(ctx context.Context, service, direction string, depth int) (domain.DependencyTrace, error) {
	if srv.ontology == nil {
		return domain.DependencyTrace{}, ontology.ErrUnavailable
	}
	ontologyDirection := ontology.DirectionBoth
	switch direction {
	case "upstream":
		ontologyDirection = ontology.DirectionIncoming
	case "downstream":
		ontologyDirection = ontology.DirectionOutgoing
	case "both":
	default:
		return domain.DependencyTrace{}, fmt.Errorf("unsupported dependency direction %q", direction)
	}
	result, err := srv.ontology.TraceDependencies(ctx, ontology.DependencyQuery{
		Service: service, Direction: ontologyDirection,
		MaxDepth: depth, MaxNodes: 500, MaxFanout: 100,
	})
	if err != nil {
		return domain.DependencyTrace{}, err
	}
	return dependencyTrace(result), nil
}

func dependencyTrace(result ontology.DependencyResult) domain.DependencyTrace {
	convert := func(facts []ontology.RelationFact) []domain.DependencyEdge {
		edges := make([]domain.DependencyEdge, 0, len(facts))
		for _, fact := range facts {
			evidence := make([]domain.Evidence, 0, len(fact.Evidence))
			for _, item := range fact.Evidence {
				evidence = append(evidence, domain.Evidence{
					Path: item.Path, Line: item.Line, Symbol: item.Symbol,
					Kind: domain.SourceKind(item.Source),
				})
			}
			edge := domain.DependencyEdge{
				CallerServiceKey: fact.Subject.ID,
				From:             fact.Subject.Name,
				To:               fact.Object.Name,
				Type:             domain.EdgeType(fact.Qualifiers["protocol"]),
				Evidence:         evidence,
				Confidence:       fact.Confidence,
			}
			if fact.Object.Class == ontology.ClassExternalSystem {
				edge.TargetKind = domain.DependencyTargetExternal
				edge.ExternalTarget = fact.Object.Name
			} else {
				edge.TargetKind = domain.DependencyTargetService
				edge.TargetServiceKey = fact.Object.ID
			}
			edges = append(edges, edge)
		}
		return edges
	}
	trace := domain.DependencyTrace{
		Upstream: convert(result.Upstream), Downstream: convert(result.Downstream),
		Truncated: result.Truncated,
	}
	if result.Root != nil {
		trace.Service = result.Root.Name
	}
	trace.Candidates = make([]string, 0, len(result.Candidates))
	for _, candidate := range result.Candidates {
		trace.Candidates = append(trace.Candidates, candidate.Name)
	}
	return trace
}

func toDependencyResult(found domain.DependencyTrace) knowledge.DependencyResult {
	convert := func(edges []domain.DependencyEdge) []knowledge.DependencyEdge {
		result := make([]knowledge.DependencyEdge, 0, len(edges))
		for _, edge := range edges {
			result = append(result, knowledge.DependencyEdge{
				From:           edge.From,
				To:             edge.To,
				Type:           string(edge.Type),
				ExternalTarget: edge.ExternalTarget,
				Confidence:     edge.Confidence,
			})
		}
		return result
	}
	return knowledge.DependencyResult{
		Service: found.Service, Candidates: found.Candidates,
		Upstream: convert(found.Upstream), Downstream: convert(found.Downstream),
		Truncated: found.Truncated,
	}
}
