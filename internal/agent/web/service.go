package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"unicode"

	"github.com/dekwanlabs/nasuta/internal/websearch"
	"github.com/dekwanlabs/nasuta/log"
)

type SearchResult = websearch.Result
type SearchProvider = websearch.Provider

type FetchedEvidence struct {
	URL     string `json:"url"`
	Title   string `json:"title,omitempty"`
	Content string `json:"content"`
}

type SearchResponse struct {
	Results   []SearchResult   `json:"results"`
	Fetched   *FetchedEvidence `json:"fetched,omitempty"`
	FetchNote string           `json:"fetch_note,omitempty"`
}

// Service owns web search providers and bounded page fetching.
type Service struct {
	searchEngine string
	apiKey       string
	providerOnce sync.Once
	providersMu  sync.RWMutex
	providers    map[string]SearchProvider
	fetchClient  *http.Client
}

func New() *Service {
	return &Service{fetchClient: ssrfSafeClient()}
}

func (srv *Service) SetSearchEngine(engine string) {
	srv.searchEngine = strings.ToLower(strings.TrimSpace(engine))
}

func (srv *Service) SetAPIKey(apiKey string) {
	srv.apiKey = apiKey
}

// RegisterProvider adds or replaces a provider during application wiring.
func (srv *Service) RegisterProvider(name string, provider SearchProvider) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fmt.Errorf("web search provider name is empty")
	}
	if provider == nil {
		return fmt.Errorf("web search provider %q is nil", name)
	}
	srv.ensureProviders()
	srv.providersMu.Lock()
	srv.providers[name] = provider
	srv.providersMu.Unlock()
	return nil
}

// Search dispatches to the configured provider.
func (srv *Service) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}

	results, err := srv.dispatchSearch(ctx, query, limit)
	if err != nil {
		return nil, err
	}

	log.InfofCtx(ctx, "[web_search] provider=%s query=%q → %d results",
		srv.searchEngine, truncateForLog(query, 60), len(results))
	return results, nil
}

// SearchWithFetch fetches the first query-relevant candidate without hiding the candidate set.
func (srv *Service) SearchWithFetch(ctx context.Context, query string, limit int) (SearchResponse, error) {
	results, err := srv.Search(ctx, query, limit)
	if err != nil {
		return SearchResponse{}, err
	}
	response := SearchResponse{Results: results}
	if len(results) == 0 {
		return response, nil
	}
	candidate, ok := relevantFetchCandidate(query, results)
	if !ok {
		response.FetchNote = "automatic fetch skipped: no search candidate was relevant to the query"
		log.WarnfCtx(ctx, "[web_search] automatic fetch skipped: no relevant candidate for query=%q", truncateForLog(query, 60))
		return response, nil
	}
	content, err := srv.FetchRelevant(ctx, candidate.URL, query)
	if err != nil {
		response.FetchNote = "automatic fetch failed: " + err.Error()
		log.WarnfCtx(ctx, "[web_search] automatic fetch failed url=%q: %v", truncateForLog(candidate.URL, 100), err)
		return response, nil
	}
	response.Fetched = &FetchedEvidence{URL: candidate.URL, Title: candidate.Title, Content: content}
	return response, nil
}

func relevantFetchCandidate(query string, results []SearchResult) (SearchResult, bool) {
	for _, result := range results {
		if resultRelevant(query, result) {
			return result, true
		}
	}
	return SearchResult{}, false
}

func resultRelevant(query string, result SearchResult) bool {
	queryLatin, queryCJK := searchSignals(query)
	if len(queryLatin) == 0 && len(queryCJK) == 0 {
		return false
	}
	candidateLatin, candidateCJK := searchSignals(result.Title + " " + result.Snippet)
	for signal := range queryLatin {
		if _, ok := candidateLatin[signal]; ok {
			return true
		}
	}

	required := min(3, len(queryCJK))
	matched := 0
	for signal := range queryCJK {
		if _, ok := candidateCJK[signal]; !ok {
			continue
		}
		matched++
		if matched >= required {
			return true
		}
	}
	return false
}

var searchStopwords = map[string]struct{}{
	"a": {}, "an": {}, "and": {}, "are": {}, "for": {}, "how": {}, "is": {}, "of": {}, "or": {},
	"the": {}, "to": {}, "what": {}, "when": {}, "where": {}, "which": {}, "who": {}, "why": {},
}

func searchSignals(value string) (map[string]struct{}, map[string]struct{}) {
	latin := make(map[string]struct{})
	cjk := make(map[string]struct{})
	var word, han []rune
	flushWord := func() {
		if len(word) < 2 {
			word = word[:0]
			return
		}
		token := strings.ToLower(string(word))
		if _, skip := searchStopwords[token]; !skip {
			latin[token] = struct{}{}
		}
		word = word[:0]
	}
	flushHan := func() {
		if len(han) == 1 {
			cjk[string(han)] = struct{}{}
		} else {
			for i := 1; i < len(han); i++ {
				cjk[string(han[i-1:i+1])] = struct{}{}
			}
		}
		han = han[:0]
	}
	for _, r := range value {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word = append(word, unicode.ToLower(r))
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return latin, cjk
}

func truncateForLog(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
