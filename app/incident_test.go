package app

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestConfigureIncidentsRegistersPlatformWriteCatalog(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	registry := tool.NewRegistry()
	platform := &Platform{
		cfg:      config.Config{WorkspaceRoot: t.TempDir()},
		settings: &config.PlatformSettings{},
		registry: registry,
	}
	if err := platform.configureIncidentsWithDB(db, nil); err != nil {
		t.Fatal(err)
	}
	if platform.incident.manager == nil || platform.incident.api == nil {
		t.Fatalf("incident platform was not fully configured: %#v", platform)
	}
	snapshot := registry.Snapshot(tool.AllPolicy())
	for _, id := range []tool.ToolID{"propose_branch", "propose_commit"} {
		candidate, ok := snapshot.Get(id)
		if !ok || candidate.Kind != tool.KindWrite {
			t.Fatalf("write tool %q = %#v, ok=%v", id, candidate, ok)
		}
	}
}
