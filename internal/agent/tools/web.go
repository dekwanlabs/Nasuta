package tools

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/agent/web"
)

type WebFetchedEvidence = web.FetchedEvidence
type WebSearchResponse = web.SearchResponse
type WebQueryRewriter = web.QueryRewriter

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

func (srv *Service) SetWebQueryRewriter(rewriter WebQueryRewriter) {
	srv.webService().SetQueryRewriter(rewriter)
}

func (srv *Service) WebSearchWithFetch(ctx context.Context, query string, limit int) (WebSearchResponse, error) {
	return srv.webService().SearchWithFetch(ctx, query, limit)
}
