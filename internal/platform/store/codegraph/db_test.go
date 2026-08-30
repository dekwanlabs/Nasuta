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
	for _, filePath := range []string{
		"gitlab/team/service/main.go",
		"repot/team/service/main.go",
	} {
		workspace := t.TempDir()
		dbPath := filepath.Join(workspace, ".codegraph", "codegraph.db")
		writeTestDatabase(t, dbPath, filePath, true)

		if _, err := Open(workspace); err == nil {
			t.Fatalf("Open succeeded with a non-canonical node path %q", filePath)
		}
	}
}

func TestCallEdgesPreserveFanoutAndRepeatedCallSites(t *testing.T) {
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
 file_path TEXT NOT NULL,language TEXT NOT NULL,start_line INTEGER NOT NULL,end_line INTEGER NOT NULL,signature TEXT
);
CREATE TABLE edges (source TEXT,target TEXT,kind TEXT,metadata TEXT,line INTEGER,col INTEGER,provenance TEXT);
INSERT INTO nodes VALUES ('root','method','Root','svc.Root','repos/team/svc/root.go','go',1,30,'func Root()');
INSERT INTO nodes VALUES
 ('n1','method','N1','svc.N1','repos/team/svc/n.go','go',40,45,''),
 ('n2','method','N2','svc.N2','repos/team/svc/n.go','go',50,55,''),
 ('n3','method','N3','svc.N3','repos/team/svc/n.go','go',60,65,''),
 ('n4','method','N4','svc.N4','repos/team/svc/n.go','go',70,75,''),
 ('n5','method','N5','svc.N5','repos/team/svc/n.go','go',80,85,''),
 ('n6','method','N6','svc.N6','repos/team/svc/n.go','go',90,95,'');
INSERT INTO edges VALUES
 ('root','n1','calls','{"confidence":0.9}',10,2,'go/ast'),
 ('root','n1','calls','{"confidence":0.8}',11,4,'go/ast'),
 ('root','n2','calls','{}',12,1,'go/ast'),('root','n3','calls','{}',13,1,'go/ast'),
 ('root','n4','calls','{}',14,1,'go/ast'),('root','n5','calls','{}',15,1,'go/ast'),
 ('root','n6','calls','{}',16,1,'go/ast');`); err != nil {
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
	hops, more, err := db.CallEdges("root", "callees", 20)
	if err != nil {
		t.Fatal(err)
	}
	if more || len(hops) != 7 {
		t.Fatalf("hops=%d more=%v, want all seven call sites", len(hops), more)
	}
	if hops[0].Target.ID != "n1" || hops[1].Target.ID != "n1" || hops[0].Edge.Line != 10 || hops[1].Edge.Line != 11 {
		t.Fatalf("repeated call sites were folded: %+v", hops[:2])
	}
	if hops[0].Edge.Col != 2 || hops[0].Edge.Provenance != "go/ast" || hops[0].Edge.Confidence != 0.9 {
		t.Fatalf("call-site evidence lost: %+v", hops[0].Edge)
	}
	_, more, err = db.CallEdges("root", "callees", 4)
	if err != nil || !more {
		t.Fatalf("bounded fanout more=%v err=%v, want explicit truncation", more, err)
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

func TestResolveRouteMethodInPathFindsSiblingModule(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, ".codegraph", "codegraph.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`
CREATE TABLE nodes (
 id TEXT PRIMARY KEY,kind TEXT NOT NULL,name TEXT NOT NULL,qualified_name TEXT NOT NULL,
 file_path TEXT NOT NULL,language TEXT NOT NULL,start_line INTEGER NOT NULL,end_line INTEGER NOT NULL,
 signature TEXT
);
CREATE TABLE edges (source TEXT,target TEXT,kind TEXT);
INSERT INTO nodes VALUES
 ('controller','method','getDevices4Share','RoomDeviceController.getDevices4Share','repos/hsas/hsas-dreo-app/hsas-share/src/RoomDeviceController.java','java',63,67,''),
 ('route','route','GET /family/me/room/devices','RoomDeviceController.getDevices4Share.route','repos/hsas/hsas-dreo-app/hsas-share/src/RoomDeviceController.java','java',63,63,'');`)
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	graph, err := Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer graph.Close()
	node, err := graph.ResolveRouteMethodInPath("repos/hsas/hsas-dreo-app", "GET", "/family/me/room/devices")
	if err != nil {
		t.Fatal(err)
	}
	if node.ID != "controller" || node.FilePath != "repos/hsas/hsas-dreo-app/hsas-share/src/RoomDeviceController.java" {
		t.Fatalf("resolved node = %+v", node)
	}
}
