package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

func (srv *Service) searchBrave(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if srv.apiKey == "" {
		return nil, fmt.Errorf("brave search requires NASUTA_WEB_SEARCH_API_KEY")
	}

	endpoint := fmt.Sprintf("%s?q=%s&count=%d", braveEndpoint, url.QueryEscape(query), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", srv.apiKey)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave search: %w", err)
	}
	defer resp.Body.Close()
	if !respOK(resp) {
		return nil, fmt.Errorf(
			"brave: HTTP %d - %s",
			resp.StatusCode,
			fetchErrorHint(resp.StatusCode),
		)
	}

	var data struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("brave: parse response: %w", err)
	}

	results := make([]SearchResult, 0, min(limit, len(data.Web.Results)))
	for _, result := range data.Web.Results {
		results = append(results, SearchResult{
			Title:   result.Title,
			URL:     result.URL,
			Snippet: result.Description,
		})
		if len(results) == limit {
			break
		}
	}
	return results, nil
}
