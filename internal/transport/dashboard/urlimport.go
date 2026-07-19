package dashboard

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/htmlconv"
)

// urlImportClient is the HTTP client used to fetch imported document URLs.
// Timeout is bounded so a hung URL can't stall the upload handler.
var urlImportClient = &http.Client{Timeout: 20 * time.Second}

// fetchURLContent fetches a URL and returns markdown-ready text.
// HTML is converted to a rough markdown approximation; plain text and markdown pass through.
// It returns both content and detected content type.
func fetchURLContent(ctx context.Context, rawURL string) (string, string, error) {
	if !strings.HasPrefix(strings.ToLower(rawURL), "http://") &&
		!strings.HasPrefix(strings.ToLower(rawURL), "https://") {
		return "", "", fmt.Errorf("url must start with http:// or https://")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "Nasuta-DocImport/1.0")
	resp, err := urlImportClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("fetch url: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("url returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10MB cap
	if err != nil {
		return "", "", fmt.Errorf("read body: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/html") {
		return htmlconv.Markdown(string(body)), "text/html", nil
	}
	if strings.Contains(contentType, "text/markdown") {
		return string(body), "text/markdown", nil
	}
	// Default: treat as plain text (works for .md served as text/plain).
	return string(body), "text/plain", nil
}
