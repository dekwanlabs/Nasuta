package memory

import "testing"

func TestNormalizeMemoryProbesBoundsAndValidatesHints(t *testing.T) {
	probes := normalizeMemoryProbes([]probeEntry{
		{Query: "response language", FactKeyHint: " user:response-language ", KindHint: "preference"},
		{Query: "response language", FactKeyHint: "user:response-language", KindHint: "preference"},
		{Query: "service owner", FactKeyHint: "invalid:key", KindHint: "profile"},
		{Query: "", FactKeyHint: "user:response-style"},
	})
	if len(probes) != 2 {
		t.Fatalf("probes = %#v", probes)
	}
	if probes[0].FactKeyHint != "user:response-language" || probes[0].KindHint != KindPreference {
		t.Fatalf("first probe = %#v", probes[0])
	}
	if probes[1].FactKeyHint != "" || probes[1].KindHint != KindProfile {
		t.Fatalf("second probe = %#v", probes[1])
	}
}
