package indexer

import (
	"strconv"
	"strings"
)

// strictJavaMappingPaths only evaluates syntax whose value is local and
// unambiguous. A symbolic constant or concatenation is deliberately left
// unresolved; inventing a route from one of its other string arguments is
// worse than omitting that candidate.
func strictJavaMappingPaths(annotation jvmAnnotation) ([]string, bool) {
	if argument, ok := jvmNamedAnnotationArgument(annotation, "path"); ok {
		return strictJavaPathValues(argument.tokens)
	}
	if argument, ok := jvmNamedAnnotationArgument(annotation, "value"); ok {
		return strictJavaPathValues(argument.tokens)
	}
	for _, argument := range annotation.arguments {
		if argument.name == "" {
			return strictJavaPathValues(argument.tokens)
		}
	}
	return []string{""}, true
}

func strictJavaMappingMethods(annotation jvmAnnotation) ([]string, bool) {
	switch annotation.name {
	case "GetMapping":
		return []string{"GET"}, true
	case "PostMapping":
		return []string{"POST"}, true
	case "PutMapping":
		return []string{"PUT"}, true
	case "DeleteMapping":
		return []string{"DELETE"}, true
	case "PatchMapping":
		return []string{"PATCH"}, true
	}
	argument, ok := jvmNamedAnnotationArgument(annotation, "method")
	if !ok {
		return []string{"ANY"}, true
	}
	items := unwrapJVMContainer(argument.tokens)
	if len(items) == 0 {
		return nil, false
	}
	if items[0].text == "{" {
		if items[len(items)-1].text != "}" {
			return nil, false
		}
		items = items[1 : len(items)-1]
	}
	parts := splitTopLevelJVMTokens(items, ",")
	methods := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for i, part := range parts {
		if len(part) == 0 && i == len(parts)-1 {
			continue
		}
		part = unwrapJVMContainer(part)
		identifiers := make([]string, 0, 3)
		for _, token := range part {
			if token.kind == jvmIdentifierToken {
				identifiers = append(identifiers, token.text)
				continue
			}
			if token.kind != jvmSymbolToken || token.text != "." {
				return nil, false
			}
		}
		if len(identifiers) < 2 || identifiers[len(identifiers)-2] != "RequestMethod" {
			return nil, false
		}
		method := identifiers[len(identifiers)-1]
		switch method {
		case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS":
		default:
			return nil, false
		}
		if _, exists := seen[method]; exists {
			continue
		}
		seen[method] = struct{}{}
		methods = append(methods, method)
	}
	if len(methods) == 0 {
		return nil, false
	}
	return methods, true
}

func jvmNamedAnnotationArgument(annotation jvmAnnotation, name string) (jvmAnnotationArgument, bool) {
	for _, argument := range annotation.arguments {
		if strings.TrimSpace(argument.name) == name {
			return argument, true
		}
	}
	return jvmAnnotationArgument{}, false
}

func strictJVMStringValues(tokens []jvmToken) ([]string, bool) {
	tokens = unwrapJVMContainer(tokens)
	if len(tokens) == 0 {
		return nil, false
	}
	if tokens[0].text == "{" {
		if tokens[len(tokens)-1].text != "}" {
			return nil, false
		}
		inner := tokens[1 : len(tokens)-1]
		if len(inner) == 0 {
			return []string{}, true
		}
		var values []string
		parts := splitTopLevelJVMTokens(inner, ",")
		for i, part := range parts {
			if len(part) == 0 && i == len(parts)-1 {
				continue
			}
			part = unwrapJVMContainer(part)
			if len(part) != 1 || part[0].kind != jvmStringToken {
				return nil, false
			}
			value, ok := decodeJVMString(part[0].text)
			if !ok {
				return nil, false
			}
			values = append(values, value)
		}
		return values, true
	}
	if len(tokens) != 1 || tokens[0].kind != jvmStringToken {
		return nil, false
	}
	value, ok := decodeJVMString(tokens[0].text)
	if !ok {
		return nil, false
	}
	return []string{value}, true
}

func unwrapJVMContainer(tokens []jvmToken) []jvmToken {
	for len(tokens) >= 2 && tokens[0].text == "(" {
		close := matchingJVMDelimiter(tokens, 0, "(", ")")
		if close != len(tokens)-1 {
			break
		}
		tokens = tokens[1 : len(tokens)-1]
	}
	return tokens
}

func splitTopLevelJVMTokens(tokens []jvmToken, delimiter string) [][]jvmToken {
	if len(tokens) == 0 {
		return nil
	}
	parts := make([][]jvmToken, 0, 2)
	start, depth := 0, 0
	for i, token := range tokens {
		switch token.text {
		case "{", "[", "(":
			depth++
		case "}", "]", ")":
			if depth > 0 {
				depth--
			}
		}
		if token.text == delimiter && depth == 0 {
			parts = append(parts, tokens[start:i])
			start = i + 1
		}
	}
	parts = append(parts, tokens[start:])
	return parts
}

func strictJavaPathValues(tokens []jvmToken) ([]string, bool) {
	values, ok := strictJVMStringValues(tokens)
	if ok && len(values) == 0 {
		return []string{""}, true
	}
	return values, ok
}

func decodeJVMString(raw string) (string, bool) {
	if strings.HasPrefix(raw, `"""`) || len(raw) < 2 {
		return "", false
	}
	value, err := strconv.Unquote(raw)
	return value, err == nil
}

func isJavaMappingAnnotation(name string) bool {
	switch name {
	case "RequestMapping", "GetMapping", "PostMapping", "PutMapping", "DeleteMapping", "PatchMapping":
		return true
	default:
		return false
	}
}
