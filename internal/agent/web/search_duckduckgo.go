package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	nethtml "golang.org/x/net/html"
)

func (srv *Service) searchDuckDuckGo(ctx context.Context, query string, limit int) ([]WebSearchResult, error) {
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://html.duckduckgo.com/html/",
		strings.NewReader(url.Values{"q": {query}}.Encode()),
	)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", userAgent)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search returned HTTP %d", resp.StatusCode)
	}
	return parseDDGResults(resp.Body, limit), nil
}

func parseDDGResults(reader io.Reader, limit int) []WebSearchResult {
	var results []WebSearchResult
	tokenizer := nethtml.NewTokenizer(reader)
	var current WebSearchResult
	var inResult, inSnippet bool

	for len(results) < limit {
		tokenType := tokenizer.Next()
		if tokenType == nethtml.ErrorToken {
			break
		}
		tag, _ := tokenizer.TagName()
		switch tokenType {
		case nethtml.StartTagToken:
			if isDiv(tag, tokenizer, "result__body") {
				current = WebSearchResult{}
				inResult = true
			}
			if inResult && isAnchor(tag, tokenizer, "result__a") {
				current.URL = extractAttr(tokenizer, "href")
			}
			if inResult && isAnchor(tag, tokenizer, "result__snippet") {
				inSnippet = true
			}
		case nethtml.EndTagToken:
			if isTag(tag, "a") && inResult && inSnippet {
				inSnippet = false
			}
			if isTag(tag, "div") && inResult {
				if current.URL != "" && current.Title != "" {
					results = append(results, current)
				}
				inResult = false
				inSnippet = false
			}
		case nethtml.TextToken:
			text := strings.TrimSpace(string(tokenizer.Text()))
			if text == "" {
				continue
			}
			if inResult && !inSnippet && current.Title == "" {
				current.Title = text
			}
			if inSnippet {
				if current.Snippet != "" {
					current.Snippet += " "
				}
				current.Snippet += text
			}
		}
	}
	return results
}
