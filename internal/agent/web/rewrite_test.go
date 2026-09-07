package web

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSignalsMatchRequiresCJKForMixedQuery(t *testing.T) {
	qLatin, qCJK := searchSignals("PostgreSQL 主从复制如何实现")
	cLatin, cCJK := searchSignals("PostgreSQL is a powerful open source database")
	if signalsMatch(qLatin, qCJK, cLatin, cCJK) {
		t.Fatal("mixed query matched a candidate that lacks the CJK mechanism")
	}

	okLatin, okCJK := searchSignals("PostgreSQL 主从复制 streaming replication 配置")
	if !signalsMatch(qLatin, qCJK, okLatin, okCJK) {
		t.Fatal("mixed query should match when the CJK mechanism is present")
	}
}

func TestSignalsMatchLatinOnly(t *testing.T) {
	qLatin, qCJK := searchSignals("How does PostgreSQL replication work?")
	cLatin, cCJK := searchSignals("PostgreSQL replication documentation")
	if !signalsMatch(qLatin, qCJK, cLatin, cCJK) {
		t.Fatal("Latin-only query should match on any content word")
	}
}

func TestContentAddressesQueryRejectsOffTopicPage(t *testing.T) {
	content := "PostgreSQL is a powerful open source object-relational database system"
	if contentAddressesQuery(content, "PostgreSQL 主从复制如何实现") {
		t.Fatal("homepage lacking the CJK mechanism passed the page gate")
	}
	if !contentAddressesQuery(content+" 主从复制 通过 wal 日志流式同步", "PostgreSQL 主从复制如何实现") {
		t.Fatal("page covering the CJK mechanism should pass the page gate")
	}
}

func TestContentAddressesQueryRequiresEveryLatinWord(t *testing.T) {
	if contentAddressesQuery("PostgreSQL is a database", "PostgreSQL replication") {
		t.Fatal("page missing one Latin content word passed the page gate")
	}
	if !contentAddressesQuery("PostgreSQL replication uses WAL", "PostgreSQL replication") {
		t.Fatal("page covering all Latin content words should pass")
	}
}

func TestRewriteQuestionWordsDropsCJKParticles(t *testing.T) {
	got := rewriteQuestionWords("央行购金为什么支撑黄金价格")
	if strings.Contains(got, "为什么") {
		t.Fatalf("rewriteQuestionWords() = %q, kept question word", got)
	}
	for _, term := range []string{"央行", "购金", "支撑", "黄金", "价格"} {
		if !strings.Contains(got, term) {
			t.Fatalf("rewriteQuestionWords() = %q, dropped content term %q", got, term)
		}
	}
}

func TestRewriteQuestionWordsDropsEnglishQuestionWords(t *testing.T) {
	got := rewriteQuestionWords("How does PostgreSQL replication work?")
	for _, skip := range []string{"how", "does"} {
		if strings.Contains(strings.ToLower(got), skip) {
			t.Fatalf("rewriteQuestionWords() = %q, kept %q", got, skip)
		}
	}
	if !strings.Contains(got, "PostgreSQL") || !strings.Contains(got, "replication") {
		t.Fatalf("rewriteQuestionWords() = %q, dropped content terms", got)
	}
}

func TestRewriteQuestionWordsKeepsContentWhenAllWordsAreStopwords(t *testing.T) {
	if got := rewriteQuestionWords("为什么 如何"); got != "为什么 如何" {
		t.Fatalf("rewriteQuestionWords() = %q, want original preserved", got)
	}
}

// captureProvider records the query the service actually dispatches, so tests
// can assert the rewrite reached the search backend rather than only the
// rewrite function.
type captureProvider struct {
	query   string
	results []SearchResult
}

func (p *captureProvider) Search(_ context.Context, query string, _ int) ([]SearchResult, error) {
	p.query = query
	return p.results, nil
}

func TestSearchRewritesQueryBeforeDispatch(t *testing.T) {
	capture := &captureProvider{results: []SearchResult{{Title: "t", URL: "https://example.com"}}}
	srv := &Service{}
	if err := srv.RegisterProvider("bing", capture); err != nil {
		t.Fatal(err)
	}
	srv.SetSearchEngine("bing")

	if _, err := srv.Search(context.Background(), "央行购金为什么支撑黄金价格", 5); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capture.query, "为什么") {
		t.Fatalf("provider received unrewritten query %q", capture.query)
	}
	if !strings.Contains(capture.query, "黄金") || !strings.Contains(capture.query, "价格") {
		t.Fatalf("provider query %q lost content terms", capture.query)
	}
}

func TestSearchUsesInjectedRewriter(t *testing.T) {
	capture := &captureProvider{results: []SearchResult{{Title: "t", URL: "https://example.com"}}}
	srv := &Service{}
	if err := srv.RegisterProvider("test", capture); err != nil {
		t.Fatal(err)
	}
	srv.SetSearchEngine("test")
	srv.SetQueryRewriter(func(_ context.Context, _ string) (string, error) {
		return "gold price central bank buying", nil
	})

	if _, err := srv.Search(context.Background(), "央行购金为什么支撑黄金价格", 5); err != nil {
		t.Fatal(err)
	}
	if capture.query != "gold price central bank buying" {
		t.Fatalf("provider query = %q, want rewriter output", capture.query)
	}
}

func TestSearchFallsBackWhenRewriterErrors(t *testing.T) {
	capture := &captureProvider{results: []SearchResult{{Title: "t", URL: "https://example.com"}}}
	srv := &Service{}
	if err := srv.RegisterProvider("test", capture); err != nil {
		t.Fatal(err)
	}
	srv.SetSearchEngine("test")
	srv.SetQueryRewriter(func(_ context.Context, _ string) (string, error) {
		return "", errors.New("rewriter unavailable")
	})

	if _, err := srv.Search(context.Background(), "央行购金为什么支撑黄金价格", 5); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(capture.query, "为什么") {
		t.Fatalf("provider received unrewritten query %q", capture.query)
	}
}
