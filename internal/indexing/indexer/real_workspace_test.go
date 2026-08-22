package indexer

import (
	"os"
	"strings"
	"testing"
)

const realWorkspaceRoot = "/Users/dequan.mac/agent-workspace/workspace"

// This test intentionally scans the checked-out projects instead of reading
// the historical .nasuta snapshot. It protects the invariant that shared
// Feign modules contribute only clients reached by executable call sites.
func TestRealWorkspaceFeignDependenciesRequireCallSites(t *testing.T) {
	if _, err := os.Stat(realWorkspaceRoot + "/repos/hsas/hsas-dreo-app"); err != nil {
		t.Skipf("real workspace is unavailable: %v", err)
	}
	bundle := BuildStructuralBundle(realWorkspaceRoot, []string{
		"repos/hsas/hsas-dreo-app",
		"repos/hsds/hsds-base-system",
	})
	var applicationToSystem bool
	for _, dependency := range bundle.Dependencies {
		if dependency.From != "hsas-dreo-application" {
			continue
		}
		if dependency.To == "hsds-base-system-feign" {
			t.Fatalf("shared Feign library was treated as an upstream service: %+v", dependency)
		}
		if dependency.To == "hsds-base-system-provider" {
			applicationToSystem = true
			if len(dependency.Evidence) == 0 || !strings.Contains(dependency.Evidence[0].Path, "/src/main/") {
				t.Fatalf("system dependency lacks business call-site evidence: %+v", dependency)
			}
		}
	}
	if !applicationToSystem {
		t.Fatalf("real hsas-dreo-application -> hsds-base-system-provider call edge missing")
	}
}
