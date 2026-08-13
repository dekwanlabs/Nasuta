package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httpclient"
	"github.com/go-resty/resty/v2"
)

// Retry budget for one Embed call. Mirrors the internal/llm convention;
// duplicated rather than imported because platform must not depend on a
// business package. Embedding is side-effect free, so retrying a whole batch
// is safe.
const (
	maxAttempts    = 3
	initialBackoff = 500 * time.Millisecond
	maxBackoff     = 8 * time.Second
)

// Embedder turns text into vectors. Implementations: HTTP (cloud) and Noop.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Dim() int
	Enabled() bool
}

// Noop is used when no embedding API key is configured (M1 mode).
type Noop struct{}

func (Noop) Embed(context.Context, []string) ([][]float32, error) {
	return nil, fmt.Errorf("embedding disabled: set EMBEDDING_API_KEY")
}
func (Noop) Dim() int      { return 0 }
func (Noop) Enabled() bool { return false }

// HTTPEmbedder calls an OpenAI-compatible or Voyage embeddings endpoint.
type HTTPEmbedder struct {
	provider string
	model    string
	dim      int
	baseURL  string
	rc       *resty.Client
	// backoff overrides the first retry delay; zero means initialBackoff.
	// Tests set it to keep retry coverage fast.
	backoff time.Duration
}

func (e *HTTPEmbedder) firstBackoff() time.Duration {
	if e.backoff <= 0 {
		return initialBackoff
	}
	return e.backoff
}

// New returns the configured embedder: LocalEmbedder for provider "local",
// an HTTPEmbedder when an API key is set, otherwise a Noop.
func New(c config.Config) Embedder {
	base := c.EmbeddingBaseURL
	if c.EmbeddingAPIKey == "" || base == "" {
		return Noop{}
	}
	key := c.EmbeddingAPIKey
	masked := key
	if len(key) > 12 {
		masked = key[:6] + "..." + key[len(key)-4:]
	}
	log.Infof("[embed] provider=%s model=%s base=%s key=%s", c.EmbeddingProvider, c.EmbeddingModel, c.EmbeddingBaseURL, masked)
	return &HTTPEmbedder{
		provider: c.EmbeddingProvider,
		model:    c.EmbeddingModel,
		dim:      c.EmbeddingDim,
		baseURL:  base,
		rc: httpclient.New(120*time.Second, map[string]string{
			"Authorization": "Bearer " + c.EmbeddingAPIKey,
		}),
	}
}

func (e *HTTPEmbedder) Dim() int      { return e.dim }
func (e *HTTPEmbedder) Enabled() bool { return true }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed retries transient transport failures (DNS blips, resets) and 429/5xx
// with exponential backoff, so one hiccup does not silently drop the whole
// dense-retrieval leg for a query.
func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	backoff := e.firstBackoff()
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		vectors, err := e.embedOnce(ctx, texts)
		if err == nil {
			if attempt > 1 {
				log.Infof("[embed] succeeded on attempt %d/%d", attempt, maxAttempts)
			}
			return vectors, nil
		}
		lastErr = err
		if !retryable(err) || attempt == maxAttempts {
			break
		}
		delay := retryDelay(err, backoff)
		log.Warnf("[embed] attempt %d/%d failed, retrying in %s: %v", attempt, maxAttempts, delay, err)
		if !sleepCtx(ctx, delay) {
			return nil, lastErr
		}
		backoff = doubleBackoff(backoff)
	}
	return nil, lastErr
}

func (e *HTTPEmbedder) embedOnce(ctx context.Context, texts []string) ([][]float32, error) {
	var out embedResponse
	resp, err := httpclient.Request(ctx, e.rc).
		SetBody(embedRequest{Model: e.model, Input: texts}).
		SetResult(&out).
		Post(e.baseURL)
	if err != nil {
		return nil, &apiError{err: err}
	}
	if resp.IsError() {
		return nil, &apiError{
			status:     resp.StatusCode(),
			body:       string(resp.Body()),
			retryAfter: parseRetryAfter(resp.Header()),
		}
	}
	vectors := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vectors[i] = d.Embedding
	}
	return vectors, nil
}

// apiError distinguishes a transport failure (err set) from a non-2xx
// response (status set) so retryable can tell them apart.
type apiError struct {
	status     int
	body       string
	err        error
	retryAfter time.Duration
}

func (e *apiError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("embedding API %d: %s", e.status, strings.TrimSpace(e.body))
}

func (e *apiError) Unwrap() error { return e.err }

// retryable mirrors llm.CallError.Retryable: transport errors always, non-2xx
// only for 429 and 5xx. A dead parent context never retries - it just fails again.
func retryable(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.err != nil {
		return !errors.Is(ae.err, context.Canceled) && !errors.Is(ae.err, context.DeadlineExceeded)
	}
	return ae.status == http.StatusTooManyRequests || ae.status >= http.StatusInternalServerError
}

// retryDelay honors a server-advised Retry-After over exponential backoff.
func retryDelay(err error, backoff time.Duration) time.Duration {
	var ae *apiError
	if errors.As(err, &ae) && ae.retryAfter > 0 {
		return ae.retryAfter
	}
	return backoff
}

// parseRetryAfter reads the delay from a Retry-After header, supporting both
// the delay-seconds and HTTP-date forms.
func parseRetryAfter(header http.Header) time.Duration {
	v := strings.TrimSpace(header.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func doubleBackoff(d time.Duration) time.Duration {
	if d *= 2; d > maxBackoff {
		return maxBackoff
	}
	return d
}
