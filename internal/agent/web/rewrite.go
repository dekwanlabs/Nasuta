package web

import (
	"context"
	"strings"
	"unicode"
)

// QueryRewriter rewrites a raw search query into a keyword-style query before
// it reaches the engine. A nil rewriter (or one that errors) falls back to the
// deterministic question-word strip in rewriteQuestionWords.
type QueryRewriter func(ctx context.Context, query string) (string, error)

// cjkQuestionWords are fixed Chinese question and modal particles. They carry
// no topical signal; stripping them keeps a single strong entity word (the
// thing a question is about) from dominating the engine's ranking. The list is
// language-general and never contains content terms.
var cjkQuestionWords = []string{
	"为什么", "是什么", "什么是", "是不是", "请问", "能否", "是否",
	"为何", "怎么", "怎样", "如何", "哪些", "哪个",
	"吗", "呢", "啊", "吧", "呀", "么", "哪", "什么",
}

// englishQuestionWords extends searchStopwords with question/filler words that
// add no topical signal to a web query.
var englishQuestionWords = map[string]struct{}{
	"does": {}, "did": {}, "do": {}, "can": {}, "could": {}, "should": {},
	"will": {}, "would": {}, "please": {}, "explain": {}, "tell": {}, "me": {},
	"i": {}, "we": {}, "you": {}, "my": {}, "about": {},
}

// rewriteQuestionWords is the deterministic fallback rewrite. It only removes
// fixed question words; it never reorders or expands terms, so it cannot
// overfit to any particular question.
func rewriteQuestionWords(query string) string {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return trimmed
	}
	rewritten := trimmed
	for _, word := range cjkQuestionWords {
		rewritten = strings.ReplaceAll(rewritten, word, "")
	}
	rewritten = stripEnglishQuestionWords(rewritten)
	rewritten = strings.Join(strings.Fields(rewritten), " ")
	if strings.TrimSpace(rewritten) == "" {
		return trimmed
	}
	return rewritten
}

func stripEnglishQuestionWords(value string) string {
	fields := strings.FieldsFunc(value, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, field := range fields {
		token := strings.ToLower(field)
		if _, skip := searchStopwords[token]; skip {
			continue
		}
		if _, skip := englishQuestionWords[token]; skip {
			continue
		}
		out = append(out, field)
	}
	return strings.Join(out, " ")
}
