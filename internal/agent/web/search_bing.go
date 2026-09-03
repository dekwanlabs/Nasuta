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

const bingEndpoint = "https://cn.bing.com/search"

func (srv *Service) searchBing(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	endpoint := fmt.Sprintf("%s?q=%s", bingEndpoint, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("bing search: %w", err)
	}
	defer resp.Body.Close()
	if !respOK(resp) {
		return nil, fmt.Errorf("bing: HTTP %d", resp.StatusCode)
	}

	detector := &bingChallengeReader{reader: resp.Body}
	results := parseBingResults(detector, limit)
	if detector.blocked {
		return nil, fmt.Errorf("bing: automated search challenge returned instead of results")
	}
	return results, nil
}

type bingParseState struct {
	current  SearchResult
	inResult bool
	inTitle  bool
}

func parseBingResults(reader io.Reader, limit int) []SearchResult {
	var results []SearchResult
	tokenizer := nethtml.NewTokenizer(reader)
	var state bingParseState

	for len(results) < limit {
		tokenType := tokenizer.Next()
		if tokenType == nethtml.ErrorToken {
			break
		}
		tag, _ := tokenizer.TagName()
		tagName := strings.ToLower(string(tag))
		switch tokenType {
		case nethtml.StartTagToken:
			state.handleBingStartTag(tokenizer, tagName)
		case nethtml.EndTagToken:
			results = state.handleBingEndTag(tagName, results)
		case nethtml.TextToken:
			state.handleBingText(tokenizer)
		}
	}
	return results
}

func (state *bingParseState) handleBingStartTag(tokenizer *nethtml.Tokenizer, tagName string) {
	if tagName == "li" && hasClass(tokenizer, "b_algo") {
		state.current = SearchResult{}
		state.inResult = true
		return
	}
	if !state.inResult {
		return
	}
	switch tagName {
	case "h2":
		state.inTitle = true
	case "a":
		if state.inTitle {
			state.current.URL = extractAttr(tokenizer, "href")
		}
	case "div":
		if hasClass(tokenizer, "b_caption") {
			state.current.Snippet = extractBingSnippet(tokenizer)
		}
	}
}

func (state *bingParseState) handleBingEndTag(tagName string, results []SearchResult) []SearchResult {
	if tagName == "li" && state.inResult {
		if state.current.URL != "" && state.current.Title != "" {
			results = append(results, state.current)
		}
		state.inResult = false
		state.inTitle = false
	}
	if tagName == "h2" && state.inTitle {
		state.inTitle = false
	}
	return results
}

func (state *bingParseState) handleBingText(tokenizer *nethtml.Tokenizer) {
	if !state.inResult || !state.inTitle {
		return
	}
	text := strings.TrimSpace(string(tokenizer.Text()))
	if text != "" && state.current.Title == "" {
		state.current.Title = text
	}
}

type bingChallengeReader struct {
	reader  io.Reader
	tail    string
	blocked bool
}

func (reader *bingChallengeReader) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	if n == 0 || reader.blocked {
		return n, err
	}
	const tailSize = 80
	text := strings.ToLower(reader.tail + string(buffer[:n]))
	markers := []string{
		`class="captcha`,
		`id="captcha`,
		"verify you are human",
		"unusual traffic",
		"请输入验证码",
		"完成以下验证",
	}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			reader.blocked = true
			break
		}
	}
	if len(text) > tailSize {
		reader.tail = text[len(text)-tailSize:]
	} else {
		reader.tail = text
	}
	return n, err
}

func extractBingSnippet(tokenizer *nethtml.Tokenizer) string {
	var snippet strings.Builder
	depth := 1
	for depth > 0 {
		tokenType := tokenizer.Next()
		if tokenType == nethtml.ErrorToken {
			break
		}
		tag, _ := tokenizer.TagName()
		switch tokenType {
		case nethtml.StartTagToken:
			if strings.EqualFold(string(tag), "div") {
				depth++
			}
		case nethtml.EndTagToken:
			if strings.EqualFold(string(tag), "div") {
				depth--
			}
		case nethtml.TextToken:
			text := strings.TrimSpace(string(tokenizer.Text()))
			if depth > 0 && text != "" {
				if snippet.Len() > 0 {
					snippet.WriteByte(' ')
				}
				snippet.WriteString(text)
			}
		}
	}
	return snippet.String()
}
