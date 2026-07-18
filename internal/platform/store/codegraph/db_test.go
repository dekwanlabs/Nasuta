package codegraph

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestSearchSymbolsUsesFTSAndPathScope(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, ".codegraph", "codegraph.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
CREATE TABLE nodes (
 id TEXT PRIMARY KEY,kind TEXT NOT NULL,name TEXT NOT NULL,qualified_name TEXT NOT NULL,
 file_path TEXT NOT NULL,language TEXT NOT NULL,start_line INTEGER NOT NULL,end_line INTEGER NOT NULL,
 signature TEXT
);
CREATE TABLE edges (source TEXT,target TEXT,kind TEXT);
CREATE VIRTUAL TABLE nodes_fts USING fts5(id,name,qualified_name,docstring,signature,content='nodes',content_rowid='rowid');
INSERT INTO nodes VALUES
 ('1','method','LoadUser','app.LoadUser','repos/team/app/user.go','go',10,20,'func LoadUser()'),
 ('2','method','LoadUser','admin.LoadUser','repos/team/admin/user.go','go',30,40,'func LoadUser()');
INSERT INTO nodes_fts(rowid,id,name,qualified_name,signature)
 SELECT rowid,id,name,qualified_name,signature FROM nodes;
`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	nodes, err := db.SearchSymbols(context.Background(), SymbolQuery{
		Terms: []string{"LoadUser"}, PathPrefixes: []string{"repos/team/app"}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].FilePath != "repos/team/app/user.go" {
		t.Fatalf("nodes = %+v", nodes)
	}
}

func TestRefreshReadsRebuiltDatabase(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, ".codegraph", "codegraph.db")
	writeTestDatabase(t, dbPath, "repos/old/service/main.go", true)

	db, err := Open(workspace)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	assertPaths(t, db, []string{"repos/old/service/main.go"})

	nextPath := filepath.Join(filepath.Dir(dbPath), "next.db")
	writeTestDatabase(t, nextPath, "repos/new/service/main.go", true)
	if err := os.Rename(nextPath, dbPath); err != nil {
		t.Fatalf("replace database: %v", err)
	}
	if err := db.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	assertPaths(t, db, []string{"repos/new/service/main.go"})
}

func TestRefreshKeepsCurrentDatabaseWhenReplacementIsInvalid(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, ".codegraph", "codegraph.db")
	writeTestDatabase(t, dbPath, "repos/old/service/main.go", true)

	db, err := Open(workspace)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	nextPath := filepath.Join(filepath.Dir(dbPath), "next.db")
	writeTestDatabase(t, nextPath, "repos/new/service/main.go", false)
	if err := os.Rename(nextPath, dbPath); err != nil {
		t.Fatalf("replace database: %v", err)
	}
	if err := db.Refresh(); err == nil {
		t.Fatal("Refresh succeeded with an invalid database")
	}

	assertPaths(t, db, []string{"repos/old/service/main.go"})
}

func TestOpenRejectsNonCanonicalNodePaths(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, ".codegraph", "codegraph.db")
	writeTestDatabase(t, dbPath, "gitlab/team/service/main.go", true)

	if _, err := Open(workspace); err == nil {
		t.Fatal("Open succeeded with a non-canonical node path")
	}
}

func writeTestDatabase(t *testing.T, path, filePath string, withEdges bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create database directory: %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE nodes (file_path TEXT NOT NULL)`); err != nil {
		t.Fatalf("create nodes: %v", err)
	}
	if withEdges {
		if _, err := db.Exec(`CREATE TABLE edges (source TEXT, target TEXT)`); err != nil {
			t.Fatalf("create edges: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO nodes (file_path) VALUES (?)`, filePath); err != nil {
		t.Fatalf("insert node: %v", err)
	}
}

func assertPaths(t *testing.T, db *DB, want []string) {
	t.Helper()
	got, err := db.DistinctNodeFilePaths()
	if err != nil {
		t.Fatalf("DistinctNodeFilePaths: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
	}
}
