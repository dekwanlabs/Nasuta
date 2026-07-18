package embed

import (
	"context"
	"fmt"
	"time"

	"github.com/dekwanlabs/astris/config"
	"github.com/dekwanlabs/astris/log"
	"github.com/dekwanlabs/astris/platform/httpclient"
	"github.com/go-resty/resty/v2"
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

func (e *HTTPEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	var out embedResponse
	resp, err := httpclient.Request(ctx, e.rc).
		SetBody(embedRequest{Model: e.model, Input: texts}).
		SetResult(&out).
		Post(e.baseURL)
	if err != nil {
		return nil, err
	}
	if resp.IsError() {
		return nil, fmt.Errorf("embedding API %d: %s", resp.StatusCode(), string(resp.Body()))
	}
	vectors := make([][]float32, len(out.Data))
	for i, d := range out.Data {
		vectors[i] = d.Embedding
	}
	return vectors, nil
}
