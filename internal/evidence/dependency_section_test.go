package evidence

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/tool"
)

func dependencyEdgeFixture(symbols ...string) domain.DependencyEdge {
	items := make([]domain.Evidence, 0, len(symbols))
	for _, symbol := range symbols {
		items = append(items, domain.Evidence{
			Path: "repos/x/Caller.java", Line: 12, Symbol: symbol, Kind: "code-scan",
		})
	}
	if len(items) == 0 {
		items = append(items, domain.Evidence{
			Path: "config-center/na/application/x/api.url", Kind: "config",
		})
	}
	return domain.DependencyEdge{
		From: "hsds-offline-cookbook", To: "hsds-aiot-recipe",
		Type: "feign", Evidence: items, Confidence: 0.9,
	}
}

func TestDependencyUnitSectionCarriesCallSites(t *testing.T) {
	unit, ok := DependencyUnit(
		"hsds-aiot-recipe", "upstream",
		dependencyEdgeFixture("APIFeignOnline.saveRecipe", "APIFeignOnline"),
		tool.EvidenceCoverage{Included: 1},
	)
	if !ok {
		t.Fatal("DependencyUnit rejected an edge with source provenance")
	}
	section := unit.Sections[0]
	want := "upstream:hsds-offline-cookbook->hsds-aiot-recipe:feign|APIFeignOnline,APIFeignOnline.saveRecipe"
	if section != want {
		t.Fatalf("section = %q, want %q", section, want)
	}
}

// A config-derived edge names no call site, so the section must stay at its
// original four-part form rather than gain an empty suffix.
func TestDependencyUnitSectionOmitsEmptySymbolSuffix(t *testing.T) {
	unit, ok := DependencyUnit(
		"hsds-aiot-recipe", "upstream", dependencyEdgeFixture(),
		tool.EvidenceCoverage{Included: 1},
	)
	if !ok {
		t.Fatal("DependencyUnit rejected a config-backed edge")
	}
	if section := unit.Sections[0]; strings.Contains(section, "|") {
		t.Fatalf("section = %q, want no symbol suffix", section)
	}
}

// The section feeds ContentHash, so a reordered evidence slice describing the
// same edge must produce a byte-identical section. (ContentHash itself also
// covers the raw evidence slice, whose order this function does not normalize.)
func TestDependencyUnitSectionSymbolOrderStable(t *testing.T) {
	forward, _ := DependencyUnit(
		"hsds-aiot-recipe", "upstream",
		dependencyEdgeFixture("b.call", "a.call", "c.call"),
		tool.EvidenceCoverage{Included: 1},
	)
	reversed, _ := DependencyUnit(
		"hsds-aiot-recipe", "upstream",
		dependencyEdgeFixture("c.call", "a.call", "b.call"),
		tool.EvidenceCoverage{Included: 1},
	)
	if forward.Sections[0] != reversed.Sections[0] {
		t.Fatalf("section order unstable: %q vs %q", forward.Sections[0], reversed.Sections[0])
	}
	if !strings.HasSuffix(forward.Sections[0], "|a.call,b.call,c.call") {
		t.Fatalf("section = %q, want sorted symbol suffix", forward.Sections[0])
	}
}

func TestFormatEdgeSymbolsBoundsCallSites(t *testing.T) {
	items := make([]domain.Evidence, 0, maxEdgeSymbols+4)
	for _, symbol := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		items = append(items, domain.Evidence{Path: "p", Symbol: symbol, Kind: "code-scan"})
	}
	suffix := formatEdgeSymbols(items)
	if got := strings.Count(suffix, ",") + 1; got != maxEdgeSymbols {
		t.Fatalf("symbol count = %d, want %d (suffix %q)", got, maxEdgeSymbols, suffix)
	}
}

func TestFormatEdgeSymbolsDedupes(t *testing.T) {
	suffix := formatEdgeSymbols([]domain.Evidence{
		{Path: "p", Symbol: "RecipeAgent", Kind: "code-scan"},
		{Path: "q", Symbol: "RecipeAgent", Kind: "code-scan"},
		{Path: "r", Symbol: "  ", Kind: "code-scan"},
	})
	if suffix != "|RecipeAgent" {
		t.Fatalf("suffix = %q, want %q", suffix, "|RecipeAgent")
	}
}
