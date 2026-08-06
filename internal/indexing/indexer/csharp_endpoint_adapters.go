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
	namespaces []string
	attributes []csharpAttrInfo
	classes    []csharpClassInfo
	methods    []csharpMethodInfo
}

type csharpAttrInfo struct {
	name      string
	arguments string
	line      int
	start     int
	end       int
}

type csharpClassInfo struct {
	name      string
	baseTypes []string
	start     int
	bodyStart int
	bodyEnd   int
	line      int
}

type csharpMethodInfo struct {
	name       string
	start      int
	line       int
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
	stripped := stripCSharpCommentsAndStrings(text)
	return csharpSource{
		usings:     extractCSharpUsings(stripped),
		attributes: extractCSharpAttributes(stripped),
		classes:    extractCSharpClasses(stripped),
		methods:    extractCSharpMethods(stripped),
	}
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
		if !strings.HasPrefix(trimmed, "using ") || strings.HasSuffix(trimmed, ";") != true {
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
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
			continue
		}
		inner := trimmed[1 : len(trimmed)-1]
		name := inner
		args := ""
		if idx := strings.IndexByte(inner, '('); idx >= 0 {
			name = inner[:idx]
			args = inner[idx+1:]
			if last := strings.LastIndexByte(args, ')'); last >= 0 {
				args = args[:last]
			}
		}
		attrs = append(attrs, csharpAttrInfo{
			name:      name,
			arguments: args,
			line:      i + 1,
		})
	}
	return attrs
}

func extractCSharpClasses(text string) []csharpClassInfo {
	var classes []csharpClassInfo
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		parts := strings.Fields(trimmed)
		for j, part := range parts {
			if part == "class" || part == "interface" || part == "record" {
				// Find name after keyword
				nameIdx := j + 1
				if nameIdx < len(parts) {
					name := strings.TrimRight(parts[nameIdx], "{:<>")
					// Filter base types after ':'
					baseTypes := []string{}
					if nameIdx+1 < len(parts) && parts[nameIdx+1] == ":" {
						for k := nameIdx + 2; k < len(parts); k++ {
							bt := strings.TrimRight(parts[k], "{,;")
							baseTypes = append(baseTypes, bt)
						}
					}
					classes = append(classes, csharpClassInfo{
						name:      name,
						baseTypes: baseTypes,
						line:      i + 1,
					})
				}
				break
			}
		}
	}
	return classes
}

func extractCSharpMethods(text string) []csharpMethodInfo {
	var methods []csharpMethodInfo
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Skip attributes, class/interface declarations
		if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "class ") ||
			strings.HasPrefix(trimmed, "interface ") || strings.HasPrefix(trimmed, "record ") {
			continue
		}
		// Look for method pattern: returnType MethodName(params)
		parts := strings.Fields(trimmed)
		for j, part := range parts {
			if idx := strings.IndexByte(part, '('); idx > 0 {
				// Found a potential method
				name := part[:idx]
				if isCSharpIdent(name) && j > 0 {
					methods = append(methods, csharpMethodInfo{
						name: name,
						line: i + 1,
					})
				}
			}
		}
	}
	return methods
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
		return ok && hasCSharpUsing(syntax, "Microsoft.AspNetCore.Mvc")
	},
	scan: scanASPNETController,
}

func scanASPNETController(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(csharpSource)
	if !ok {
		return nil
	}
	// Find class-level [Route] prefix
	classPrefix := ""
	for _, attr := range syntax.attributes {
		if attr.name == "Route" {
			classPrefix = routeAttributePath(attr)
		}
	}
	var candidates []endpointCandidate
	for _, attr := range syntax.attributes {
		method := httpAttrMethod(attr.name)
		if method == "" {
			continue
		}
		path, ok := attributeStringArg(attr)
		if !ok {
			continue
		}
		handler := findCSharpMethodName(syntax, attr.line)
		candidates = append(candidates, sourceEndpointCandidate(
			source, "aspnet-core",
			[]valueExpr{literalValue(method)},
			[]valueExpr{literalValue(joinPaths(classPrefix, path))},
			filepath.Base(source.rel), handler,
			attr.line, 0.85,
		))
	}
	return candidates
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

func findCSharpMethodName(source csharpSource, afterLine int) string {
	for _, m := range source.methods {
		if m.line > afterLine {
			return m.name
		}
	}
	return ""
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
