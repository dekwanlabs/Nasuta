package agent

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/llm"
)

func TestWebEvidenceConvergesAfterIndependentFetches(t *testing.T) {
	var state webEvidenceState
	state.Observe(webFetchCall("https://a.example/article"), "evidence a")
	if hint := state.ConvergenceHint(); hint != "" {
		t.Fatalf("one domain produced hint %q", hint)
	}
	state.Observe(webFetchCall("https://b.example/report"), "evidence b")
	hint := state.ConvergenceHint()
	if !strings.Contains(hint, "2 independent web domains") {
		t.Fatalf("hint = %q", hint)
	}
	if repeated := state.ConvergenceHint(); repeated != "" {
		t.Fatalf("repeated hint = %q", repeated)
	}
}

func TestWebEvidenceIgnoresFailuresAndRepeatedDomains(t *testing.T) {
	var state webEvidenceState
	state.Observe(webFetchCall("https://health.baidu.com/one"), "evidence")
	state.Observe(webFetchCall("https://baike.baidu.com/two"), "evidence")
	state.Observe(webFetchCall("https://news.qq.com/report"), "error: unavailable")
	if hint := state.ConvergenceHint(); hint != "" {
		t.Fatalf("invalid evidence produced hint %q", hint)
	}
}

func TestWebEvidenceIgnoresEmptyAndFailedAutomaticFetches(t *testing.T) {
	var state webEvidenceState
	if state.Observe(webSearchCall(), `{"fetched":{"url":"https://baidu.com/page","content":"(empty body — status 403 Forbidden)"}}`) {
		t.Fatal("403 automatic fetch was accepted as evidence")
	}
	if state.Observe(webSearchCall(), `{"results":[],"fetch_note":"automatic fetch skipped"}`) {
		t.Fatal("skipped automatic fetch was accepted as evidence")
	}
	if hint := state.ConvergenceHint(); hint != "" {
		t.Fatalf("invalid automatic fetches produced hint %q", hint)
	}
}

func TestWebEvidenceCountsValidAutomaticFetch(t *testing.T) {
	var state webEvidenceState
	if !state.Observe(webSearchCall(), `{"fetched":{"url":"https://example.com/page","content":"status 200 OK\n\nrelevant evidence"}}`) {
		t.Fatal("valid automatic fetch was not accepted as evidence")
	}
}

func webFetchCall(rawURL string) llm.ToolCall {
	return llm.ToolCall{Function: llm.ToolFunction{Name: "web_fetch", Arguments: `{"url":"` + rawURL + `"}`}}
}

func webSearchCall() llm.ToolCall {
	return llm.ToolCall{Function: llm.ToolFunction{Name: "web_search"}}
}
