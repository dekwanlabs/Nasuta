package app

import (
	"context"
	"flag"
	"net/http"

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