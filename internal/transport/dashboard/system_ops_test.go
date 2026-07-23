package dashboard

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
)

type rebuildIndexingOps struct {
	IndexingOps
	rebuild    func(context.Context) error
	rebuildSQL func(context.Context) error
	bootstrap  func(context.Context) error
}

func (ops rebuildIndexingOps) RebuildGraph(ctx context.Context) error {
	return ops.rebuild(ctx)
}

func (ops rebuildIndexingOps) RebuildSQLIndex(ctx context.Context) error {
	return ops.rebuildSQL(ctx)
}

func (ops rebuildIndexingOps) Bootstrap(ctx context.Context) error {
	return ops.bootstrap(ctx)
}

func TestAPIRebuildSQLIndexSurvivesRequestCancellation(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	handler := &Handler{idx: rebuildIndexingOps{rebuildSQL: func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			t.Fatalf("operation inherited canceled request context: %v", err)
		}
		return nil
	}}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/system/rebuild-sql-index", nil).WithContext(requestCtx)

	handler.APIRebuildSQLIndex(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestSystemOperationContextSurvivesRequestCancellation(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	cancelRequest()
	request := httptest.NewRequest(http.MethodPost, "/api/system/bootstrap", nil).WithContext(requestCtx)

	ctx, cancel := systemOperationContext(request)
	defer cancel()
	if err := ctx.Err(); err != nil {
		t.Fatalf("system operation inherited canceled request context: %v", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("system operation has no server-owned deadline")
	}
}

func TestAPIRebuildCodeGraphRefreshesConnection(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, ".codegraph", "codegraph.db")
	writeDashboardCodeGraphDB(t, dbPath, "repos/old/service/main.go")

	cgDB, err := codegraph.Open(workspace)
	if err != nil {
		t.Fatalf("open codegraph: %v", err)
	}
	defer cgDB.Close()

	handler := &Handler{
		cfg:         config.Config{WorkspaceRoot: workspace},
		codegraphDB: cgDB,
		idx: rebuildIndexingOps{rebuild: func(context.Context) error {
			nextPath := filepath.Join(filepath.Dir(dbPath), "next.db")
			writeDashboardCodeGraphDB(t, nextPath, "repos/new/service/main.go")
			return os.Rename(nextPath, dbPath)
		}},
	}
	recorder := httptest.NewRecorder()

	handler.APIRebuildCodeGraph(recorder, httptest.NewRequest(http.MethodPost, "/api/ops/codegraph", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	paths, err := handler.codegraphDB.DistinctNodeFilePaths()
	if err != nil {
		t.Fatalf("DistinctNodeFilePaths: %v", err)
	}
	if len(paths) != 1 || paths[0] != "repos/new/service/main.go" {
		t.Fatalf("paths = %v, want rebuilt database path", paths)
	}
}

func TestAPIBootstrapRefreshesCodeGraphBeforeIndexing(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, ".codegraph", "codegraph.db")
	writeDashboardCodeGraphDB(t, dbPath, "repos/old/service/main.go")

	cgDB, err := codegraph.Open(workspace)
	if err != nil {
		t.Fatalf("open codegraph: %v", err)
	}
	defer cgDB.Close()

	var order []string
	handler := &Handler{
		cfg:         config.Config{WorkspaceRoot: workspace},
		codegraphDB: cgDB,
	}
	handler.idx = rebuildIndexingOps{
		rebuild: func(context.Context) error {
			order = append(order, "graph")
			nextPath := filepath.Join(filepath.Dir(dbPath), "next.db")
			writeDashboardCodeGraphDB(t, nextPath, "repos/new/service/main.go")
			return os.Rename(nextPath, dbPath)
		},
		bootstrap: func(context.Context) error {
			order = append(order, "index")
			paths, err := handler.codegraphDB.DistinctNodeFilePaths()
			if err != nil {
				return err
			}
			if len(paths) != 1 || paths[0] != "repos/new/service/main.go" {
				return fmt.Errorf("bootstrap saw stale codegraph paths: %v", paths)
			}
			return nil
		},
	}
	recorder := httptest.NewRecorder()

	handler.APIBootstrap(recorder, httptest.NewRequest(http.MethodPost, "/api/system/bootstrap", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if fmt.Sprint(order) != "[graph index]" {
		t.Fatalf("operation order = %v, want graph then index", order)
	}
}

func writeDashboardCodeGraphDB(t *testing.T, path, filePath string) {
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
	if _, err := db.Exec(`CREATE TABLE edges (source TEXT, target TEXT)`); err != nil {
		t.Fatalf("create edges: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO nodes (file_path) VALUES (?)`, filePath); err != nil {
		t.Fatalf("insert node: %v", err)
	}
}
