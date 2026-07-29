package featurehttp

import (
	"net/http"
	"testing"
)

func TestRegisterRoutesIncludesFeatureDeliverySurface(t *testing.T) {
	mux := http.NewServeMux()
	New(nil).RegisterRoutes(func(pattern string, handler http.HandlerFunc) {
		mux.HandleFunc(pattern, handler)
	})
	for _, target := range []struct {
		method string
		path   string
	}{
		{"POST", "/api/features"},
		{"GET", "/api/features/feat-1"},
		{"POST", "/api/features/feat-1/artifacts/system_design/generate"},
		{"POST", "/api/features/feat-1/implementations"},
		{"GET", "/api/feature-implementations/run-1/events"},
		{"GET", "/api/feature-implementations/run-1/patch"},
	} {
		request, err := http.NewRequest(target.method, target.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, pattern := mux.Handler(request)
		if pattern == "" {
			t.Fatalf("route not registered: %s %s", target.method, target.path)
		}
	}
}
