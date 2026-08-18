package tools

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/agent/web"
)

type WebSearchResult = web.SearchResult
type WebSearchProvider = web.SearchProvider
type WebFetchedEvidence = web.FetchedEvidence
type WebSearchResponse = web.SearchResponse
type WebSourceStatus = web.SourceStatus

const (
	WebSourceUsable   = web.SourceUsable
	WebSourceUnusable = web.SourceUnusable
)

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

func (srv *Service) RegisterWebProvider(name string, provider WebSearchProvider) error {
	return srv.webService().RegisterProvider(name, provider)
}

func (srv *Service) WebSearch(ctx context.Context, query string, limit int) ([]WebSearchResult, error) {
	return srv.webService().Search(ctx, query, limit)
}

func (srv *Service) WebSearchWithFetch(ctx context.Context, query string, limit int) (WebSearchResponse, error) {
	return srv.webService().SearchWithFetch(ctx, query, limit)
}

func (srv *Service) WebFetch(ctx context.Context, rawURL string) (string, error) {
	return srv.webService().Fetch(ctx, rawURL)
}

func (srv *Service) WebFetchRelevant(ctx context.Context, rawURL, query string) (string, error) {
	return srv.webService().FetchRelevant(ctx, rawURL, query)
}
