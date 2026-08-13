package embed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/platform/httpclient"
)

// newTestEmbedder uses a 1ms first backoff so retry coverage stays fast
// without weakening the production budget.
func newTestEmbedder(url string) *HTTPEmbedder {
	return &HTTPEmbedder{
		provider: "voyage",
		model:    "voyage-code-3",
		dim:      2,
		baseURL:  url,
		rc:       httpclient.New(5*time.Second, nil),
		backoff:  time.Millisecond,
	}
}

func TestEmbedRetriesTransportFailureThenSucceeds(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// First attempt dies mid-flight, mimicking a DNS/connection blip.
		if atomic.AddInt32(&calls, 1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("ResponseWriter is not a Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.25,0.5]}]}`))
	}))
	defer server.Close()

	vectors, err := newTestEmbedder(server.URL).Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatalf("Embed after retry: %v", err)
	}
	if len(vectors) != 1 || len(vectors[0]) != 2 || vectors[0][0] != 0.25 || vectors[0][1] != 0.5 {
		t.Fatalf("vectors = %#v", vectors)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server calls = %d, want 2", got)
	}
}

func TestEmbedRetries5xxAndHonorsRetryAfter(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,2]}]}`))
	}))
	defer server.Close()

	if _, err := newTestEmbedder(server.URL).Embed(context.Background(), []string{"hi"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("server calls = %d, want 2", got)
	}
}

func TestEmbedDoesNotRetryClientError(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer server.Close()

	_, err := newTestEmbedder(server.URL).Embed(context.Background(), []string{"hi"})
	if err == nil {
		t.Fatal("expected error for 401")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want it to mention 401", err)
	}
	// A bad API key will never fix itself - retrying just burns latency.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("server calls = %d, want 1", got)
	}
}

func TestEmbedExhaustsAttemptsAndReturnsLastError(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if _, err := newTestEmbedder(server.URL).Embed(context.Background(), []string{"hi"}); err == nil {
		t.Fatal("expected error after exhausting attempts")
	}
	if got := atomic.LoadInt32(&calls); got != maxAttempts {
		t.Fatalf("server calls = %d, want %d", got, maxAttempts)
	}
}

func TestEmbedStopsRetryingOnCancelledContext(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := newTestEmbedder(server.URL).Embed(ctx, []string{"hi"}); err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("server calls = %d, want 0", got)
	}
}

func TestDoubleBackoffSaturatesAtMax(t *testing.T) {
	if got := doubleBackoff(time.Second); got != 2*time.Second {
		t.Fatalf("doubleBackoff(1s) = %s", got)
	}
	if got := doubleBackoff(maxBackoff); got != maxBackoff {
		t.Fatalf("doubleBackoff(max) = %s, want %s", got, maxBackoff)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		header string
		want   time.Duration
	}{
		{"", 0},
		{"2", 2 * time.Second},
		{"0", 0},
		{"garbage", 0},
		{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 0},
	}
	for _, tc := range cases {
		h := http.Header{}
		if tc.header != "" {
			h.Set("Retry-After", tc.header)
		}
		if got := parseRetryAfter(h); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tc.header, got, tc.want)
		}
	}
}
