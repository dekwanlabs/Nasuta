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

func (srv *Service) searchDuckDuckGo(ctx context.Context, query string, limit int) ([]SearchResult, error) {
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

type ddgParseState struct {
	current   SearchResult
	inResult  bool
	inSnippet bool
}

func parseDDGResults(reader io.Reader, limit int) []SearchResult {
	var results []SearchResult
	tokenizer := nethtml.NewTokenizer(reader)
	var state ddgParseState

	for len(results) < limit {
		tokenType := tokenizer.Next()
		if tokenType == nethtml.ErrorToken {
			break
		}
		tag, _ := tokenizer.TagName()
		switch tokenType {
		case nethtml.StartTagToken:
			state.handleDDGStartTag(tag, tokenizer)
		case nethtml.EndTagToken:
			results = state.handleDDGEndTag(tag, results)
		case nethtml.TextToken:
			state.handleDDGText(tokenizer)
		}
	}
	return results
}

func (state *ddgParseState) handleDDGStartTag(tag []byte, tokenizer *nethtml.Tokenizer) {
	if isDiv(tag, tokenizer, "result__body") {
		state.current = SearchResult{}
		state.inResult = true
		return
	}
	if !state.inResult {
		return
	}
	if isAnchor(tag, tokenizer, "result__a") {
		state.current.URL = extractAttr(tokenizer, "href")
	}
	if isAnchor(tag, tokenizer, "result__snippet") {
		state.inSnippet = true
	}
}

func (state *ddgParseState) handleDDGEndTag(tag []byte, results []SearchResult) []SearchResult {
	if isTag(tag, "a") && state.inResult && state.inSnippet {
		state.inSnippet = false
	}
	if isTag(tag, "div") && state.inResult {
		if state.current.URL != "" && state.current.Title != "" {
			results = append(results, state.current)
		}
		state.inResult = false
		state.inSnippet = false
	}
	return results
}

func (state *ddgParseState) handleDDGText(tokenizer *nethtml.Tokenizer) {
	text := strings.TrimSpace(string(tokenizer.Text()))
	if text == "" {
		return
	}
	if state.inResult && !state.inSnippet && state.current.Title == "" {
		state.current.Title = text
	}
	if state.inSnippet {
		if state.current.Snippet != "" {
			state.current.Snippet += " "
		}
		state.current.Snippet += text
	}
}
