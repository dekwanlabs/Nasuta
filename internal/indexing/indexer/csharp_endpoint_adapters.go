package indexer

import (
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---- C# language frontend ----

type csharpSource struct {
	usings     map[string]string
	attributes []csharpAttrInfo
	classes    []csharpClassInfo
	methods    []csharpMethodInfo
}

type csharpAttrInfo struct {
	name          string
	qualifiedName string
	arguments     string
	line          int
	start         int
	end           int
}

type csharpClassInfo struct {
	name       string
	baseTypes  []string
	start      int
	bodyStart  int
	bodyEnd    int
	line       int
	depth      int
	attributes []csharpAttrInfo
}

type csharpMethodInfo struct {
	name       string
	start      int
	line       int
	depth      int
	attributes []csharpAttrInfo
}

func parseCSharpEndpointSource(root, file, text string) (endpointSource, bool) {
	source := parseCSharpSource(text)
	moduleRoot := findCSharpModuleRoot(root, file)
	modulePath := ""
	serviceName := filepath.Base(relativeTo(root, moduleRoot))
	if moduleRoot != "" {
		modulePath = relativeTo(root, moduleRoot)
		serviceName = readCSharpProjectName(moduleRoot)
	}
	return endpointSource{
		language:    "csharp",
		root:        root,
		file:        file,
		rel:         relativeTo(root, file),
		repo:        topSegment(relativeTo(root, file)),
		moduleRoot:  moduleRoot,
		modulePath:  modulePath,
		serviceName: serviceName,
		text:        text,
		syntax:      source,
	}, true
}

func parseCSharpSource(text string) csharpSource {
	commentsStripped := stripCSharpComments(text)
	declarationsStripped := stripCSharpCommentsAndStrings(text)
	attributes := extractCSharpAttributes(commentsStripped)
	classes := extractCSharpClasses(declarationsStripped)
	methods := extractCSharpMethods(declarationsStripped)
	bindCSharpAttributes(commentsStripped, attributes, classes, methods)
	return csharpSource{
		usings:     extractCSharpUsings(commentsStripped),
		attributes: attributes,
		classes:    classes,
		methods:    methods,
	}
}

// stripCSharpComments preserves byte offsets and string literal contents. The
// attribute frontend needs both: offsets for declaration binding and the
// original literal text for route values.
func stripCSharpComments(text string) string {
	out := []byte(text)
	for i := 0; i < len(text); {
		if end, ok := csharpLiteralEnd(text, i); ok {
			i = end
			continue
		}
		switch {
		case strings.HasPrefix(text[i:], "//"):
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		case strings.HasPrefix(text[i:], "/*"):
			out[i], out[i+1] = ' ', ' '
			i += 2
			for i < len(out) {
				if strings.HasPrefix(text[i:], "*/") {
					out[i], out[i+1] = ' ', ' '
					i += 2
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
			}
		default:
			i++
		}
	}
	return string(out)
}

func csharpLiteralEnd(text string, start int) (int, bool) {
	if start >= len(text) {
		return start, false
	}
	if text[start] == '\'' {
		return csharpQuotedLiteralEnd(text, start, '\'', false), true
	}

	i := start
	for i < len(text) && text[i] == '$' {
		i++
	}
	verbatim := false
	if i < len(text) && text[i] == '@' {
		verbatim = true
		i++
	}
	if i == start && text[i] == '@' {
		verbatim = true
		i++
		if i < len(text) && text[i] == '$' {
			i++
		}
	}
	if i >= len(text) || text[i] != '"' {
		return start, false
	}

	quotes := 0
	for i+quotes < len(text) && text[i+quotes] == '"' {
		quotes++
	}
	if quotes >= 3 {
		needle := strings.Repeat(`"`, quotes)
		if close := strings.Index(text[i+quotes:], needle); close >= 0 {
			return i + quotes + close + quotes, true
		}
		return len(text), true
	}
	return csharpQuotedLiteralEnd(text, i, '"', verbatim), true
}

func csharpQuotedLiteralEnd(text string, start int, quote byte, verbatim bool) int {
	for i := start + 1; i < len(text); i++ {
		if verbatim && quote == '"' && text[i] == '"' &&
			i+1 < len(text) && text[i+1] == '"' {
			i++
			continue
		}
		if !verbatim && text[i] == '\\' {
			i++
			continue
		}
		if text[i] == quote {
			return i + 1
		}
	}
	return len(text)
}

func stripCSharpCommentsAndStrings(text string) string {
	out := make([]byte, len(text))
	copy(out, text)
	i := 0
	for i < len(out) {
		switch {
		case strings.HasPrefix(text[i:], "//"):
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		case strings.HasPrefix(text[i:], "/*"):
			out[i], out[i+1] = ' ', ' '
			i += 2
			depth := 1
			for i < len(out) && depth > 0 {
				if strings.HasPrefix(text[i:], "/*") {
					depth++
					out[i], out[i+1] = ' ', ' '
					i += 2
				} else if strings.HasPrefix(text[i:], "*/") {
					depth--
					out[i], out[i+1] = ' ', ' '
					i += 2
				} else {
					if out[i] != '\n' {
						out[i] = ' '
					}
					i++
				}
			}
		case strings.HasPrefix(text[i:], `"""`):
			out[i], out[i+1], out[i+2] = ' ', ' ', ' '
			i += 3
			for i+2 < len(out) {
				if text[i] == '"' && text[i+1] == '"' && text[i+2] == '"' {
					out[i], out[i+1], out[i+2] = ' ', ' ', ' '
					i += 3
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
			}
		case strings.HasPrefix(text[i:], `$"`):
			out[i], out[i+1] = ' ', ' '
			i += 2
			depth := 1
			for i < len(out) {
				if out[i] == '\\' {
					out[i] = ' '
					i++
					if i < len(out) {
						out[i] = ' '
						i++
					}
					continue
				}
				if out[i] == '"' {
					out[i] = ' '
					i++
					depth--
					if depth == 0 {
						break
					}
					continue
				}
				if out[i] == '{' {
					depth++
				} else if out[i] == '}' {
					depth--
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
			}
		case strings.HasPrefix(text[i:], `@"`):
			out[i], out[i+1] = ' ', ' '
			i += 2
			for i < len(out) {
				if text[i] == '"' && i+1 < len(out) && text[i+1] == '"' {
					out[i], out[i+1] = ' ', ' '
					i += 2
				} else if out[i] == '"' {
					out[i] = ' '
					i++
					break
				} else {
					if out[i] != '\n' {
						out[i] = ' '
					}
					i++
				}
			}
		case out[i] == '"':
			out[i] = ' '
			i++
			for i < len(out) {
				if out[i] == '\\' {
					out[i] = ' '
					i++
					if i < len(out) {
						out[i] = ' '
						i++
					}
					continue
				}
				if out[i] == '"' {
					out[i] = ' '
					i++
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
			}
		case out[i] == '\'':
			out[i] = ' '
			i++
			for i < len(out) {
				if out[i] == '\\' {
					out[i] = ' '
					i++
					if i < len(out) {
						out[i] = ' '
						i++
					}
					continue
				}
				if out[i] == '\'' {
					out[i] = ' '
					i++
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
			}
		default:
			i++
		}
	}
	return string(out)
}

func extractCSharpUsings(text string) map[string]string {
	usings := make(map[string]string)
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "global ")
		if !strings.HasPrefix(trimmed, "using ") || !strings.HasSuffix(trimmed, ";") {
			continue
		}
		// "using Microsoft.AspNetCore.Mvc;"
		rest := strings.TrimPrefix(trimmed, "using ")
		rest = strings.TrimSuffix(rest, ";")
		rest = strings.TrimSpace(rest)
		// Handle "using X = Y;"
		if before, after, ok := strings.Cut(rest, "="); ok {
			usings[strings.TrimSpace(before)] = strings.TrimSpace(strings.TrimSuffix(after, ";"))
		} else {
			// Store namespace and Alias mapping
			parts := strings.Split(rest, ".")
			if len(parts) > 0 {
				alias := parts[len(parts)-1]
				usings[alias] = rest
			}
			usings[rest] = rest
		}
	}
	return usings
}

// hasCSharpUsing checks if any using directive contains the given namespace prefix.
func hasCSharpUsing(source csharpSource, namespacePrefix string) bool {
	for _, qualified := range source.usings {
		if strings.HasPrefix(qualified, namespacePrefix) || qualified == namespacePrefix {
			return true
		}
	}
	return false
}

func extractCSharpAttributes(text string) []csharpAttrInfo {
	var attrs []csharpAttrInfo
	for i := 0; i < len(text); {
		if end, ok := csharpLiteralEnd(text, i); ok {
			i = end
			continue
		}
		if text[i] != '[' || !csharpAttributeStart(text, i) {
			i++
			continue
		}
		close := matchingCSharpBracket(text, i)
		if close < 0 {
			i++
			continue
		}
		line := 1 + strings.Count(text[:i], "\n")
		for _, part := range splitTopLevelCSharp(text[i+1:close], ',') {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if colon := topLevelCSharpIndex(part, ':'); colon >= 0 {
				target := strings.TrimSpace(part[:colon])
				switch target {
				case "assembly", "module", "field", "event", "method",
					"param", "property", "return", "type":
					part = strings.TrimSpace(part[colon+1:])
				}
			}
			name := part
			arguments := ""
			if open := topLevelCSharpIndex(part, '('); open >= 0 {
				closeParen := matchingCSharpDelimiter(part, open, '(', ')')
				if closeParen < 0 {
					continue
				}
				name = strings.TrimSpace(part[:open])
				arguments = part[open+1 : closeParen]
			}
			qualified := strings.TrimSpace(strings.TrimPrefix(name, "global::"))
			simple := qualified
			if dot := strings.LastIndexByte(simple, '.'); dot >= 0 {
				simple = simple[dot+1:]
			}
			simple = strings.TrimSuffix(simple, "Attribute")
			if !isCSharpIdent(simple) {
				continue
			}
			attrs = append(attrs, csharpAttrInfo{
				name:          simple,
				qualifiedName: strings.TrimSuffix(qualified, "Attribute"),
				arguments:     arguments,
				line:          line,
				start:         i,
				end:           close + 1,
			})
		}
		i = close + 1
	}
	return attrs
}

func csharpAttributeStart(text string, index int) bool {
	lineStart := strings.LastIndexByte(text[:index], '\n') + 1
	prefix := strings.TrimSpace(text[lineStart:index])
	return prefix == "" || strings.HasSuffix(prefix, "]")
}

func matchingCSharpBracket(text string, start int) int {
	depth := 0
	for i := start; i < len(text); {
		if end, ok := csharpLiteralEnd(text, i); ok {
			i = end
			continue
		}
		switch text[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

func splitTopLevelCSharp(text string, delimiter byte) []string {
	var parts []string
	start := 0
	paren, bracket, brace := 0, 0, 0
	for i := 0; i < len(text); {
		if end, ok := csharpLiteralEnd(text, i); ok {
			i = end
			continue
		}
		switch text[i] {
		case '(':
			paren++
		case ')':
			if paren > 0 {
				paren--
			}
		case '[':
			bracket++
		case ']':
			if bracket > 0 {
				bracket--
			}
		case '{':
			brace++
		case '}':
			if brace > 0 {
				brace--
			}
		default:
			if text[i] == delimiter && paren == 0 && bracket == 0 && brace == 0 {
				parts = append(parts, text[start:i])
				start = i + 1
			}
		}
		i++
	}
	return append(parts, text[start:])
}

func topLevelCSharpIndex(text string, target byte) int {
	paren, bracket, brace := 0, 0, 0
	for i := 0; i < len(text); {
		if end, ok := csharpLiteralEnd(text, i); ok {
			i = end
			continue
		}
		if text[i] == target && paren == 0 && bracket == 0 && brace == 0 {
			return i
		}
		switch text[i] {
		case '(':
			paren++
		case ')':
			if paren > 0 {
				paren--
			}
		case '[':
			bracket++
		case ']':
			if bracket > 0 {
				bracket--
			}
		case '{':
			brace++
		case '}':
			if brace > 0 {
				brace--
			}
		}
		i++
	}
	return -1
}

func matchingCSharpDelimiter(text string, start int, open, close byte) int {
	depth := 0
	for i := start; i < len(text); {
		if end, ok := csharpLiteralEnd(text, i); ok {
			i = end
			continue
		}
		switch text[i] {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i
			}
		}
		i++
	}
	return -1
}

func extractCSharpClasses(text string) []csharpClassInfo {
	var classes []csharpClassInfo
	lines := strings.Split(text, "\n")
	offset := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		parts := strings.Fields(trimmed)
		for j, part := range parts {
			if part == "class" || part == "interface" || part == "record" {
				// Find name after keyword
				nameIdx := j + 1
				if nameIdx < len(parts) {
					name := csharpDeclaredName(parts[nameIdx])
					if !isCSharpIdent(name) {
						break
					}
					// Filter base types after ':'
					baseTypes := []string{}
					if colon := strings.IndexByte(trimmed, ':'); colon >= 0 {
						bases := trimmed[colon+1:]
						if open := strings.IndexByte(bases, '{'); open >= 0 {
							bases = bases[:open]
						}
						for _, base := range strings.Split(bases, ",") {
							if base = strings.TrimSpace(base); base != "" {
								baseTypes = append(baseTypes, base)
							}
						}
					}
					lineStart := offset + firstNonSpaceByte(line)
					keyword := offset + strings.Index(line, part)
					bodyStart := strings.IndexByte(text[keyword:], '{')
					if bodyStart < 0 {
						break
					}
					bodyStart += keyword
					bodyEnd := matchingCSharpDelimiter(text, bodyStart, '{', '}')
					if bodyEnd < 0 {
						break
					}
					classes = append(classes, csharpClassInfo{
						name:      name,
						baseTypes: baseTypes,
						start:     lineStart,
						bodyStart: bodyStart,
						bodyEnd:   bodyEnd,
						line:      i + 1,
						depth:     csharpBraceDepth(text, lineStart),
					})
				}
				break
			}
		}
		offset += len(line) + 1
	}
	return classes
}

func csharpDeclaredName(text string) string {
	for i, r := range text {
		if r == '<' || r == '(' || r == ':' || r == '{' {
			return text[:i]
		}
	}
	return strings.TrimRight(text, "{:;,")
}

func extractCSharpMethods(text string) []csharpMethodInfo {
	var methods []csharpMethodInfo
	lines := strings.Split(text, "\n")
	offset := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Skip attributes, class/interface declarations
		if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "class ") ||
			strings.HasPrefix(trimmed, "interface ") || strings.HasPrefix(trimmed, "record ") {
			offset += len(line) + 1
			continue
		}
		name, ok := csharpMethodNameOnLine(line)
		if ok {
			start := offset + firstNonSpaceByte(line)
			methods = append(methods, csharpMethodInfo{
				name:  name,
				start: start,
				line:  i + 1,
				depth: csharpBraceDepth(text, start),
			})
		}
		offset += len(line) + 1
	}
	return methods
}

func csharpMethodNameOnLine(line string) (string, bool) {
	for open := strings.IndexByte(line, '('); open >= 0; {
		end := open
		for end > 0 && (line[end-1] == ' ' || line[end-1] == '\t') {
			end--
		}
		start := end
		for start > 0 {
			r, size := utf8.DecodeLastRuneInString(line[:start])
			if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				break
			}
			start -= size
		}
		name := line[start:end]
		prefix := strings.TrimSpace(line[:start])
		if isCSharpIdent(name) && prefix != "" &&
			!strings.HasSuffix(prefix, ".") && !strings.Contains(prefix, "=") &&
			!isCSharpControlWord(name) {
			return name, true
		}
		next := strings.IndexByte(line[open+1:], '(')
		if next < 0 {
			break
		}
		open += next + 1
	}
	return "", false
}

func isCSharpControlWord(word string) bool {
	switch word {
	case "if", "for", "foreach", "while", "switch", "catch", "lock",
		"using", "nameof", "typeof", "sizeof", "new", "return", "throw":
		return true
	default:
		return false
	}
}

func firstNonSpaceByte(text string) int {
	for i, r := range text {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return len(text)
}

func csharpBraceDepth(text string, end int) int {
	depth := 0
	if end > len(text) {
		end = len(text)
	}
	for i := 0; i < end; i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth
}

func bindCSharpAttributes(
	text string,
	attributes []csharpAttrInfo,
	classes []csharpClassInfo,
	methods []csharpMethodInfo,
) {
	for i := range classes {
		classes[i].attributes = csharpAttributesBefore(text, attributes, classes[i].start)
	}
	for i := range methods {
		methods[i].attributes = csharpAttributesBefore(text, attributes, methods[i].start)
	}
}

func csharpAttributesBefore(
	text string,
	attributes []csharpAttrInfo,
	declarationStart int,
) []csharpAttrInfo {
	cursor := declarationStart
	var reversed []csharpAttrInfo
	for i := len(attributes) - 1; i >= 0; {
		for i >= 0 && attributes[i].end > cursor {
			i--
		}
		if i < 0 {
			break
		}
		groupStart, groupEnd := attributes[i].start, attributes[i].end
		first := i
		for first > 0 &&
			attributes[first-1].start == groupStart &&
			attributes[first-1].end == groupEnd {
			first--
		}
		if strings.TrimSpace(text[groupEnd:cursor]) != "" {
			break
		}
		for j := i; j >= first; j-- {
			reversed = append(reversed, attributes[j])
		}
		cursor = groupStart
		i = first - 1
	}
	bound := make([]csharpAttrInfo, len(reversed))
	for i := range reversed {
		bound[len(reversed)-1-i] = reversed[i]
	}
	return bound
}

func isCSharpIdent(s string) bool {
	if s == "" {
		return false
	}
	first, size := utf8.DecodeRuneInString(s)
	if first != '_' && !unicode.IsLetter(first) {
		return false
	}
	for i := size; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		if r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
		i += sz
	}
	return true
}

// attributeStringArg extracts a double-quoted string value from an attribute argument.
func attributeStringArg(attr csharpAttrInfo) (string, bool) {
	args := strings.TrimSpace(attr.arguments)
	if args == "" {
		return "", true // empty positional arg
	}
	// Look for first quoted string
	for i := 0; i < len(args); i++ {
		if args[i] == '"' {
			end := strings.IndexByte(args[i+1:], '"')
			if end >= 0 {
				return args[i+1 : i+1+end], true
			}
			return "", false
		}
		if args[i] == ',' {
			// First positional arg is empty or non-string
			return args[:i], false
		}
	}
	return args, false
}

// routeAttributePath extracts the path from [Route("path")] or [Route("path", "METHOD")].
func routeAttributePath(attr csharpAttrInfo) string {
	path, _ := attributeStringArg(attr)
	return path
}

// routeAttributeMethods extracts methods from [Route("path", "GET")] or returns empty.
func routeAttributeMethod(attr csharpAttrInfo) string {
	args := strings.TrimSpace(attr.arguments)
	// Find second arg after first comma outside strings
	depth := 0
	inString := false
	for i := 0; i < len(args); i++ {
		c := args[i]
		if c == '"' {
			inString = !inString
		} else if !inString {
			switch c {
			case '(':
				depth++
			case ')':
				depth--
			case ',':
				if depth == 0 {
					second := strings.TrimSpace(args[i+1:])
					if len(second) >= 2 && second[0] == '"' {
						end := strings.IndexByte(second[1:], '"')
						if end >= 0 {
							return second[1 : 1+end]
						}
					}
					return ""
				}
			}
		}
	}
	return ""
}

// ---- ASP.NET Core Controller adapter ----

var csharpASPNETControllerAdapter = endpointAdapter{
	language:  "csharp",
	framework: "aspnet-core-controller",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(csharpSource)
		if !ok {
			return false
		}
		if hasCSharpUsing(syntax, "Microsoft.AspNetCore.Mvc") {
			return true
		}
		for _, attr := range syntax.attributes {
			if strings.HasPrefix(attr.qualifiedName, "Microsoft.AspNetCore.Mvc.") {
				return true
			}
		}
		return false
	},
	scan: scanASPNETController,
}

func scanASPNETController(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(csharpSource)
	if !ok {
		return nil
	}
	var candidates []endpointCandidate
	for _, class := range syntax.classes {
		if !isCSharpControllerClass(syntax, class) {
			continue
		}
		prefix := literalValue("")
		classRoutes := csharpAttributesNamed(syntax, class.attributes, "Route")
		switch len(classRoutes) {
		case 0:
		case 1:
			path, resolved := attributeStringArg(classRoutes[0])
			if !resolved {
				prefix = unresolvedValue(classRoutes[0].arguments)
			} else {
				prefix = literalValue(path)
			}
		default:
			prefix = unresolvedValue("multiple class Route attributes")
		}

		for _, handler := range syntax.methods {
			if !csharpClassDirectlyContains(class, handler.start, handler.depth) {
				continue
			}
			methodRoutes := csharpAttributesNamed(syntax, handler.attributes, "Route")
			for _, attr := range handler.attributes {
				if !isCSharpMVCAttribute(syntax, attr) {
					continue
				}
				method := httpAttrMethod(attr.name)
				if method == "" {
					continue
				}
				path, resolved := attributeStringArg(attr)
				if !resolved {
					candidates = append(candidates, sourceEndpointCandidate(
						source, "aspnet-core",
						[]valueExpr{literalValue(method)},
						[]valueExpr{unresolvedValue(attr.arguments)},
						class.name, handler.name,
						attr.line, 0.85,
					))
					continue
				}
				if path == "" && len(methodRoutes) == 1 {
					path, resolved = attributeStringArg(methodRoutes[0])
				}
				route := unresolvedValue(attr.arguments)
				if resolved {
					route = joinPathValues(prefix, literalValue(path))
				}
				candidates = append(candidates, sourceEndpointCandidate(
					source, "aspnet-core",
					[]valueExpr{literalValue(method)},
					[]valueExpr{route},
					class.name, handler.name,
					attr.line, 0.85,
				))
			}
		}
	}
	return candidates
}

func isCSharpControllerClass(source csharpSource, class csharpClassInfo) bool {
	for _, attr := range class.attributes {
		if isCSharpMVCAttribute(source, attr) &&
			(attr.name == "ApiController" || attr.name == "Controller") {
			return true
		}
	}
	for _, base := range class.baseTypes {
		switch csharpSimpleTypeName(base) {
		case "Controller", "ControllerBase":
			return true
		}
	}
	return false
}

func csharpSimpleTypeName(name string) string {
	name = strings.TrimSpace(strings.TrimPrefix(name, "global::"))
	if generic := strings.IndexByte(name, '<'); generic >= 0 {
		name = name[:generic]
	}
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		name = name[dot+1:]
	}
	return strings.TrimSpace(name)
}

