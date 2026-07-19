package graph

import (
	"sync"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/platform"
)

// Direction of a dependency walk.
type Direction string

const (
	Downstream Direction = "downstream"
	Upstream   Direction = "upstream"
	Both       Direction = "both"
)

// Graph is the in-memory service dependency graph. Nodes are services,
// edges are @FeignClient (and other) dependencies. It is safe for concurrent
// reads while being rebuilt after each incremental index pass.
type Graph struct {
	mu         sync.RWMutex
	downstream map[string][]domain.DependencyEdge // normalized(from) -> edges
	upstream   map[string][]domain.DependencyEdge // normalized(to)   -> edges
}

// New returns an empty graph.
func New() *Graph {
	return &Graph{
		downstream: map[string][]domain.DependencyEdge{},
		upstream:   map[string][]domain.DependencyEdge{},
	}
}

// Rebuild replaces the adjacency lists from the given edges.
func (g *Graph) Rebuild(edges []domain.DependencyEdge) {
	down := map[string][]domain.DependencyEdge{}
	up := map[string][]domain.DependencyEdge{}
	for _, e := range edges {
		down[platform.Normalize(e.From)] = append(down[platform.Normalize(e.From)], e)
		up[platform.Normalize(e.To)] = append(up[platform.Normalize(e.To)], e)
	}
	g.mu.Lock()
	g.downstream = down
	g.upstream = up
	g.mu.Unlock()
}

// Result is the upstream/downstream answer for trace_deps.
type Result struct {
	Upstream   []domain.DependencyEdge `json:"upstream"`
	Downstream []domain.DependencyEdge `json:"downstream"`
}

// Chain returns dependency edges up to depth hops in the requested direction.
func (g *Graph) Chain(service string, dir Direction, depth int) Result {
	g.mu.RLock()
	defer g.mu.RUnlock()
	norm := platform.Normalize(service)
	res := Result{Upstream: []domain.DependencyEdge{}, Downstream: []domain.DependencyEdge{}}
	if dir != Upstream {
		res.Downstream = g.walk(norm, Downstream, depth)
	}
	if dir != Downstream {
		res.Upstream = g.walk(norm, Upstream, depth)
	}
	return res
}

// walk performs a breadth-first traversal of edges (ports walkEdges from TS).
func (g *Graph) walk(service string, dir Direction, depth int) []domain.DependencyEdge {
	results := []domain.DependencyEdge{}
	visited := map[string]struct{}{}
	frontier := []string{service}

	adj := g.downstream
	if dir == Upstream {
		adj = g.upstream
	}

	for level := 0; level < depth; level++ {
		var next []string
		for _, current := range frontier {
			for _, edge := range adj[current] {
				key := edge.From + "->" + edge.To + ":" + string(edge.Type)
				if _, seen := visited[key]; seen {
					continue
				}
				visited[key] = struct{}{}
				results = append(results, edge)
				if dir == Downstream {
					next = append(next, platform.Normalize(edge.To))
				} else {
					next = append(next, platform.Normalize(edge.From))
				}
			}
		}
		frontier = next
	}
	return results
}
