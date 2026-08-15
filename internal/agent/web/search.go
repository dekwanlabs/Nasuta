package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/websearch"
	nethtml "golang.org/x/net/html"
)

const userAgent = "Mozilla/5.0 (compatible; Nasuta/1.0)"

func (srv *Service) dispatchSearch(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	name := srv.searchEngine
	if name == "" {
		name = "duckduckgo"
	}
	srv.ensureProviders()
	srv.providersMu.RLock()
	provider, ok := srv.providers[name]
	srv.providersMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unsupported web search provider %q", name)
	}
	return provider.Search(ctx, query, limit)
}

func (srv *Service) ensureProviders() {
	srv.providerOnce.Do(func() {
		srv.providers = map[string]SearchProvider{
			"duckduckgo": websearch.ProviderFunc(srv.searchDuckDuckGo),
			"brave":      websearch.ProviderFunc(srv.searchBrave),
			"bing":       websearch.ProviderFunc(srv.searchBing),
		}
	})
}

func respOK(resp *http.Response) bool {
	return resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
}

func fetchErrorHint(status int) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "API key rejected - check NASUTA_WEB_SEARCH_API_KEY"
	case http.StatusTooManyRequests:
		return "rate limited - wait and retry"
	default:
		return "server error"
	}
}

func isAnchor(tag []byte, tokenizer *nethtml.Tokenizer, class string) bool {
	return isTag(tag, "a") && hasClass(tokenizer, class)
}

func isDiv(tag []byte, tokenizer *nethtml.Tokenizer, class string) bool {
	return isTag(tag, "div") && hasClass(tokenizer, class)
}

func isTag(tag []byte, name string) bool {
	return len(tag) == len(name) && strings.EqualFold(string(tag), name)
}

func hasClass(tokenizer *nethtml.Tokenizer, target string) bool {
	for {
		key, value, more := tokenizer.TagAttr()
		if isTag(key, "class") {
			for _, class := range strings.Fields(string(value)) {
				if class == target {
					return true
				}
			}
			return false
		}
		if !more {
			return false
		}
	}
}

func extractAttr(tokenizer *nethtml.Tokenizer, name string) string {
	for {
		key, value, more := tokenizer.TagAttr()
		if isTag(key, name) {
			return string(value)
		}
		if !more {
			return ""
		}
	}
}
