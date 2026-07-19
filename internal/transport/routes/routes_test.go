package routes

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
)

func TestCommonRoutesExcludeApplicationObserveEndpoints(t *testing.T) {
	mux := http.NewServeMux()
	Setup(mux, Config{
		Dashboard: &dashboard.Handler{},
		MCP:       http.NotFoundHandler(),
		VCS:       func(http.ResponseWriter, *http.Request) {},
		Cfg:       config.Config{},
	})
	for _, path := range []string{"/api/observe/status", "/api/observe/sources"} {
		_, pattern := mux.Handler(&http.Request{Method: http.MethodGet, URL: mustURL(t, path)})
		if pattern != "" {
			t.Fatalf("common route table contains %q via pattern %q", path, pattern)
		}
	}
	_, pattern := mux.Handler(&http.Request{Method: http.MethodGet, URL: mustURL(t, "/api/semantic/status")})
	if pattern == "" {
		t.Fatal("semantic status route is not registered")
	}
	_, pattern = mux.Handler(&http.Request{Method: http.MethodGet, URL: mustURL(t, "/api/qdrant/stats")})
	if pattern != "" {
		t.Fatalf("removed Qdrant status route is still registered via %q", pattern)
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