func csharpClassDirectlyContains(class csharpClassInfo, offset, depth int) bool {
	return class.bodyStart >= 0 && class.bodyEnd > class.bodyStart &&
		offset > class.bodyStart && offset < class.bodyEnd &&
		depth == class.depth+1
}

func csharpAttributesNamed(
	source csharpSource,
	attributes []csharpAttrInfo,
	name string,
) []csharpAttrInfo {
	var matches []csharpAttrInfo
	for _, attr := range attributes {
		if attr.name == name && isCSharpMVCAttribute(source, attr) {
			matches = append(matches, attr)
		}
	}
	return matches
}

func isCSharpMVCAttribute(source csharpSource, attr csharpAttrInfo) bool {
	if strings.HasPrefix(attr.qualifiedName, "Microsoft.AspNetCore.Mvc.") {
		return true
	}
	if qualified := source.usings[attr.qualifiedName]; strings.HasPrefix(
		qualified, "Microsoft.AspNetCore.Mvc.",
	) {
		return true
	}
	return hasCSharpUsing(source, "Microsoft.AspNetCore.Mvc")
}

func httpAttrMethod(name string) string {
	switch name {
	case "HttpGet":
		return "GET"
	case "HttpPost":
		return "POST"
	case "HttpPut":
		return "PUT"
	case "HttpDelete":
		return "DELETE"
	case "HttpPatch":
		return "PATCH"
	case "HttpHead":
		return "HEAD"
	case "HttpOptions":
		return "OPTIONS"
	default:
		return ""
	}
}

