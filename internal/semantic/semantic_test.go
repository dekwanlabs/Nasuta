package semantic

import "testing"

func TestRequiredCapabilitiesReturnsValueCopy(t *testing.T) {
	first := RequiredCapabilities()
	first.Dense = false

	if !RequiredCapabilities().Dense {
		t.Fatal("required capabilities were mutated through a returned value")
	}
}

func TestValidateCapabilitiesRejectsMissingBehavior(t *testing.T) {
	actual := RequiredCapabilities()
	actual.GroupBy = false

	if err := ValidateCapabilities("test", actual); err == nil {
		t.Fatal("missing GroupBy capability unexpectedly accepted")
	}
}

func TestDeduplicateHitsKeepsFirstHitPerGroup(t *testing.T) {
	hits := []Hit{
		{ID: "a1", Metadata: map[string]any{"repo": "a"}},
		{ID: "a2", Metadata: map[string]any{"repo": "a"}},
		{ID: "b1", Metadata: map[string]any{"repo": "b"}},
	}

	grouped := DeduplicateHits(hits, "repo", 2)
	if len(grouped) != 2 || grouped[0].ID != "a1" || grouped[1].ID != "b1" {
		t.Fatalf("grouped hits = %#v, want first hit from each group", grouped)
	}
}
