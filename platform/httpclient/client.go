// Package httpclient owns shared Resty configuration for outbound adapters.
package httpclient

import (
	"context"
	"net/http"
	"time"

	"github.com/go-resty/resty/v2"
)

// New creates a Resty client with shared headers and no implicit retries.
func New(timeout time.Duration, headers map[string]string) *resty.Client {
	return resty.New().
		SetHeaders(headers).
		SetTimeout(timeout).
		SetRetryCount(0)
}

// NewWithHTTP keeps an injected client's transport and timeout when provided.
func NewWithHTTP(hc *http.Client, timeout time.Duration, headers map[string]string) *resty.Client {
	rc := New(timeout, headers)
	if hc != nil {
		rc.SetTransport(hc.Transport).SetTimeout(hc.Timeout)
	}
	return rc
}

// Request starts a Resty request with the caller's context.
func Request(ctx context.Context, rc *resty.Client) *resty.Request {
	return rc.R().SetContext(ctx)
}
