package web

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/platform/htmlconv"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
	"golang.org/x/net/html/charset"
)

const fetchMaxBytes = 1 << 20

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

	client := srv.fetchClient
	if client == nil {
		client = ssrfSafeClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if !respOK(resp) {
		return "", fmt.Errorf("fetch %s: HTTP %s", rawURL, resp.Status)
	}

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
		return "", fmt.Errorf("fetch %s: empty response body", rawURL)
	}

	const modelBudget = 8000
	if strings.TrimSpace(query) != "" {
		out = formatWebPassages(retrieval.SelectWebPassages(out, query, modelBudget))
	} else {
		out = truncateRunes(out, modelBudget)
	}

	header := fmt.Sprintf(
		"status %s · %s · %s · %d bytes\n\n",
		resp.Status,
		contentTypeShort(ct),
		encodingName,
		len(body),
	)
	log.InfofCtx(
		ctx,
		"[web_fetch] url=%q → %d chars (status=%s)",
		truncateForLog(rawURL, 80),
		len(out),
		resp.Status,
	)
	return header + out, nil
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
	fmt.Fprintf(
		&out,
		"[selected %d of %d passages locally with BM25]",
		len(selection.Passages),
		selection.TotalPassages,
	)
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

var cgnatCIDR = func() *net.IPNet {
	_, network, _ := net.ParseCIDR("100.64.0.0/10")
	return network
}()

func blockedFetchIP(ip net.IP) bool {
	return !ip.IsGlobalUnicast() || ip.IsPrivate() || cgnatCIDR.Contains(ip)
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
				if len(ips) == 0 {
					return nil, fmt.Errorf("host %s resolved without an address", host)
				}
				for _, ip := range ips {
					if blockedFetchIP(ip.IP) {
						return nil, fmt.Errorf(
							"refusing to fetch internal address %s (resolves to %s)",
							host,
							ip.IP,
						)
					}
				}
				return dialer.DialContext(
					ctx,
					network,
					net.JoinHostPort(ips[0].IP.String(), port),
				)
			},
		},
	}
}
