package qa

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestWithoutToolRemovesSessionDetailsFromRunSnapshot(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry := NewRegistry(&Service{}, config.Config{}, memory.NewSessionStore(db), nil)
	snapshot := registry.Snapshot(tool.ReadPolicy())
	if _, ok := snapshot.Get("get_turn"); !ok {
		t.Fatal("registered detail tool missing")
	}
	filtered := withoutSessionHistoryTools(preparedScenarioTools{
		snapshot: snapshot,
		executor: NewToolExecutor(registry),
	})
	if _, ok := filtered.Get("get_turn"); ok {
		t.Fatal("detail tool remained visible without a compaction reference")
	}
	for _, published := range snapshot.MCPTools() {
		if published.ID == "get_turn" {
			t.Fatal("detail tool was published over MCP")
		}
	}
}
