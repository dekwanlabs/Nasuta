package indexer

type javaDeclarationKind uint8

const (
	javaTypeDeclaration javaDeclarationKind = iota + 1
	javaMethodDeclaration
)

type javaDeclaration struct {
	kind                      javaDeclarationKind
	name                      string
	start, bodyStart, bodyEnd int
	depth                     int
}

func javaTypeDeclarations(tokens []jvmToken) []javaDeclaration {
	var declarations []javaDeclaration
	seen := make(map[int]struct{})
	for i, token := range tokens {
		if token.kind != jvmIdentifierToken {
			continue
		}
		switch token.text {
		case "class", "interface", "enum", "record":
		default:
			continue
		}
		declaration, ok := javaTypeDeclarationAfter(tokens, i)
		if !ok {
			continue
		}
		if _, exists := seen[declaration.start]; exists {
			continue
		}
		seen[declaration.start] = struct{}{}
		declarations = append(declarations, declaration)
	}
	return declarations
}

func enclosingJavaType(declarations []javaDeclaration, offset int, depth int) *javaDeclaration {
	best := -1
	bestSpan := 0
	for i := range declarations {
		declaration := declarations[i]
		if declaration.kind != javaTypeDeclaration ||
			!javaDeclarationContains(declaration, offset) ||
			depth < declaration.depth {
			continue
		}
		span := declaration.bodyEnd - declaration.bodyStart
		if best < 0 || span < bestSpan {
			best = i
			bestSpan = span
		}
	}
	if best < 0 {
		return nil
	}
	return &declarations[best]
}

// javaDeclarationAfter finds the declaration directly annotated by an
// annotation. It deliberately stops at statement boundaries instead of
// guessing from a later method-looking line.
func javaDeclarationAfter(tokens []jvmToken, from, depth int) (javaDeclaration, bool) {
	for i := from; i < len(tokens); i++ {
		token := tokens[i]
		if token.braceDepth != depth {
			return javaDeclaration{}, false
		}
		if token.text == "@" {
			if i+1 < len(tokens) && tokens[i+1].kind == jvmIdentifierToken &&
				tokens[i+1].text == "interface" {
				return javaTypeDeclarationAfter(tokens, i+1)
			}
			_, next, ok := parseJVMAnnotation("", tokens, i)
			if ok {
				i = next - 1
				continue
			}
		}
		if token.kind == jvmSymbolToken {
			switch token.text {
			case ";", "=", "{", "}":
				return javaDeclaration{}, false
			case "(":
				if i == 0 || tokens[i-1].kind != jvmIdentifierToken ||
					isJavaControlWord(tokens[i-1].text) {
					return javaDeclaration{}, false
				}
				return javaMethodDeclarationAfter(tokens, i-1, i)
			}
			continue
		}
		if token.kind != jvmIdentifierToken {
			continue
		}
		switch token.text {
		case "class", "interface", "enum", "record":
			return javaTypeDeclarationAfter(tokens, i)
		}
	}
	return javaDeclaration{}, false
}

func javaTypeDeclarationAfter(tokens []jvmToken, keyword int) (javaDeclaration, bool) {
	name := keyword + 1
	for name < len(tokens) && tokens[name].kind != jvmIdentifierToken {
		if tokens[name].text == ";" || tokens[name].text == "{" {
			return javaDeclaration{}, false
		}
		name++
	}
	if name >= len(tokens) {
		return javaDeclaration{}, false
	}
	body := -1
	paren, bracket := 0, 0
	for i := name + 1; i < len(tokens); i++ {
		switch tokens[i].text {
		case "(":
			paren++
		case ")":
			if paren > 0 {
				paren--
			}
		case "[":
			bracket++
		case "]":
			if bracket > 0 {
				bracket--
			}
		case "{":
			if paren == 0 && bracket == 0 {
				body = i
			}
		case ";":
			if paren == 0 && bracket == 0 && body < 0 {
				return javaDeclaration{}, false
			}
		}
		if body >= 0 {
			break
		}
	}
	if body < 0 {
		return javaDeclaration{}, false
	}
	bodyEnd := javaBodyEnd(tokens, body)
	if bodyEnd < 0 {
		return javaDeclaration{}, false
	}
	return javaDeclaration{
		kind: javaTypeDeclaration, name: tokens[name].text,
		start: tokens[keyword].start, bodyStart: tokens[body].start,
		bodyEnd: bodyEnd, depth: tokens[keyword].braceDepth,
	}, true
}

func javaMethodDeclarationAfter(tokens []jvmToken, name, open int) (javaDeclaration, bool) {
	close := matchingJVMDelimiter(tokens, open, "(", ")")
	if close < 0 {
		return javaDeclaration{}, false
	}
	body := -1
	inThrows := false
	for i := close + 1; i < len(tokens); i++ {
		token := tokens[i]
		if token.text == "@" {
			if _, next, ok := parseJVMAnnotation("", tokens, i); ok {
				i = next - 1
				continue
			}
		}
		switch token.text {
		case "[", "]":
			continue
		case "throws":
			if inThrows {
				return javaDeclaration{}, false
			}
			inThrows = true
			continue
		case "{":
			body = i
		case ";":
			return javaDeclaration{
				kind: javaMethodDeclaration, name: tokens[name].text,
				start: tokens[name].start, bodyStart: -1, bodyEnd: -1,
				depth: tokens[name].braceDepth,
			}, true
		default:
			if inThrows && (token.kind == jvmIdentifierToken ||
				token.text == "." || token.text == "," || token.text == "<" ||
				token.text == ">" || token.text == "?") {
				continue
			}
			return javaDeclaration{}, false
		}
		if body >= 0 {
			return javaDeclaration{
				kind: javaMethodDeclaration, name: tokens[name].text,
				start: tokens[name].start, bodyStart: tokens[body].start,
				bodyEnd: javaBodyEnd(tokens, body), depth: tokens[name].braceDepth,
			}, true
		}
	}
	return javaDeclaration{}, false
}

func javaBodyEnd(tokens []jvmToken, open int) int {
	close := matchingJVMDelimiter(tokens, open, "{", "}")
	if close < 0 {
		return -1
	}
	return tokens[close].start
}

func isJavaControlWord(word string) bool {
	switch word {
	case "if", "for", "while", "switch", "catch", "synchronized", "new",
		"return", "throw", "assert", "do", "try":
		return true
	default:
		return false
	}
}

func javaAnnotationIsController(name string) bool {
	return name == "RestController" || name == "Controller"
}

func javaDeclarationContains(declaration javaDeclaration, offset int) bool {
	return declaration.kind == javaTypeDeclaration &&
		declaration.bodyStart >= 0 &&
		offset > declaration.bodyStart &&
		offset < declaration.bodyEnd
}

func javaDeclarationDirectlyContains(declaration javaDeclaration, offset, depth int) bool {
	return javaDeclarationContains(declaration, offset) && depth == declaration.depth+1
}
