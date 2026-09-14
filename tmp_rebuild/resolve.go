package main

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/ontology"
	ontologysqlite "github.com/dekwanlabs/nasuta/internal/platform/ontologystore/sqlite"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

func resolveMain() {
	ctx := context.Background()
	db, _ := store.Open("/Users/dequan.mac/agent-workspace/workspace/.nasuta/index.db")
	defer db.Close()
	backend := ontologysqlite.New(db)

	for _, q := range []string{"tts-proxy", "tts-proxy-v2", "speech-proxy"} {
		res, err := backend.Resolve(ctx, ontology.ResolveQuery{Text: q, Classes: []ontology.Class{ontology.ClassService}, Limit: 20})
		if err != nil {
			fmt.Printf("%s: ERR %v\n", q, err)
			continue
		}
		fmt.Printf("\n=== %s: %d candidates ===\n", q, len(res.Entities))
		for _, e := range res.Entities {
			fmt.Printf("  %s (class=%s) id=%s\n", e.Name, e.Class, e.ID)
		}
	}
}
