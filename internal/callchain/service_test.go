package callchain

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/platform"
	_ "modernc.org/sqlite"
)

func TestTraceClosesFeignDownstreamAndContinuesTraversal(t *testing.T) {
	workspace := t.TempDir()
	structure := buildStructure(t, workspace)
	defer structure.Close()
	graph := buildCodeGraph(t, workspace)
	defer graph.Close()
	feign, err := graph.FindNodeAt(context.Background(), "repos/team/orders/PaymentsClient.java", 30)
	if err != nil {
		t.Fatal(err)
	}
	if method, path, ok := graph.RouteForNode(*feign); !ok || method != "POST" || path != "/payments/charge" {
		t.Fatalf("feign route=%s %s ok=%v", method, path, ok)
	}

	result, err := New(structure, graph).Trace(context.Background(), Request{
		File: "repos/team/orders/Caller.java", Line: 10, Direction: "callees",
		MaxDepth: 4, MaxNodes: 20, MaxFanout: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Target == nil || result.Target.ServiceName != "orders" {
		t.Fatalf("target=%+v", result.Target)
	}
	if len(result.Callees.Hops) != 3 {
		t.Fatalf("hops=%+v, want local, bridge, downstream local", result.Callees.Hops)
	}
	bridge := result.Callees.Hops[1]
	if !bridge.Bridge || bridge.Source.Name != "charge" || bridge.Target.Name != "chargeImpl" || bridge.Target.ServiceName != "payments" {
		t.Fatalf("bridge=%+v", bridge)
	}
	if result.Callees.Hops[2].Target.Name != "save" {
		t.Fatalf("downstream traversal did not continue: %+v", result.Callees.Hops[2])
	}
}

func TestTraceClosesFeignUpstreamAndContinuesTraversal(t *testing.T) {
	workspace := t.TempDir()
	structure := buildStructure(t, workspace)
	defer structure.Close()
	graph := buildCodeGraph(t, workspace)
	defer graph.Close()

	result, err := New(structure, graph).Trace(context.Background(), Request{
		File: "repos/team/payments/PaymentsController.java", Line: 45, Direction: "callers",
		MaxDepth: 4, MaxNodes: 20, MaxFanout: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Callers.Hops) != 2 {
		t.Fatalf("hops=%+v, want upstream bridge and local caller", result.Callers.Hops)
	}
	bridge := result.Callers.Hops[0]
	if !bridge.Bridge || bridge.Source.Name != "charge" || bridge.Target.Name != "chargeImpl" {
		t.Fatalf("upstream bridge=%+v", bridge)
	}
	if result.Callers.Hops[1].Source.Name != "checkout" {
		t.Fatalf("upstream traversal did not continue: %+v", result.Callers.Hops[1])
	}
}

func TestTraceReturnsCandidatesForAmbiguousSymbol(t *testing.T) {
	workspace := t.TempDir()
	structure := buildStructure(t, workspace)
	defer structure.Close()
	graph := buildCodeGraph(t, workspace)
	defer graph.Close()
	service := New(structure, graph)

	result, err := service.Trace(context.Background(), Request{Query: "charge", Direction: "both"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Target != nil || len(result.Candidates) < 2 {
		t.Fatalf("ambiguous result=%+v", result)
	}
	result, err = service.Trace(context.Background(), Request{
		Query: "charge", QualifiedName: "PaymentsClient.charge", Direction: "both",
	})
	if err != nil || result.Target == nil || result.Target.ID != "feign" {
		t.Fatalf("qualified result=%+v err=%v", result, err)
	}
}

func TestTraceReportsUnavailableIndexes(t *testing.T) {
	_, err := New(nil, nil).Trace(context.Background(), Request{Query: "charge"})
	if err == nil {
		t.Fatal("Trace succeeded without structure and codegraph indexes")
	}
}

func buildStructure(t *testing.T, workspace string) *store.SQLite {
	t.Helper()
	db, err := store.Open(filepath.Join(workspace, ".nasuta", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	orders := callChainService("team/orders", "orders")
	payments := callChainService("team/payments", "payments")
	bundle := domain.IndexBundle{
		Repositories: []domain.RepositoryRecord{
			{Repo: orders.Repo, HeadSHA: "orders", IndexedAt: time.Now().UnixMilli()},
			{Repo: payments.Repo, HeadSHA: "payments", IndexedAt: time.Now().UnixMilli()},
		},
		Services: []domain.ServiceRecord{orders, payments},
		Dependencies: []domain.DependencyEdge{{
			CallerServiceKey: orders.ServiceKey, TargetKind: domain.DependencyTargetService,
			TargetServiceKey: payments.ServiceKey, From: "orders", To: "payments", Type: domain.EdgeFeign,
			Evidence:   []domain.Evidence{{Path: "repos/team/orders/PaymentsClient.java", Line: 5, Symbol: "PaymentsClient", Kind: domain.SourceCodeScan}},
			Confidence: 0.95,
		}},
		Endpoints: []domain.EndpointRecord{{
			ServiceKey: payments.ServiceKey, ServiceName: "payments", Repo: payments.Repo,
			Method: "POST", Path: "/payments/charge", Handler: "PaymentsController", HandlerMethod: "chargeImpl",
			File: "repos/team/payments/PaymentsController.java", Line: 40, Source: domain.SourceCodeScan, Confidence: 0.95,
		}},
	}
	if err := db.ReplaceStructure(context.Background(), "callchain", bundle); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func callChainService(repo, name string) domain.ServiceRecord {
	return domain.ServiceRecord{
		ServiceKey: platform.UUIDFromString(repo + "\x00."), ServiceName: name, Repo: repo, ModulePath: ".",
		Layer: "server", Language: "java", Tags: []string{}, Docs: []string{}, SourceOfTruth: []string{},
		Entrypoints: []domain.Evidence{}, Ports: []int{}, Confidence: 0.9,
	}
}

func buildCodeGraph(t *testing.T, workspace string) *codegraph.DB {
	t.Helper()
	path := filepath.Join(workspace, ".codegraph", "codegraph.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
CREATE TABLE nodes (
 id TEXT PRIMARY KEY,kind TEXT NOT NULL,name TEXT NOT NULL,qualified_name TEXT NOT NULL,
 file_path TEXT NOT NULL,language TEXT NOT NULL,start_line INTEGER NOT NULL,end_line INTEGER NOT NULL,signature TEXT
);
CREATE TABLE edges (source TEXT,target TEXT,kind TEXT,metadata TEXT,line INTEGER,col INTEGER,provenance TEXT);
CREATE VIRTUAL TABLE nodes_fts USING fts5(id,name,qualified_name,docstring,signature,content='nodes',content_rowid='rowid');
INSERT INTO nodes VALUES
 ('caller','method','checkout','orders.checkout','repos/team/orders/Caller.java','java',10,20,''),
 ('feign','method','charge','PaymentsClient.charge','repos/team/orders/PaymentsClient.java','java',30,35,''),
 ('feign-route','route','POST /payments/charge','PaymentsClient.charge.route','repos/team/orders/PaymentsClient.java','java',29,29,''),
 ('impl','method','chargeImpl','PaymentsController.chargeImpl','repos/team/payments/PaymentsController.java','java',40,50,''),
 ('impl-route','route','POST /payments/charge','PaymentsController.chargeImpl.route','repos/team/payments/PaymentsController.java','java',40,40,''),
 ('save','method','save','PaymentService.save','repos/team/payments/PaymentService.java','java',60,70,''),
 ('other-charge','method','charge','Other.charge','repos/team/payments/Other.java','java',80,90,'');
INSERT INTO nodes_fts(rowid,id,name,qualified_name,signature)
 SELECT rowid,id,name,qualified_name,signature FROM nodes;
INSERT INTO edges VALUES
 ('caller','feign','calls','{"confidence":0.95}',15,4,'java'),
 ('impl','save','calls','{"confidence":0.95}',45,8,'java');`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	graph, err := codegraph.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}
