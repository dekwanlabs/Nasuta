package domain

import "context"

// Searcher exposes bounded knowledge lookups to internal consumers.
type Searcher interface {
	ServiceLookup(ctx context.Context, query string, limit int) map[string]any
	CodeSearch(ctx context.Context, query, lang string, limit int) map[string]any
}

type Embedder interface {
	Enabled() bool
	Dim() int
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
}

type Store interface {
	Close() error
}

type Notifier interface {
	Notify(ctx context.Context, title, body string) error
}
