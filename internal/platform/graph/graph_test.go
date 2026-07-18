package graph

import (
	"testing"

	"github.com/dekwanlabs/astris/internal/domain"
)

func edge(from, to string) types.DependencyEdge {
	return types.DependencyEdge{From: from, To: to, Type: types.EdgeFeign, Confidence: 0.9}
}

func newTestGraph() *Graph {
	g := New()
	// a -> b -> c -> d ; e -> b
	g.Rebuild([]types.DependencyEdge{
		edge("a", "b"), edge("b", "c"), edge("c", "d"), edge("e", "b"),
	})
	return g
}

func TestChainDownstreamDepth(t *testing.T) {
	g := newTestGraph()
	if got := g.Chain("a", Downstream, 1); len(got.Downstream) != 1 {
		t.Fatalf("depth1 downstream = %d, want 1", len(got.Downstream))
	}
	got := g.Chain("a", Downstream, 2)
	if len(got.Downstream) != 2 {
		t.Fatalf("depth2 downstream = %d, want 2 (a->b, b->c)", len(got.Downstream))
	}
	if len(got.Upstream) != 0 {
		t.Fatalf("downstream-only should have no upstream, got %d", len(got.Upstream))
	}
}

func TestChainUpstream(t *testing.T) {
	g := newTestGraph()
	// who reaches b within 1 hop upstream: a, e
	got := g.Chain("b", Upstream, 1)
	if len(got.Upstream) != 2 {
		t.Fatalf("upstream depth1 of b = %d, want 2 (a->b, e->b)", len(got.Upstream))
	}
}

func TestChainBoth(t *testing.T) {
	g := newTestGraph()
	got := g.Chain("b", Both, 1)
	if len(got.Downstream) != 1 || len(got.Upstream) != 2 {
		t.Fatalf("both depth1 of b = down %d up %d, want down 1 up 2",
			len(got.Downstream), len(got.Upstream))
	}
}

func TestChainNoCycleExplosion(t *testing.T) {
	g := New()
	g.Rebuild([]types.DependencyEdge{edge("a", "b"), edge("b", "a")})
	// depth 5 over a 2-cycle must terminate and not duplicate edges
	got := g.Chain("a", Downstream, 5)
	if len(got.Downstream) != 2 {
		t.Fatalf("cycle downstream = %d, want 2 unique edges", len(got.Downstream))
	}
}

func TestChainNormalization(t *testing.T) {
	g := New()
	g.Rebuild([]types.DependencyEdge{edge("Hsas_App_User", "hsds-user-provider")})
	// query with different casing/separators should still resolve
	got := g.Chain("hsas-app-user", Downstream, 1)
	if len(got.Downstream) != 1 {
		t.Fatalf("normalized lookup = %d, want 1", len(got.Downstream))
	}
}
