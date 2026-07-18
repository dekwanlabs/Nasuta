package routes

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/dekwanlabs/astris/config"
	"github.com/dekwanlabs/astris/internal/transport/dashboard"
)

func TestCommonRoutesExcludeScenarioEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	Setup(mux, Config{
		Dashboard: &dashboard.Handler{},
		MCP:       http.NotFoundHandler(),
		VCS:       func(http.ResponseWriter, *http.Request) {},
		Cfg:       config.Config{},
	})
	for _, path := range []string{"/api/observe/status", "/api/incidents", "/api/alert/webhook", "/api/qa/actions"} {
		_, pattern := mux.Handler(&http.Request{Method: http.MethodGet, URL: mustURL(t, path)})
		if pattern != "" {
			t.Fatalf("common route table contains %q via pattern %q", path, pattern)
		}
	}
}

func mustURL(t *testing.T, path string) *url.URL {
	t.Helper()
	value, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
