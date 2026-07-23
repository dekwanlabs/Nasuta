package semanticstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/platform/semanticstore/milvus"
	"github.com/dekwanlabs/nasuta/internal/platform/store"
	"github.com/dekwanlabs/nasuta/internal/semantic"
)

// New is the only production dispatcher for semantic providers.
func New(cfg config.SemanticConfig) (semantic.Store, error) {
	provider := strings.ToLower(cfg.Provider)
	if provider == "" {
		return nil, fmt.Errorf("semantic provider is required: configure SEMANTIC_PROVIDER and SEMANTIC_ENDPOINT")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("semantic provider %q requires SEMANTIC_ENDPOINT", provider)
	}
	if cfg.Collection == "" {
		return nil, fmt.Errorf("semantic provider %q requires SEMANTIC_COLLECTION", provider)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var (
		backend semantic.Store
		err     error
	)
	switch provider {
	case "qdrant":
		backend, err = store.NewQdrant(ctx, cfg)
	case "milvus":
		backend, err = milvus.New(ctx, cfg)
	default:
		return nil, fmt.Errorf("semantic provider %q is unsupported", provider)
	}
	if err != nil {
		return nil, fmt.Errorf("semantic provider %q: %w", provider, err)
	}
	if err := semantic.ValidateCapabilities(provider, backend.Capabilities()); err != nil {
		backend.Close()
		return nil, err
	}
	return backend, nil
}
