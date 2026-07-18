package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dekwanlabs/astris/internal/platform/htmlconv"
	"github.com/dekwanlabs/astris/internal/retrieval"
	"github.com/dekwanlabs/astris/log"
	nethtml "golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

// ── Public API ──────────────────────────────────────────────────────────────────

// WebSearchResult is one result from a web search.
type WebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// WebSearch dispatches to the configured search engine.
func (srv *Service) WebSearch(ctx context.Context, query string, limit int) ([]WebSearchResult, error) {
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

	log.InfofCtx(ctx, "[web_search] engine=%s query=%q → %d results",
		srv.webSearchEngine, truncateForLog(query, 60), len(results))
	return results, nil
}

// WebFetch downloads a page and returns readable text.
func (srv *Service) WebFetch(ctx context.Context, rawURL string) (string, error) {
	return srv.WebFetchRelevant(ctx, rawURL, "")
}

// WebFetchRelevant keeps the model input bounded by selecting passages locally.
func (srv *Service) WebFetchRelevant(ctx context.Context, rawURL, query string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("URL must be an absolute http(s) address")
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,text/plain,text/markdown,application/json,*/*;q=0.5")

	resp, err := ssrfSafeClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	out, encodingName, err := decodeWebBody(body, contentType)
	if err != nil {
		return "", fmt.Errorf("decode body from %s: %w", rawURL, err)
	}
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "text/html") || looksLikeHTML(out) {
		out = htmlconv.Markdown(out)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return fmt.Sprintf("(empty body — status %s)", resp.Status), nil
	}

	const modelBudget = 8000
	if strings.TrimSpace(query) != "" {
		selection := retrieval.SelectWebPassages(out, query, modelBudget)
		out = formatWebPassages(selection)
	} else {
		out = truncateRunes(out, modelBudget)
	}

	header := fmt.Sprintf("status %s · %s · %s · %d bytes\n\n", resp.Status, contentTypeShort(ct), encodingName, len(body))
	result := header + out

	log.InfofCtx(ctx, "[web_fetch] url=%q → %d chars (status=%s)", truncateForLog(rawURL, 80), len(out), resp.Status)
	return result, nil
}

func decodeWebBody(body []byte, contentType string) (string, string, error) {
	encoding, name, certain := charset.DetermineEncoding(body, contentType)
	if utf8.Valid(body) && !certain && name == "windows-1252" {
		return string(body), "utf-8", nil
	}
	decoded, err := encoding.NewDecoder().Bytes(body)
	if err != nil {
		return "", "", fmt.Errorf("convert %s to UTF-8: %w", name, err)
	}
	if !utf8.Valid(decoded) {
		return "", "", fmt.Errorf("converted %s body is not valid UTF-8", name)
	}
	return string(decoded), name, nil
}

func formatWebPassages(selection retrieval.WebPassageSelection) string {
	if len(selection.Passages) == 0 {
		return truncateRunes(selection.Fallback, 8000)
	}
	var out strings.Builder
	if selection.Title != "" {
		out.WriteString("# ")
		out.WriteString(selection.Title)
		out.WriteString("\n\n")
	}
	for _, passage := range selection.Passages {
		if passage.Heading != "" && passage.Heading != selection.Title {
			out.WriteString("## ")
			out.WriteString(passage.Heading)
			out.WriteString("\n\n")
		}
		out.WriteString(passage.Content)
		out.WriteString("\n\n")
	}
	out.WriteString(fmt.Sprintf("[selected %d of %d passages locally with BM25]", len(selection.Passages), selection.TotalPassages))
	return truncateRunes(strings.TrimSpace(out.String()), 8000)
}

func truncateRunes(value string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	marker := []rune("\n...(truncated)")
	if max <= len(marker) {
		return string(runes[:max])
	}
	return string(runes[:max-len(marker)]) + string(marker)
}

// ── Engine dispatcher ──────────────────────────────────────────────────────────

func (srv *Service) dispatchSearch(ctx context.Context, query string, limit int) ([]WebSearchResult, error) {
	switch srv.webSearchEngine {
	case "brave":
		return srv.searchBrave(ctx, query, limit)
	case "bing":
		return srv.searchBing(ctx, query, limit)
	default:
		return srv.searchDuckDuckGo(ctx, query, limit)
	}
}

// ── DuckDuckGo ─────────────────────────────────────────────────────────────────

func (srv *Service) searchDuckDuckGo(ctx context.Context, query string, limit int) ([]WebSearchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
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

// parseDDGResults extracts results from DuckDuckGo's HTML result page.
func parseDDGResults(r io.Reader, limit int) []WebSearchResult {
	var results []WebSearchResult
	z := nethtml.NewTokenizer(r)
	var cur WebSearchResult
	var inResult, inSnippet bool

	for {
		if len(results) >= limit {
			break
		}
		tt := z.Next()
		if tt == nethtml.ErrorToken {
			break
		}
		tag, _ := z.TagName()
		switch tt {
		case nethtml.StartTagToken:
			if isDiv(tag, z, "result__body") {
				cur = WebSearchResult{}
				inResult = true
			}
			if inResult && isAnchor(tag, z, "result__a") {
				cur.URL = extractAttr(z, "href")
			}
			if inResult && isAnchor(tag, z, "result__snippet") {
				inSnippet = true
			}
		case nethtml.EndTagToken:
			if isTag(tag, "a") && inResult && inSnippet {
				inSnippet = false
			}
			if isTag(tag, "div") && inResult {
				if cur.URL != "" && cur.Title != "" {
					results = append(results, cur)
				}
				inResult = false
				inSnippet = false
			}
		case nethtml.TextToken:
			if inResult && !inSnippet {
				t := strings.TrimSpace(string(z.Text()))
				if t != "" && cur.Title == "" {
					cur.Title = t
				}
			}
			if inSnippet {
				t := strings.TrimSpace(string(z.Text()))
				if t != "" {
					if cur.Snippet != "" {
						cur.Snippet += " "
					}
					cur.Snippet += t
				}
			}
		}
	}
	return results
}

// ── Brave Search API ──────────────────────────────────────────────────────────

const braveEndpoint = "https://api.search.brave.com/res/v1/web/search"

func (srv *Service) searchBrave(ctx context.Context, query string, limit int) ([]WebSearchResult, error) {
	if srv.webSearchAPIKey == "" {
		return nil, fmt.Errorf("brave search requires CODELOOM_WEB_SEARCH_API_KEY")
	}

	u := fmt.Sprintf("%s?q=%s&count=%d", braveEndpoint, url.QueryEscape(query), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("X-Subscription-Token", srv.webSearchAPIKey)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("brave search: %w", err)
	}
	defer resp.Body.Close()

	if !respOK(resp) {
		return nil, fmt.Errorf("brave: HTTP %d — %s", resp.StatusCode, fetchErrorHint(resp.StatusCode))
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

	results := make([]WebSearchResult, 0, len(data.Web.Results))
	for _, r := range data.Web.Results {
		results = append(results, WebSearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Description,
		})
		if len(results) >= limit {
			break
		}
	}
	return results, nil
}

// ── Bing (CN) HTML ────────────────────────────────────────────────────────────
//
// Uses cn.bing.com — the CN endpoint returns raw URLs in the HTML rather than
// click-tracking redirects that the international www.bing.com wraps.

const bingEndpoint = "https://cn.bing.com/search"

func (srv *Service) searchBing(ctx context.Context, query string, limit int) ([]WebSearchResult, error) {
	u := fmt.Sprintf("%s?q=%s", bingEndpoint, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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

	return parseBingResults(resp.Body, limit), nil
}

// parseBingResults extracts results from cn.bing.com HTML.
// Each result is in <li class="b_algo"> with <h2><a href> title and <p> snippet.
func parseBingResults(r io.Reader, limit int) []WebSearchResult {
	var results []WebSearchResult
	z := nethtml.NewTokenizer(r)
	var cur WebSearchResult
	var inAlgo, inTitle bool

	for {
		if len(results) >= limit {
			break
		}
		tt := z.Next()
		if tt == nethtml.ErrorToken {
			break
		}
		tag, _ := z.TagName()
		switch tt {
		case nethtml.StartTagToken:
			tagStr := strings.ToLower(string(tag))
			if tagStr == "li" && hasClass(z, "b_algo") {
				cur = WebSearchResult{}
				inAlgo = true
			}
			if inAlgo && tagStr == "h2" {
				inTitle = true
			}
			if inAlgo && inTitle && tagStr == "a" {
				cur.URL = extractAttr(z, "href")
			}
			// Bing wraps snippets in <p> within <div class="b_caption">.
			if inAlgo && tagStr == "div" && hasClass(z, "b_caption") {
				cur.Snippet = extractBingSnippet(z)
			}
		case nethtml.EndTagToken:
			tagStr := strings.ToLower(string(tag))
			if tagStr == "li" && inAlgo {
				if cur.URL != "" && cur.Title != "" {
					results = append(results, cur)
				}
				inAlgo = false
				inTitle = false
			}
			if tagStr == "h2" && inTitle {
				inTitle = false
			}
		case nethtml.TextToken:
			if inAlgo && inTitle {
				t := strings.TrimSpace(string(z.Text()))
				if t != "" && cur.Title == "" {
					cur.Title = t
				}
			}
		}
	}
	return results
}

// extractBingSnippet reads text from the first <p> inside a b_caption div.
func extractBingSnippet(z *nethtml.Tokenizer) string {
	var b strings.Builder
	depth := 1
	for depth > 0 {
		tt := z.Next()
		if tt == nethtml.ErrorToken {
			break
		}
		tag, _ := z.TagName()
		switch tt {
		case nethtml.StartTagToken:
			tagStr := strings.ToLower(string(tag))
			if tagStr == "div" || tagStr == "p" {
				depth++
			}
		case nethtml.EndTagToken:
			tagStr := strings.ToLower(string(tag))
			if tagStr == "div" {
				depth--
			}
		case nethtml.TextToken:
			if depth > 0 {
				t := strings.TrimSpace(string(z.Text()))
				if t != "" {
					if b.Len() > 0 {
						b.WriteByte(' ')
					}
					b.WriteString(t)
				}
			}
		}
	}
	return b.String()
}

// ── HTML parsing helpers ──────────────────────────────────────────────────────

const userAgent = "Mozilla/5.0 (compatible; Astris/1.0)"

func respOK(resp *http.Response) bool { return resp.StatusCode >= 200 && resp.StatusCode < 300 }

func fetchErrorHint(status int) string {
	switch status {
	case 401, 403:
		return "API key rejected — check CODELOOM_WEB_SEARCH_API_KEY"
	case 429:
		return "rate limited — wait and retry"
	default:
		return "server error"
	}
}

func isAnchor(tag []byte, z *nethtml.Tokenizer, class string) bool {
	return isTag(tag, "a") && hasClass(z, class)
}

func isDiv(tag []byte, z *nethtml.Tokenizer, class string) bool {
	return isTag(tag, "div") && hasClass(z, class)
}

func isTag(tag []byte, name string) bool {
	return len(tag) == len(name) && strings.EqualFold(string(tag), name)
}

func hasClass(z *nethtml.Tokenizer, target string) bool {
	for {
		key, val, more := z.TagAttr()
		if isTag(key, "class") {
			for _, c := range strings.Fields(string(val)) {
				if c == target {
					return true
				}
			}
			return false
		}
		if !more {
			break
		}
	}
	return false
}

func extractAttr(z *nethtml.Tokenizer, name string) string {
	for {
		key, val, more := z.TagAttr()
		if isTag(key, name) {
			return string(val)
		}
		if !more {
			break
		}
	}
	return ""
}

// ── HTML → text (web_fetch) ───────────────────────────────────────────────────

const fetchMaxBytes = 1 << 20 // 1 MiB

func looksLikeHTML(s string) bool {
	head := s
	if len(head) > 512 {
		head = head[:512]
	}
	low := strings.ToLower(head)
	return strings.Contains(low, "<!doctype html") || strings.Contains(low, "<html")
}

func contentTypeShort(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.TrimSpace(ct)
}

// ── SSRF protection ───────────────────────────────────────────────────────────

var cgnatCIDR = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

func blockedFetchIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() ||
		cgnatCIDR.Contains(ip)
}

func ssrfSafeClient() *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if blockedFetchIP(ip.IP) {
						return nil, fmt.Errorf("refusing to fetch internal address %s (resolves to %s)", host, ip.IP)
					}
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
			},
		},
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func truncateForLog(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
