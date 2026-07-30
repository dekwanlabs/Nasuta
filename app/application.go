package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"

	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/log"
)

// Main runs the standard Nasuta distribution.
func Main() {
	platform, err := New()
	if err != nil {
		log.Fatalf("build app: %v", err)
	}
	defer platform.Close()

	ctx := context.Background()
	options := parseOptions()
	switch options.mode {
	case "selftest":
		runSelftest(ctx, platform.knowledge, options.service)
	case "server":
		if err := platform.configureIncidents(nil); err != nil {
			log.Fatalf("configure incident workflows: %v", err)
		}
		mux := http.NewServeMux()
		platform.RegisterCommonRoutes(mux)
		if err := platform.Serve(ctx, mux); err != nil {
			log.Fatalf("serve platform: %v", err)
		}
	default:
		log.Fatalf("unknown mode %q", options.mode)
	}
}

type options struct {
	mode    string
	service string
}

func parseOptions() options {
	var value options
	flag.StringVar(&value.mode, "mode", "server", "server | selftest")
	flag.StringVar(&value.service, "service", "hsas-app-user", "service name for selftest")
	flag.Parse()
	return value
}

func runSelftest(ctx context.Context, tools *agent.Service, service string) {
	fmt.Println(toJSON(tools.IndexSummary(ctx)))
	fmt.Println(toJSON(tools.ServiceLookup(ctx, service, 3)))
	dependencies, err := tools.TraceDeps(ctx, service, "both", 2)
	if err != nil {
		fmt.Println(toJSON(map[string]any{"service": service, "error": err.Error()}))
	} else {
		fmt.Println(toJSON(dependencies))
	}
	fmt.Println(toJSON(tools.ListApis(ctx, service, "", 10)))
	fmt.Println(toJSON(tools.DocGapCheck(ctx, service)))
	fmt.Println(toJSON(tools.RunbookSearch(ctx, knowledge.RunbookQuery{Query: "eureka", Limit: 5})))
}

func toJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(data)
}
