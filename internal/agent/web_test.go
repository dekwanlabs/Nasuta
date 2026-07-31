package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecodeWebBodyUsesHTMLMetaCharset(t *testing.T) {
	body := append([]byte(`<html><head><meta http-equiv="Content-Type" content="text/html; charset=gb2312"></head><body>`),
		[]byte{0xc8, 0xba, 0xd3, 0xf1, 0xb4, 0xe5}...)
	body = append(body, []byte(`</body></html>`)...)

	got, encoding, err := decodeWebBody(body, "text/html")
	if err != nil {
		t.Fatalf("decodeWebBody() error = %v", err)
	}
	if encoding != "gbk" {
		t.Fatalf("encoding = %q, want gbk", encoding)
	}
	if !strings.Contains(got, "群玉村") {
		t.Fatalf("decoded body = %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatal("decoded body is not valid UTF-8")
	}
}

func TestDecodeWebBodyKeepsUndeclaredUTF8(t *testing.T) {
	want := "<html><body>海南临高</body></html>"
	got, encoding, err := decodeWebBody([]byte(want), "text/html")
	if err != nil {
		t.Fatalf("decodeWebBody() error = %v", err)
	}
	if encoding != "utf-8" || got != want {
		t.Fatalf("decodeWebBody() = (%q, %q), want (%q, utf-8)", got, encoding, want)
	}
}

func TestTruncateRunesPreservesUTF8(t *testing.T) {
	got := truncateRunes(strings.Repeat("中", 9000), 8000)
	if !utf8.ValidString(got) {
		t.Fatal("truncateRunes() split a UTF-8 rune")
	}
	if !strings.HasSuffix(got, "\n...(truncated)") {
		t.Fatalf("truncateRunes() missing marker: %q", got[len(got)-30:])
	}
}

func TestWebSearchRejectsUnknownEngine(t *testing.T) {
	srv := &Service{}
	srv.SetWebSearchEngine(" SearXNG ")

	_, err := srv.WebSearch(context.Background(), "query", 1)
	if err == nil || !strings.Contains(err.Error(), `unsupported web search provider "searxng"`) {
		t.Fatalf("WebSearch error = %v, want unsupported-engine error", err)
	}
}

func TestSetWebSearchEngineCanonicalizesValue(t *testing.T) {
	srv := &Service{}
	srv.SetWebSearchEngine(" Bing ")
	if srv.webSearchEngine != "bing" {
		t.Fatalf("webSearchEngine = %q, want %q", srv.webSearchEngine, "bing")
	}
}

type stubWebSearchProvider struct{}

func (stubWebSearchProvider) Search(context.Context, string, int) ([]WebSearchResult, error) {
	return []WebSearchResult{{Title: "stub", URL: "https://example.com"}}, nil
}

type urlWebSearchProvider struct{ url string }

func (p urlWebSearchProvider) Search(context.Context, string, int) ([]WebSearchResult, error) {
	return []WebSearchResult{{Title: "source", URL: p.url, Snippet: "authoritative answer to the question"}}, nil
}

func TestWebSearchWithFetchCombinesSearchAndEvidence(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><h1>Source</h1><p>authoritative evidence</p></body></html>`))
	}))
	defer page.Close()

	srv := &Service{}
	if err := srv.RegisterWebSearchProvider("test", urlWebSearchProvider{url: page.URL}); err != nil {
		t.Fatalf("RegisterWebSearchProvider() error = %v", err)
	}
	srv.SetWebSearchEngine("test")

	response, err := srv.WebSearchWithFetch(context.Background(), "question", 5)
	if err != nil {
		t.Fatalf("WebSearchWithFetch() error = %v", err)
	}
	if len(response.Results) != 1 || response.Fetched == nil || !strings.Contains(response.Fetched.Content, "authoritative evidence") {
		t.Fatalf("response = %+v, want search result and fetched evidence", response)
	}
}

func TestWebSearchDispatchesThroughRegisteredProvider(t *testing.T) {
	srv := &Service{}
	if err := srv.RegisterWebSearchProvider("internal-test", stubWebSearchProvider{}); err != nil {
		t.Fatalf("RegisterWebSearchProvider() error = %v", err)
	}
	srv.SetWebSearchEngine(" internal-test ")

	results, err := srv.WebSearch(context.Background(), "query", 1)
	if err != nil {
		t.Fatalf("WebSearch() error = %v", err)
	}
	if len(results) != 1 || results[0].Title != "stub" {
		t.Fatalf("WebSearch() = %+v, want stub result", results)
	}
}

func TestParseBingResultsKeepsResultAfterNestedCaptionContent(t *testing.T) {
	html := `<ol id="b_results"><li class="b_algo"><h2><a href="https://example.com"><strong>Example</strong> result</a></h2><div class="b_caption"><p class="b_lineclamp2"><span>Snippet text</span></p></div></li></ol>`
	results := parseBingResults(strings.NewReader(html), 1)
	if len(results) != 1 {
		t.Fatalf("parseBingResults() returned %d results, want 1", len(results))
	}
	if results[0].URL != "https://example.com" || results[0].Title != "Example" || results[0].Snippet != "Snippet text" {
		t.Fatalf("unexpected result: %+v", results[0])
	}
}

func TestParseBingResultsDetectsChallenge(t *testing.T) {
	html := `<html><body><div class="captcha"><p>请输入验证码</p></div></body></html>`
	detector := &bingChallengeReader{reader: strings.NewReader(html)}
	if _, err := io.ReadAll(detector); err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !detector.blocked {
		t.Fatal("bingChallengeReader did not detect challenge response")
	}
}

func TestRelevantFetchCandidateRejectsBroadAndUnrelatedResults(t *testing.T) {
	results := []WebSearchResult{
		{Title: "海南", Snippet: "海南省旅游信息", URL: "https://example.com/hainan"},
		{Title: "Kmart Australia", Snippet: "Store locations", URL: "https://example.com/kmart"},
		{Title: "临高中学", Snippet: "学校介绍与招生信息", URL: "https://example.com/school"},
	}
	got, ok := relevantFetchCandidate("海南省临高中学 怎么样 评价", results)
	if !ok || got.URL != "https://example.com/school" {
		t.Fatalf("relevantFetchCandidate() = (%+v, %v), want school result", got, ok)
	}
}

func TestRelevantFetchCandidateSupportsLatinTerms(t *testing.T) {
	results := []WebSearchResult{{Title: "PostgreSQL documentation", URL: "https://example.com/postgres"}}
	got, ok := relevantFetchCandidate("How does PostgreSQL replication work?", results)
	if !ok || got.URL != results[0].URL {
		t.Fatalf("relevantFetchCandidate() = (%+v, %v), want PostgreSQL result", got, ok)
	}
}

func TestWebFetchRelevantRejectsNonSuccessStatus(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer page.Close()

	srv := &Service{}
	_, err := srv.WebFetchRelevant(context.Background(), page.URL, "question")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403 Forbidden") {
		t.Fatalf("WebFetchRelevant() error = %v, want HTTP status error", err)
	}
}

func TestWebSearchWithFetchSkipsIrrelevantCandidate(t *testing.T) {
	srv := &Service{}
	if err := srv.RegisterWebSearchProvider("test", urlWebSearchProvider{url: "https://example.invalid"}); err != nil {
		t.Fatalf("RegisterWebSearchProvider() error = %v", err)
	}
	srv.SetWebSearchEngine("test")

	response, err := srv.WebSearchWithFetch(context.Background(), "unrelated target", 5)
	if err != nil {
		t.Fatalf("WebSearchWithFetch() error = %v", err)
	}
	if response.Fetched != nil || !strings.Contains(response.FetchNote, "skipped") {
		t.Fatalf("response = %+v, want skipped automatic fetch", response)
	}
}

func TestWebSearchToolPayloadIsDeliveredWithoutLoss(t *testing.T) {
	response := WebSearchResponse{
		Results: []WebSearchResult{{Title: "relevant candidate", URL: "https://example.com/result", Snippet: "candidate summary"}},
		Fetched: &WebFetchedEvidence{Title: "large page", URL: "https://example.com/page", Content: strings.Repeat("page content ", 4000)},
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	if got := formatToolResultForLLM("web_search", string(raw)); got != string(raw) {
		t.Fatal("web search payload changed before model delivery")
	}
}
