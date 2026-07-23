package ontologystore

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

func TestNewDispatchesSQLiteExplicitly(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	backend, err := New(config.OntologyConfig{Provider: "sqlite"}, db)
	if err != nil || backend == nil {
		t.Fatalf("backend=%v err=%v", backend, err)
	}
}

func TestNewDoesNotSubstituteSQLiteForNeo4j(t *testing.T) {
	_, err := New(config.OntologyConfig{Provider: "neo4j", Neo4j: config.Neo4jConfig{URI: "neo4j://localhost:7687"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("error = %v", err)
	}
}

func TestNewRejectsUnsupportedProvider(t *testing.T) {
	_, err := New(config.OntologyConfig{Provider: "memory"}, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error = %v", err)
	}
}
