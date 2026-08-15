package agent

import (
	"github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/internal/agent/web"
)

type Deps = tools.Deps
type Service = tools.Service

type WebSearchResult = web.SearchResult
type WebSearchProvider = web.SearchProvider
type WebFetchedEvidence = web.FetchedEvidence
type WebSearchResponse = web.SearchResponse

func NewTools(deps Deps) *Service {
	return tools.New(deps)
}
