package routes

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

func TestBearerAuthAcceptsConfiguredTokenWithoutDatabaseLookup(t *testing.T) {
	var keyAuthCalled bool
	handler := bearerAuth("configured-token", func(context.Context, string) (bool, error) {
		keyAuthCalled = true
		return false, nil
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer configured-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if keyAuthCalled {
		t.Fatal("persisted key lookup ran for the configured token")
	}
}

func TestBearerAuthAcceptsPersistedMCPKey(t *testing.T) {
	handler := bearerAuth("", func(_ context.Context, candidate string) (bool, error) {
		return candidate == "mcp-assigned-key", nil
	}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer mcp-assigned-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func TestBearerAuthRejectsInvalidMCPKey(t *testing.T) {
	handler := bearerAuth("", func(context.Context, string) (bool, error) {
		return false, nil
	}, http.NotFoundHandler())

	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer invalid-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestBearerAuthSurfacesPersistedKeyBackendFailure(t *testing.T) {
	handler := bearerAuth("", func(context.Context, string) (bool, error) {
		return false, errors.New("database unavailable")
	}, http.NotFoundHandler())

	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer mcp-assigned-key")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
}

func TestBearerAuthAllowsRequestsWhenNoAuthenticationIsConfigured(t *testing.T) {
	handler := bearerAuth("", nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

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
	_, pattern = mux.Handler(&http.Request{Method: http.MethodGet, URL: mustURL(t, "/api/qa/runtime")})
	if pattern == "" {
		t.Fatal("QA runtime status route is not registered")
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

func TestTraceMiddlewareLogsHTTPError(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httputil.WriteErr(w, errors.New("query failed"))
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/incidents?secret=value", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	got := logs.String()
	for _, want := range []string{
		"level=ERROR",
		"method=GET",
		"path=/api/incidents",
		"status=500",
		"error=query",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log does not contain %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "secret=value") {
		t.Fatalf("log contains raw query: %s", got)
	}
}

func TestTraceMiddlewareLogsClientErrorAsWarning(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httputil.WriteBadRequest(w, "invalid request")
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/test", nil))

	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "status=400") {
		t.Fatalf("unexpected log:\n%s", got)
	}
}

func TestTraceMiddlewareDoesNotLogSuccessfulRequest(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if got := logs.String(); got != "" {
		t.Fatalf("successful request produced log:\n%s", got)
	}
}

func TestResponseRecorderPreservesFlusher(t *testing.T) {
	writer := &flushWriter{ResponseRecorder: httptest.NewRecorder()}
	handler := TraceMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("wrapped response writer does not implement http.Flusher")
		}
		flusher.Flush()
	}))

	handler.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, "/events", nil))

	if !writer.flushed {
		t.Fatal("flush was not forwarded")
	}
}

type flushWriter struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (w *flushWriter) Flush() {
	w.flushed = true
}
