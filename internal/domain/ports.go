package types

import "context"

type Searcher interface {
	ServiceLookup(ctx context.Context, query string, limit int) map[string]any
	CodeSearch(ctx context.Context, query, lang string, limit int) map[string]any
	RunbookSearch(ctx context.Context, query string, limit int, includeText bool, scope string) map[string]any
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
