package application

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"

	"github.com/dekwanlabs/astris/internal/agent"
	"github.com/dekwanlabs/astris/log"
)

// Main runs the standard Astris distribution.
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

func runSelftest(ctx context.Context, knowledge *agent.Service, service string) {
	fmt.Println(toJSON(knowledge.IndexSummary(ctx)))
	fmt.Println(toJSON(knowledge.ServiceLookup(ctx, service, 3)))
	fmt.Println(toJSON(knowledge.TraceDeps(service, "both", 2)))
	fmt.Println(toJSON(knowledge.ListApis(ctx, service, "", 10)))
	fmt.Println(toJSON(knowledge.DocGapCheck(ctx, service)))
	fmt.Println(toJSON(knowledge.RunbookSearch(ctx, "eureka", 5, false, "")))
}

func toJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(data)
}
