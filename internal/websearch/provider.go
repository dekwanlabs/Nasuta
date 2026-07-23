package websearch

import "context"

// Result is one candidate returned by a search provider.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// Provider executes one external search backend.
type Provider interface {
	Search(ctx context.Context, query string, limit int) ([]Result, error)
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(context.Context, string, int) ([]Result, error)

func (f ProviderFunc) Search(ctx context.Context, query string, limit int) ([]Result, error) {
	return f(ctx, query, limit)
}
