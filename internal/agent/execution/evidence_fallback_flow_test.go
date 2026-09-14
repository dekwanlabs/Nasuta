package execution

import (
	"encoding/json"
	"testing"

	"github.com/dekwanlabs/nasuta/tool"
)

func dependencyUnitFixture(section string) tool.EvidenceUnit {
	return tool.EvidenceUnit{
		SourceKind: "dependency",
		Target:     "hsds-aiot-recipe",
		Sections:   []string{section},
		Facets:     []string{"external_dependency"},
		TrustTier:  60,
	}
}

func TestFallbackDependencyEdgeParsesCallSites(t *testing.T) {
	from, to, protocol, symbols, ok := fallbackDependencyEdge(dependencyUnitFixture(
		"upstream:hsds-offline-cookbook->hsds-aiot-recipe:feign|APIFeignOnline.saveRecipe",
	))
	if !ok {
		t.Fatal("edge with a call site was rejected")
	}
	if from != "hsds-offline-cookbook" || to != "hsds-aiot-recipe" || protocol != "feign" {
		t.Fatalf("from/to/protocol = %q/%q/%q", from, to, protocol)
	}
	if len(symbols) != 1 || symbols[0] != "APIFeignOnline.saveRecipe" {
		t.Fatalf("symbols = %v, want [APIFeignOnline.saveRecipe]", symbols)
	}
}

// The four-part form predates call sites and still has to parse, reporting no
// symbols so the diagram can skip it.
func TestFallbackDependencyEdgeParsesLegacySection(t *testing.T) {
	from, to, protocol, symbols, ok := fallbackDependencyEdge(dependencyUnitFixture(
		"upstream:hsds-offline-cookbook->hsds-aiot-recipe:feign",
	))
	if !ok {
		t.Fatal("legacy four-part section was rejected")
	}
	if from != "hsds-offline-cookbook" || to != "hsds-aiot-recipe" || protocol != "feign" {
		t.Fatalf("from/to/protocol = %q/%q/%q", from, to, protocol)
	}
	if len(symbols) != 0 {
		t.Fatalf("symbols = %v, want none", symbols)
	}
}

func TestFallbackDependencyEdgeSplitsMultipleCallSites(t *testing.T) {
	_, _, _, symbols, ok := fallbackDependencyEdge(dependencyUnitFixture(
		"upstream:a->b:feign|RecipeAgent,RecipeAgent.get_recipe_detail",
	))
	if !ok {
		t.Fatal("edge rejected")
	}
	if len(symbols) != 2 {
		t.Fatalf("symbols = %v, want 2", symbols)
	}
}

// An edge naming no call site collapses every interface between the pair, so it
// may be a read or a write; drawing it can invert the flow. It must not become
// a diagram edge, while an edge with a call site must.
func TestFallbackFlowSkipsEdgesWithoutCallSites(t *testing.T) {
	flow := fallbackFlow([]tool.EvidenceUnit{
		dependencyUnitFixture("upstream:hsds-offline-cookbook->hsds-aiot-recipe:feign"),
	})
	if flow != nil {
		t.Fatalf("flow built from call-site-less edges = %v, want nil", flow)
	}

	flow = fallbackFlow([]tool.EvidenceUnit{
		dependencyUnitFixture("upstream:hsas-aiot-application->hsds-aiot-recipe:feign|RecipeAgent.get_recipe_detail"),
	})
	if flow == nil {
		t.Fatal("edge with a call site produced no flow")
	}
	encoded, err := json.Marshal(flow)
	if err != nil {
		t.Fatalf("marshal flow: %v", err)
	}
	var decoded struct {
		Nodes []struct{ ID string } `json:"nodes"`
		Edges []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal flow: %v", err)
	}
	if len(decoded.Edges) != 1 || len(decoded.Nodes) != 2 {
		t.Fatalf("nodes/edges = %d/%d, want 2/1", len(decoded.Nodes), len(decoded.Edges))
	}
}

// A mixed pool keeps only the placeable edge, so one undecidable neighbour
// cannot pull its services into the diagram as nodes.
func TestFallbackFlowKeepsOnlyPlaceableEdges(t *testing.T) {
	flow := fallbackFlow([]tool.EvidenceUnit{
		dependencyUnitFixture("upstream:hsds-offline-cookbook->hsds-aiot-recipe:feign"),
		dependencyUnitFixture("upstream:hsds-cookbook-provider->hsds-aiot-recipe:feign|ChefAIFeign.getChefAiRecipeDetail"),
	})
	if flow == nil {
		t.Fatal("flow is nil despite one placeable edge")
	}
	encoded, _ := json.Marshal(flow)
	var decoded struct {
		Nodes []struct{ ID string }       `json:"nodes"`
		Edges []struct{ From, To string } `json:"edges"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal flow: %v", err)
	}
	if len(decoded.Edges) != 1 {
		t.Fatalf("edges = %d, want 1", len(decoded.Edges))
	}
	for _, node := range decoded.Nodes {
		if node.ID == "hsds-offline-cookbook" {
			t.Fatal("a call-site-less edge contributed a node to the diagram")
		}
	}
}
