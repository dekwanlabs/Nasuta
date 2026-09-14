package main

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/ontology"
	ontologysqlite "github.com/dekwanlabs/nasuta/internal/platform/ontologystore/sqlite"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
)

func traceMain() {
	ctx := context.Background()
	db, err := store.Open("/Users/dequan.mac/agent-workspace/workspace/.nasuta/index.db")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	backend := ontologysqlite.New(db)
	svc := ontology.NewService(backend)

	for _, name := range []string{"tts-proxy", "speech-proxy", "voice-assistant-gateway"} {
		res, err := svc.TraceDependencies(ctx, ontology.DependencyQuery{
			Service: name, Direction: ontology.DirectionBoth, MaxDepth: 2, MaxNodes: 500, MaxFanout: 100,
		})
		if err != nil {
			fmt.Printf("%s: ERR %v\n", name, err)
			continue
		}
		fmt.Printf("\n=== %s ===\n", name)
		fmt.Printf("root=%v candidates=%d\n", func() string { if res.Root != nil { return res.Root.Name }; return "" }(), len(res.Candidates))
		fmt.Printf("upstream=%d downstream=%d\n", len(res.Upstream), len(res.Downstream))
		for _, f := range res.Upstream {
			fmt.Printf("  UP: %s -> %s (%s) expr=%q\n", f.Subject.Name, f.Object.Name, f.Qualifiers["protocol"], f.Qualifiers["target_expression"])
		}
		for _, f := range res.Downstream {
			fmt.Printf("  DOWN: %s -> %s (%s) expr=%q\n", f.Subject.Name, f.Object.Name, f.Qualifiers["protocol"], f.Qualifiers["target_expression"])
		}
	}
}
