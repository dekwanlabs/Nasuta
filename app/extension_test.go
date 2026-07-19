package app

import (
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestExtensionDepsDetachSettingsAndExposeStablePorts(t *testing.T) {
	registry := tool.NewRegistry()
	readTools := tool.NewReadRegistry(registry)
	settings := &config.PlatformSettings{
		VCSGroups:          []string{"group-a"},
		VCSExcludeProjects: []string{"repo-a"},
	}
	platform := &Platform{
		cfg:       config.Config{WorkspaceRoot: "/workspace"},
		settings:  settings,
		readTools: readTools,
	}

	deps := platform.extensionDeps()
	deps.Settings.VCSGroups[0] = "changed"
	deps.Settings.VCSExcludeProjects[0] = "changed"

	if settings.VCSGroups[0] != "group-a" || settings.VCSExcludeProjects[0] != "repo-a" {
		t.Fatalf("extension mutated platform settings: %#v", settings)
	}
	if deps.WorkspaceRoot != "/workspace" || deps.ReadTools != readTools {
		t.Fatalf("extension deps = %#v", deps)
	}
}