// ---- ASP.NET Core Minimal API adapter ----

var csharpMinimalAPIAdapter = endpointAdapter{
	language:  "csharp",
	framework: "aspnet-core-minimal",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(csharpSource)
		if !ok {
			return false
		}
		// Detect via usings or via MapGet/MapPost method presence in text
		return hasCSharpUsing(syntax, "Microsoft.AspNetCore.Builder") ||
			hasCSharpUsing(syntax, "Microsoft.AspNetCore")
	},
	scan: scanCSharpMinimalAPI,
}

func scanCSharpMinimalAPI(source endpointSource) []endpointCandidate {
	// Minimal API uses app.MapGet/MapPost patterns. These are detected by scanning
	// the raw text for the method patterns, combined with using evidence from applies.
	text := source.text
	var candidates []endpointCandidate
	for _, pattern := range csharpMinimalAPIPatterns {
		for _, m := range pattern.FindAllStringSubmatch(text, -1) {
			if len(m) > 2 {
				candidates = append(candidates, sourceEndpointCandidate(
					source, "aspnet-minimal",
					[]valueExpr{literalValue(strings.ToUpper(m[1]))},
					[]valueExpr{literalValue(m[2])},
					filepath.Base(source.rel), "",
					0, 0.75,
				))
			}
		}
	}
	return candidates
}

// ---- ServiceStack adapter ----

var csharpServiceStackAdapter = endpointAdapter{
	language:  "csharp",
	framework: "servicestack",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(csharpSource)
		if !ok {
			return false
		}
		return hasCSharpUsing(syntax, "ServiceStack")
	},
	scan: scanCSharpServiceStack,
}

func scanCSharpServiceStack(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(csharpSource)
	if !ok {
		return nil
	}
	var candidates []endpointCandidate
	for _, attr := range syntax.attributes {
		if attr.name != "Route" {
			continue
		}
		method := routeAttributeMethod(attr)
		if method == "" {
			method = "ANY"
		}
		path := routeAttributePath(attr)
		if path == "" {
			continue
		}
		candidates = append(candidates, sourceEndpointCandidate(
			source, "servicestack",
			[]valueExpr{literalValue(strings.ToUpper(method))},
			[]valueExpr{literalValue(path)},
			filepath.Base(source.rel), "",
			attr.line, 0.8,
		))
	}
	return candidates
}
