package main

import (
	"context"
	"fmt"
	"os"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/indexing"
	ontologysqlite "github.com/dekwanlabs/nasuta/internal/platform/ontologystore/sqlite"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

func main() {
	root := "/Users/dequan.mac/agent-workspace/workspace"
	sqlitePath := "/Users/dequan.mac/agent-workspace/workspace/.nasuta/index.db"

	db, err := store.Open(sqlitePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open sqlite: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	svc := &indexing.Service{
		Cfg:      config.Config{WorkspaceRoot: root, SQLitePath: sqlitePath},
		Platform: &config.PlatformSettings{},
		DB:       db,
	}
	svc.SetOntologyPublisher(ontologysqlite.New(db))

	if err := svc.RebuildSQLIndex(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "rebuild: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("REBUILD OK")
	traceMain()
	resolveMain()
}
