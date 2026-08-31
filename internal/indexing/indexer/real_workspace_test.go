package indexer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This test intentionally scans the checked-out projects instead of reading
// the historical .nasuta snapshot. It protects the invariant that shared
// Feign modules contribute only clients reached by executable call sites.
func TestRealWorkspaceFeignDependenciesRequireCallSites(t *testing.T) {
	if os.Getenv("NASUTA_RUN_REAL_WORKSPACE_TESTS") != "1" {
		t.Skip("real workspace tests disabled; set NASUTA_RUN_REAL_WORKSPACE_TESTS=1 to enable")
	}
	root := strings.TrimSpace(os.Getenv("NASUTA_REAL_WORKSPACE_ROOT"))
	if root == "" {
		t.Skip("real workspace tests require NASUTA_REAL_WORKSPACE_ROOT")
	}
	root = filepath.Clean(root)
	if _, err := os.Stat(filepath.Join(root, "repos", "hsas", "hsas-dreo-app")); err != nil {
		t.Skipf("real workspace is unavailable: %v", err)
	}
	bundle := BuildStructuralBundle(root, []string{
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
