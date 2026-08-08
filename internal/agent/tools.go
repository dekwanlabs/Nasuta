package agent

import (
	agenttools "github.com/dekwanlabs/nasuta/internal/agent/tools"
	agentweb "github.com/dekwanlabs/nasuta/internal/agent/web"
)

type Deps = agenttools.Deps
type Service = agenttools.Service

type WebSearchResult = agentweb.WebSearchResult
type WebSearchProvider = agentweb.WebSearchProvider
type WebFetchedEvidence = agentweb.WebFetchedEvidence
type WebSearchResponse = agentweb.WebSearchResponse

func NewTools(deps Deps) *Service {
	return agenttools.New(deps)
}
