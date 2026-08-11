package agent

import (
	"github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/internal/agent/web"
)

type Deps = tools.Deps
type Service = tools.Service

type WebSearchResult = web.WebSearchResult
type WebSearchProvider = web.WebSearchProvider
type WebFetchedEvidence = web.WebFetchedEvidence
type WebSearchResponse = web.WebSearchResponse

func NewTools(deps Deps) *Service {
	return tools.New(deps)
}
