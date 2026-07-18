package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewAppliesDefaultsAndRestyEncodesJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("X-Request") != "override" {
			t.Fatalf("headers = %#v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"name":"codeloom"}` {
			t.Fatalf("body = %s", body)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	rc := New(time.Second, map[string]string{"Authorization": "Bearer token", "X-Request": "default"})
	resp, err := Request(context.Background(), rc).
		SetHeader("X-Request", "override").
		SetBody(map[string]string{"name": "codeloom"}).
		Post(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != http.StatusOK || string(resp.Body()) != `{"ok":true}` {
		t.Fatalf("response status=%d body=%s", resp.StatusCode(), resp.Body())
	}
}

func TestRequestPropagatesContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Request(ctx, New(time.Second, nil)).Get(server.URL)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
}

func TestNewWithHTTPPreservesInjectedNetworking(t *testing.T) {
	transport := http.DefaultTransport
	timeout := 275 * time.Millisecond
	rc := NewWithHTTP(&http.Client{Transport: transport, Timeout: timeout}, time.Second, nil)
	if rc.GetClient().Transport != transport {
		t.Fatal("injected transport was not preserved")
	}
	if rc.GetClient().Timeout != timeout {
		t.Fatalf("timeout = %s, want %s", rc.GetClient().Timeout, timeout)
	}
}

func TestRequestLeavesStreamingBodyOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("data: ok\n\n"))
	}))
	defer server.Close()

	resp, err := Request(context.Background(), New(time.Second, nil)).
		SetDoNotParseResponse(true).
		Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	raw := resp.RawBody()
	defer raw.Close()
	body, err := io.ReadAll(raw)
	if err != nil || string(body) != "data: ok\n\n" {
		t.Fatalf("stream = %q err=%v", body, err)
	}
}
