package agent

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
)

func TestBuiltinToolDescriptionsKeepEvidenceBoundariesDistinct(t *testing.T) {
	descriptions := make(map[string]string)
	for _, candidate := range builtinTools(&Service{}, config.Config{}) {
		descriptions[string(candidate.ID)] = candidate.Description
	}
	checks := map[string][]string{
		"get_service":     {"metadata", "does not establish dependencies"},
		"trace_deps":      {"service-level", "does not establish method-level"},
		"list_apis":       {"complete API routes", "class-level and method-level"},
		"search_code":     {"fallback", "not as proof", "complete API route"},
		"get_symbol":      {"exact definitions", "does not establish its callers"},
		"trace_calls":     {"method-level callers and callees", "not proof of complete service dependencies"},
		"search_runbooks": {"operational runbooks", "do not prove current runtime state"},
		"check_docs":      {"documentation coverage", "does not establish runtime"},
		"index_stats":     {"health and summary counts", "does not establish business behavior"},
	}
	for name, fragments := range checks {
		description := descriptions[name]
		if description == "" {
			t.Errorf("missing built-in tool %q", name)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(description, fragment) {
				t.Errorf("%s description missing %q: %q", name, fragment, description)
			}
		}
	}
}
