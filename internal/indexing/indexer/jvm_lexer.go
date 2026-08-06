package indexer

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

type jvmTokenKind uint8

const (
	jvmIdentifierToken jvmTokenKind = iota
	jvmStringToken
	jvmCharToken
	jvmSymbolToken
)

type jvmToken struct {
	kind       jvmTokenKind
	text       string
	start, end int
	line       int
	braceDepth int
}

type jvmAnnotationArgument struct {
	name   string
	tokens []jvmToken
}

type jvmAnnotation struct {
	name, qualifiedName  string
	arguments            []jvmAnnotationArgument
	text                 string
	line, braceDepth     int
	tokenStart, tokenEnd int
	start, end           int
}

type jvmSource struct {
	tokens      []jvmToken
	annotations []jvmAnnotation
	imports     map[string]string
}

func scanJVMSource(text string) jvmSource {
	tokens := lexJVM(text)
	return jvmSource{
		tokens:      tokens,
		annotations: extractJVMAnnotations(text, tokens),
		imports:     extractJVMImports(tokens),
	}
}

// lexJVM keeps literals as single tokens so annotation delimiters inside them
// cannot affect structural parsing.
func lexJVM(text string) []jvmToken {
	tokens := make([]jvmToken, 0, len(text)/8)
	line := 1
	braceDepth := 0
	for i := 0; i < len(text); {
		switch {
		case strings.HasPrefix(text[i:], "//"):
			i += 2
			for i < len(text) && text[i] != '\n' {
				_, size := utf8.DecodeRuneInString(text[i:])
				i += size
			}
		case strings.HasPrefix(text[i:], "/*"):
			i += 2
			depth := 1
			for i < len(text) && depth > 0 {
				switch {
				case strings.HasPrefix(text[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(text[i:], "*/"):
					depth--
					i += 2
				default:
					r, size := utf8.DecodeRuneInString(text[i:])
					if r == '\n' {
						line++
					}
					i += size
				}
			}
		case strings.HasPrefix(text[i:], `"""`):
			start, startLine := i, line
			i += 3
			for i < len(text) {
				if strings.HasPrefix(text[i:], `"""`) && !jvmEscapedAt(text, i) {
					i += 3
					break
				}
				r, size := utf8.DecodeRuneInString(text[i:])
				if r == '\n' {
					line++
				}
				i += size
			}
			tokens = append(tokens, jvmToken{
				kind: jvmStringToken, text: text[start:i],
				start: start, end: i, line: startLine, braceDepth: braceDepth,
			})
		case text[i] == '"':
			start, startLine := i, line
			i = scanJVMQuoted(text, i, '"', &line)
			tokens = append(tokens, jvmToken{
				kind: jvmStringToken, text: text[start:i],
				start: start, end: i, line: startLine, braceDepth: braceDepth,
			})
		case text[i] == '\'':
			start, startLine := i, line
			i = scanJVMQuoted(text, i, '\'', &line)
			tokens = append(tokens, jvmToken{
				kind: jvmCharToken, text: text[start:i],
				start: start, end: i, line: startLine, braceDepth: braceDepth,
			})
		default:
			r, size := utf8.DecodeRuneInString(text[i:])
			if unicode.IsSpace(r) {
				if r == '\n' {
					line++
				}
				i += size
				continue
			}
			if isJVMIdentifierStart(r) {
				start, startLine := i, line
				i += size
				for i < len(text) {
					next, nextSize := utf8.DecodeRuneInString(text[i:])
					if !isJVMIdentifierPart(next) {
						break
					}
					i += nextSize
				}
				tokens = append(tokens, jvmToken{
					kind: jvmIdentifierToken, text: text[start:i],
					start: start, end: i, line: startLine, braceDepth: braceDepth,
				})
				continue
			}
			tokenDepth := braceDepth
			if r == '}' && braceDepth > 0 {
				braceDepth--
				tokenDepth = braceDepth
			}
			tokens = append(tokens, jvmToken{
				kind: jvmSymbolToken, text: text[i : i+size],
				start: i, end: i + size, line: line, braceDepth: tokenDepth,
			})
			if r == '{' {
				braceDepth++
			}
			i += size
		}
	}
	return tokens
}

func scanJVMQuoted(text string, start int, quote byte, line *int) int {
	i := start + 1
	escaped := false
	for i < len(text) {
		ch := text[i]
		i++
		if ch == '\n' {
			(*line)++
		}
		switch {
		case escaped:
			escaped = false
		case ch == '\\':
			escaped = true
		case ch == quote:
			return i
		}
	}
	return i
}

func jvmEscapedAt(text string, index int) bool {
	backslashes := 0
	for i := index - 1; i >= 0 && text[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func isJVMIdentifierStart(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.Is(unicode.Sc, r)
}

func isJVMIdentifierPart(r rune) bool {
	return isJVMIdentifierStart(r) || unicode.IsDigit(r) ||
		unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) ||
		unicode.Is(unicode.Pc, r)
}

// extractJVMAnnotations returns direct annotations only. Nested annotations
// remain part of their parent's argument expression, not declaration metadata.
func extractJVMAnnotations(text string, tokens []jvmToken) []jvmAnnotation {
	annotations := make([]jvmAnnotation, 0)
	for i := 0; i < len(tokens); {
		annotation, next, ok := parseJVMAnnotation(text, tokens, i)
		if !ok {
			i++
			continue
		}
		annotations = append(annotations, annotation)
		i = next
	}
	return annotations
}

func extractJVMImports(tokens []jvmToken) map[string]string {
	imports := make(map[string]string)
	for i := 0; i < len(tokens); i++ {
		if tokens[i].text != "import" {
			continue
		}
		i++
		if i < len(tokens) && tokens[i].text == "static" {
			i++
		}
		parts := make([]string, 0, 4)
		for i < len(tokens) && tokens[i].text != ";" {
			if tokens[i].kind == jvmIdentifierToken {
				parts = append(parts, tokens[i].text)
			}
			i++
		}
		if len(parts) == 0 {
			continue
		}
		qualified := strings.Join(parts, ".")
		simple := parts[len(parts)-1]
		imports[simple] = qualified
	}
	return imports
}

func parseJVMAnnotation(text string, tokens []jvmToken, start int) (jvmAnnotation, int, bool) {
	if start >= len(tokens) || tokens[start].text != "@" ||
		start+1 >= len(tokens) || tokens[start+1].kind != jvmIdentifierToken {
		return jvmAnnotation{}, start + 1, false
	}

	parts := []string{tokens[start+1].text}
	end := start + 2
	for end+1 < len(tokens) && tokens[end].text == "." &&
		tokens[end+1].kind == jvmIdentifierToken {
		parts = append(parts, tokens[end+1].text)
		end += 2
	}
	var arguments []jvmAnnotationArgument
	if end < len(tokens) && tokens[end].text == "(" {
		close := matchingJVMDelimiter(tokens, end, "(", ")")
		if close < 0 {
			return jvmAnnotation{}, start + 1, false
		}
		arguments = parseJVMAnnotationArguments(tokens[end+1 : close])
		end = close + 1
	}

	annotationText := ""
	if text != "" {
		annotationText = sanitizedJVMSpan(text, tokens, start, end)
	}
	return jvmAnnotation{
		name:          parts[len(parts)-1],
		qualifiedName: strings.Join(parts, "."),
		arguments:     arguments,
		text:          annotationText,
		line:          tokens[start].line,
		braceDepth:    tokens[start].braceDepth,
		tokenStart:    start,
		tokenEnd:      end,
		start:         tokens[start].start,
		end:           tokens[end-1].end,
	}, end, true
}

func parseJVMAnnotationArguments(tokens []jvmToken) []jvmAnnotationArgument {
	parts := splitTopLevelJVMTokens(tokens, ",")
	arguments := make([]jvmAnnotationArgument, 0, len(parts))
	for i, part := range parts {
		if len(part) == 0 {
			if i == len(parts)-1 {
				continue
			}
			arguments = append(arguments, jvmAnnotationArgument{})
			continue
		}
		name, value := splitJVMNamedAnnotationArgument(part)
		arguments = append(arguments, jvmAnnotationArgument{name: name, tokens: value})
	}
	return arguments
}

func splitJVMNamedAnnotationArgument(tokens []jvmToken) (string, []jvmToken) {
	depth := 0
	for i, token := range tokens {
		switch token.text {
		case "{", "[", "(":
			depth++
		case "}", "]", ")":
			if depth > 0 {
				depth--
			}
		case "=":
			if depth == 0 && i == 1 && tokens[0].kind == jvmIdentifierToken {
				return tokens[0].text, tokens[i+1:]
			}
		}
	}
	return "", tokens
}

func matchingJVMDelimiter(tokens []jvmToken, start int, open, close string) int {
	depth := 0
	for i := start; i < len(tokens); i++ {
		if tokens[i].kind != jvmSymbolToken {
			continue
		}
		switch tokens[i].text {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func sanitizedJVMSpan(text string, tokens []jvmToken, start, end int) string {
	spanStart := tokens[start].start
	spanEnd := tokens[end-1].end
	out := make([]byte, spanEnd-spanStart)
	for i := spanStart; i < spanEnd; i++ {
		switch text[i] {
		case ' ', '\t', '\r', '\n':
			out[i-spanStart] = text[i]
		default:
			out[i-spanStart] = ' '
		}
	}
	for _, token := range tokens[start:end] {
		copy(out[token.start-spanStart:token.end-spanStart], text[token.start:token.end])
	}
	return string(out)
}
