package tools

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/agent/web"
)

type WebSearchResult = web.WebSearchResult
type WebSearchProvider = web.WebSearchProvider
type WebFetchedEvidence = web.WebFetchedEvidence
type WebSearchResponse = web.WebSearchResponse

func (srv *Service) webService() *web.Service {
	srv.webOnce.Do(func() {
		if srv.web == nil {
			srv.web = web.New()
		}
	})
	return srv.web
}

func (srv *Service) SetWebSearchEngine(engine string) {
	srv.webService().SetSearchEngine(engine)
}

func (srv *Service) SetWebSearchAPIKey(apiKey string) {
	srv.webService().SetAPIKey(apiKey)
}

func (srv *Service) RegisterWebSearchProvider(name string, provider WebSearchProvider) error {
	return srv.webService().RegisterProvider(name, provider)
}

func (srv *Service) WebSearch(ctx context.Context, query string, limit int) ([]WebSearchResult, error) {
	return srv.webService().WebSearch(ctx, query, limit)
}

func (srv *Service) WebSearchWithFetch(ctx context.Context, query string, limit int) (WebSearchResponse, error) {
	return srv.webService().WebSearchWithFetch(ctx, query, limit)
}

func (srv *Service) WebFetch(ctx context.Context, rawURL string) (string, error) {
	return srv.webService().WebFetch(ctx, rawURL)
}

func (srv *Service) WebFetchRelevant(ctx context.Context, rawURL, query string) (string, error) {
	return srv.webService().WebFetchRelevant(ctx, rawURL, query)
}
