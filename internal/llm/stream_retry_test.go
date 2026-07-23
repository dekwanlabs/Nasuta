package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/platform/httpclient"
	"github.com/go-resty/resty/v2"
)

func TestPostStreamRetriesOn429(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	rc := httpclient.New(time.Second, nil)
	resp, err := postStream(context.Background(), func() (*resty.Response, error) {
		return rc.R().SetDoNotParseResponse(true).Get(srv.URL)
	}, nil)
	if err != nil {
		t.Fatalf("postStream: %v", err)
	}
	defer resp.RawBody().Close()
	if resp.StatusCode() != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode())
	}
	if hits != 2 {
		t.Fatalf("hits = %d, want one retry", hits)
	}
}

func TestPostStreamNonRetryableStatusNoRetry(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	rc := httpclient.New(time.Second, nil)
	_, err := postStream(context.Background(), func() (*resty.Response, error) {
		return rc.R().SetDoNotParseResponse(true).Get(srv.URL)
	}, nil)
	var ce *CallError
	if !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
		t.Fatalf("want 400 CallError, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want no retry", hits)
	}
}
