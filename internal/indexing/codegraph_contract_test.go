package indexing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
)

func TestCodegraphBuilderContract(t *testing.T) {
	if os.Getenv("NASUTA_CODEGRAPH_CONTRACT") != "1" {
		t.Skip("set NASUTA_CODEGRAPH_CONTRACT=1 to run the external builder contract")
	}
	cli, err := exec.LookPath("codegraph")
	if err != nil {
		t.Skip("codegraph CLI unavailable")
	}
	workspace := t.TempDir()
	sourcePath := filepath.Join(workspace, "repos", "team", "sample", "sample.go")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte(`package sample
func Caller() int { return Callee() }
func Callee() int { return 1 }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := runCodegraphAt(ctx, cli, workspace); err != nil {
		t.Fatal(err)
	}
	db, err := codegraph.Open(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if db == nil {
		t.Fatal("builder produced no readable codegraph database")
	}
	defer db.Close()
	nodes, err := db.SearchSymbols(ctx, codegraph.SymbolQuery{Terms: []string{"Caller"}, Limit: 5})
	if err != nil || len(nodes) == 0 {
		t.Fatalf("Caller node missing: nodes=%+v err=%v", nodes, err)
	}
	hops, _, err := db.CallEdges(nodes[0].ID, "callees", 10)
	if err != nil || len(hops) == 0 {
		t.Fatalf("Caller -> Callee edge missing: hops=%+v err=%v", hops, err)
	}
	if hops[0].Edge.Line <= 0 {
		t.Fatalf("call-site line missing: %+v", hops[0].Edge)
	}
}
