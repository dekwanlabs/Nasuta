package agent

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/dekwanlabs/astris/llm"
	"golang.org/x/net/publicsuffix"
)

type webEvidenceState struct {
	domains    map[string]struct{}
	lastHinted int
}

func (state *webEvidenceState) Observe(call llm.ToolCall, result string) {
	if call.Function.Name != "web_fetch" || strings.HasPrefix(strings.TrimSpace(result), "error:") {
		return
	}
	args, err := parseArgs(context.Background(), call.Function.Arguments)
	if err != nil {
		return
	}
	rawURL, _ := args["url"].(string)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" {
		return
	}
	if state.domains == nil {
		state.domains = make(map[string]struct{})
	}
	host := strings.ToLower(parsed.Hostname())
	if registrable, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		host = registrable
	}
	state.domains[host] = struct{}{}
}

func (state *webEvidenceState) ConvergenceHint() string {
	count := len(state.domains)
	if count < 2 || count <= state.lastHinted {
		return ""
	}
	state.lastHinted = count
	return fmt.Sprintf(`[WEB_EVIDENCE_STATUS] You have successfully fetched evidence from %d independent web domains. If the evidence now supports the requested conclusion, answer immediately. Continue searching only for a specific unresolved fact, ambiguity, or source-quality gap that you can name; do not keep searching merely to accumulate more pages.`, count)
}
