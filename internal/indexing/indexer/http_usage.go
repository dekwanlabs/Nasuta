package indexer

import (
	"regexp"
	"strings"
)

// httpURLUsedByClient reports whether a URL literal is part of a concrete
// client/request expression. A URL kept in a constant, annotation, comment,
// or configuration file is not an inter-service dependency on its own.
func httpURLUsedByClient(text string, start, end int, clientCall *regexp.Regexp) bool {
	if httpURLIsComment(text, start, end) {
		return false
	}

	// The URL is accepted only when it is inside the argument list of an
	// actual client call. This handles multi-line calls without relying on a
	// fragile character window and prevents a nearby, unrelated URL constant
	// from being attributed to the request.
	for _, call := range clientCall.FindAllStringIndex(text, -1) {
		open := strings.IndexByte(text[call[0]:call[1]], '(')
		if open < 0 {
			continue
		}
		open += call[0]
		close := matchingParen(text, open)
		if close > end && start >= open && start < close {
			return true
		}
	}

	// Common clients receive a URL through a local variable:
	//   String base = "https://orders";
	//   restTemplate.getForObject(base + "/api", ...);
	// The declaration alone is not a dependency; the variable must be used
	// inside a concrete request call in the same source file.
	if variable := httpURLBindingName(text, start); variable != "" {
		for _, call := range clientCall.FindAllStringIndex(text, -1) {
			open := strings.IndexByte(text[call[0]:call[1]], '(')
			if open < 0 {
				continue
			}
			open += call[0]
			close := matchingParen(text, open)
			if close <= start {
				continue
			}
			args := text[open+1 : close]
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(variable) + `\b`).MatchString(args) {
				return true
			}
		}
	}
	return false
}

func httpURLIsComment(text string, start, end int) bool {
	lineStart := strings.LastIndexByte(text[:start], '\n') + 1
	lineEnd := strings.IndexByte(text[end:], '\n')
	if lineEnd < 0 {
		lineEnd = len(text)
	} else {
		lineEnd += end
	}
	trimmed := strings.TrimSpace(text[lineStart:lineEnd])
	return strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*")
}

func httpURLBindingName(text string, literalStart int) string {
	lineStart := strings.LastIndexByte(text[:literalStart], '\n') + 1
	left := strings.TrimRight(text[lineStart:literalStart], "\"'` \t")
	if idx := strings.LastIndex(left, "\n"); idx >= 0 {
		left = left[idx+1:]
	}
	match := regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*(?::=|=)\s*$`).FindStringSubmatch(left)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

func matchingParen(text string, open int) int {
	depth := 0
	var quote byte
	escaped := false
	for i := open; i < len(text); i++ {
		ch := text[i]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' || ch == '`' {
			quote = ch
			continue
		}
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
