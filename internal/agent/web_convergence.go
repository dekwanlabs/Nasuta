package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/dekwanlabs/nasuta/llm"
	"golang.org/x/net/publicsuffix"
)

type webEvidenceState struct {
	domains    map[string]struct{}
	lastHinted int
}

func (state *webEvidenceState) Observe(call llm.ToolCall, result string) bool {
	trimmed := strings.TrimSpace(result)
	if call.Function.Name == "web_search" {
		var response struct {
			Fetched *struct {
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"fetched"`
		}
		if json.Unmarshal([]byte(trimmed), &response) == nil && response.Fetched != nil && usableWebContent(response.Fetched.Content) {
			return state.observeURL(response.Fetched.URL)
		}
		return false
	}
	if call.Function.Name != "web_fetch" || strings.HasPrefix(trimmed, "error:") {
		return false
	}
	if !usableWebContent(trimmed) {
		return false
	}
	args, err := parseArgs(context.Background(), call.Function.Arguments)
	if err != nil {
		return false
	}
	rawURL, _ := args["url"].(string)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	return state.observeURL(parsed.String())
}

func usableWebContent(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	lower := strings.ToLower(content)
	return !strings.HasPrefix(lower, "(empty body") &&
		!strings.HasPrefix(lower, "status 401 unauthorized") &&
		!strings.HasPrefix(lower, "status 403 forbidden")
}

func (state *webEvidenceState) observeURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	if state.domains == nil {
		state.domains = make(map[string]struct{})
	}
	host := strings.ToLower(parsed.Hostname())
	if registrable, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		host = registrable
	}
	state.domains[host] = struct{}{}
	return true
}

func (state *webEvidenceState) ConvergenceHint() string {
	count := len(state.domains)
	if count < 2 || count <= state.lastHinted {
		return ""
	}
	state.lastHinted = count
	return fmt.Sprintf(`[WEB_EVIDENCE_STATUS] You have successfully fetched evidence from %d independent web domains. If the evidence now supports the requested conclusion, answer immediately. Continue searching only for a specific unresolved fact, ambiguity, or source-quality gap that you can name; do not keep searching merely to accumulate more pages.`, count)
}
