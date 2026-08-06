package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanPythonServices registers Python project roots so routes in package
// subdirectories retain stable service ownership.
func scanPythonServices(root string, dirs []string) []domain.ServiceRecord {
	files := walkFiles(root, dirs, func(name string) bool {
		return name == "pyproject.toml" || name == "setup.py" || name == "setup.cfg" || name == "main.py"
	})
	var records []domain.ServiceRecord
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		moduleRoot := filepath.Dir(file)
		if filepath.Base(file) == "main.py" {
			if found := findPythonModuleRoot(root, file); found != "" {
				moduleRoot = found
			}
		}
		modulePath := relativeTo(root, moduleRoot)
		key := canonicalPath(modulePath)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		rel := relativeTo(root, file)
		serviceName := readPythonAppName(moduleRoot)
		if serviceName == "" {
			serviceName = readPythonProjectName(moduleRoot)
		}
		layer := inferLayer(serviceName, modulePath)
		runtime, confidence := "", 0.7
		entrypoints := []domain.Evidence{}
		sourceOfTruth := []string{rel}
		if entrypoint := findPythonEntrypoint(moduleRoot); entrypoint != "" {
			runtime = "fastapi"
			confidence = 0.85
			entryRel := relativeTo(root, entrypoint)
			entrypoints = append(entrypoints, domain.Evidence{Path: entryRel, Kind: domain.SourceCodeScan})
			sourceOfTruth = append(sourceOfTruth, entryRel)
		}
		records = append(records, domain.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "server",
			Scope:         layer,
			ModulePath:    modulePath,
			Language:      "python",
			Runtime:       runtime,
			Tags:          []string{"code-scan", "ai"},
			Docs:          []string{},
			SourceOfTruth: sourceOfTruth,
			Entrypoints:   entrypoints,
			Ports:         readPythonPorts(moduleRoot),
			Confidence:    confidence,
		})
	}
	return records
}

// ---- Python language frontend ----

type pythonSource struct {
	imports     map[string]string
	decorators  []pythonDecoratorInfo
	functions   []pythonFunctionInfo
	assignments []pythonAssignmentInfo
}

type pythonDecoratorInfo struct {
	receiver string
	name     string
	args     string
	line     int
	end      int
}

type pythonFunctionInfo struct {
	name string
	line int
}

type pythonAssignmentInfo struct {
	target string
	value  string
	args   string
	line   int
}

func parsePythonEndpointSource(root, file, text string) (endpointSource, bool) {
	source := parsePythonSource(text)
	moduleRoot := findPythonModuleRoot(root, file)
	modulePath := ""
	if moduleRoot != "" {
		modulePath = relativeTo(root, moduleRoot)
	}
	serviceName := readPythonAppName(moduleRoot)
	if serviceName == "" {
		serviceName = readPythonProjectName(moduleRoot)
	}
	return endpointSource{
		language:    "python",
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

// stripPythonCommentsAndStrings replaces Python comments and string literals
// with whitespace, preserving line counts and byte offsets. We use a marker
// approach: strings and comments are replaced but their original positions are
// preserved so that structural analysis (finding @, ., def, etc.) does not
// mistake code-like patterns inside strings for real code.

// parsePythonSource extracts imports, decorators, functions, and assignments
// from Python source text.
func parsePythonSource(original string) pythonSource {
	stripped := stripPythonCommentsAndStrings(original)
	imports := extractPythonImports(original)
	decorators := extractPythonDecorators(original, stripped)
	functions := extractPythonFunctions(original)
	assignments := extractPythonAssignments(original)
	return pythonSource{
		imports:     imports,
		decorators:  decorators,
		functions:   functions,
		assignments: assignments,
	}
}
func stripPythonCommentsAndStrings(text string) string {
	out := make([]byte, len(text))
	copy(out, text)
	i := 0
	for i < len(out) {
		switch {
		case strings.HasPrefix(text[i:], "#"):
			for i < len(out) && out[i] != '\n' {
				out[i] = ' '
				i++
			}
		case strings.HasPrefix(text[i:], `"""`):
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
		case strings.HasPrefix(text[i:], `'''`):
			i += 3
			for i+2 < len(out) {
				if text[i] == '\'' && text[i+1] == '\'' && text[i+2] == '\'' {
					out[i], out[i+1], out[i+2] = ' ', ' ', ' '
					i += 3
					break
				}
				if out[i] != '\n' {
					out[i] = ' '
				}
				i++
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

// extractPythonImports parses import and from-import statements from original.
func extractPythonImports(original string) map[string]string {
	imports := make(map[string]string)
	lines := strings.Split(original, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "import ") && !strings.HasPrefix(trimmed, "from ") {
			continue
		}
		if trimmed == "import" || trimmed == "from" {
			// Import continuing on next line
			if i+1 < len(lines) {
				cont := strings.TrimSpace(lines[i+1])
				if strings.HasPrefix(cont, "import ") {
					for _, part := range strings.Split(cont[7:], ",") {
						part = strings.TrimSpace(part)
						if alias, module, ok := strings.Cut(part, " as "); ok {
							imports[strings.TrimSpace(alias)] = strings.TrimSpace(module)
						} else if part != "" {
							imports[part] = part
						}
					}
				}
			}
			continue
		}
		if strings.HasPrefix(trimmed, "from ") {
			rest := trimmed[5:]
			if idx := strings.Index(rest, " import "); idx >= 0 {
				module := strings.TrimSpace(rest[:idx])
				names := strings.TrimSpace(rest[idx+8:])
				if names == "(" {
					// Parenthesized import — scan forward
					for j := i + 1; j < len(lines); j++ {
						names += " " + strings.TrimSpace(lines[j])
						if strings.Contains(lines[j], ")") {
							break
						}
					}
					if idx2 := strings.Index(names, ")"); idx2 >= 0 {
						names = names[:idx2]
					}
				}
				for _, part := range strings.Split(names, ",") {
					part = strings.TrimSpace(part)
					if alias, name, ok := strings.Cut(part, " as "); ok {
						imports[strings.TrimSpace(alias)] = module + "." + strings.TrimSpace(name)
					} else if part != "" {
						imports[part] = module + "." + part
					}
				}
			}
		} else {
			for _, part := range strings.Split(trimmed[7:], ",") {
				part = strings.TrimSpace(part)
				if alias, module, ok := strings.Cut(part, " as "); ok {
					imports[strings.TrimSpace(alias)] = strings.TrimSpace(module)
				} else if part != "" {
					imports[part] = part
				}
			}
		}
	}
	return imports
}

// extractPythonDecorators finds @receiver.name(args) decorators.
// It uses stripped text for structural navigation (finding @, ., identifiers) but
// switches to original text when parsing the argument list so that string literals
// remain intact.
func extractPythonDecorators(original, stripped string) []pythonDecoratorInfo {
	var decorators []pythonDecoratorInfo
	i := 0
	for i < len(stripped) {
		if stripped[i] == '\n' {
			i++
			continue
		}
		if stripped[i] != '@' {
			i++
			continue
		}
		// Read receiver: @receiver.name
		j := i + 1
		for j < len(stripped) && isPythonIdentByte(stripped[j]) {
			j++
		}
		receiver := stripped[i+1 : j]
		if receiver == "" {
			i = j
			continue
		}
		name := ""
		if j < len(stripped) && stripped[j] == '.' {
			k := j + 1
			for k < len(stripped) && isPythonIdentByte(stripped[k]) {
				k++
			}
			if k > j+1 {
				name = stripped[j+1 : k]
				j = k
			}
		}
		if name == "" {
			i = j
			continue
		}
		// Skip whitespace to find '('
		for j < len(stripped) && (stripped[j] == ' ' || stripped[j] == '\t') {
			j++
		}
		if j >= len(stripped) || stripped[j] != '(' {
			i = j
			continue
		}
		// Parse arguments from the original text so string literals are preserved.
		args, end, ok := pythonCallArgs(original, j)
		if !ok {
			i = j + 1
			continue
		}
		lineNo := strings.Count(stripped[:j+1], "\n") + 1
		decorators = append(decorators, pythonDecoratorInfo{
			receiver: receiver,
			name:     name,
			args:     args,
			line:     lineNo,
			end:      end,
		})
		i = end
	}
	return decorators
}

// extractPythonFunctions finds def declarations in original source text.
func extractPythonFunctions(original string) []pythonFunctionInfo {
	var functions []pythonFunctionInfo
	lines := strings.Split(original, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "def ") {
			if strings.HasPrefix(trimmed, "async ") && strings.Contains(trimmed, " def ") {
				trimmed = trimmed[strings.Index(trimmed, " def ")+1:]
			} else {
				continue
			}
		}
		rest := strings.TrimPrefix(trimmed, "def ")
		if idx := strings.IndexByte(rest, '('); idx > 0 {
			functions = append(functions, pythonFunctionInfo{
				name: strings.TrimSpace(rest[:idx]),
				line: i + 1,
			})
		}
	}
	return functions
}

// extractPythonAssignments finds name = Constructor(args) patterns in original
// source. It handles multi-line RHS by scanning forward for the full argument list.
func extractPythonAssignments(original string) []pythonAssignmentInfo {
	var assignments []pythonAssignmentInfo
	lines := strings.Split(original, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "=") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "@") || strings.HasPrefix(trimmed, "def ") ||
			strings.HasPrefix(trimmed, "class ") || strings.HasPrefix(trimmed, "import ") ||
			strings.HasPrefix(trimmed, "from ") {
			continue
		}
		eqIdx := strings.Index(trimmed, "=")
		target := strings.TrimSpace(trimmed[:eqIdx])
		rhs := strings.TrimSpace(trimmed[eqIdx+1:])
		if len(rhs) == 0 || !unicode.IsUpper(rune(rhs[0])) {
			continue
		}
		if !isPythonIdent(target) {
			continue
		}
		// Handle multi-line RHS: scan forward if rhs ends with "(" but has
		// no matching ")" on the same line.
		var args string
		if idx := strings.IndexByte(rhs, '('); idx >= 0 {
			// Count current line paren depth
			depth := 0
			for _, ch := range rhs[idx:] {
				switch ch {
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			if depth == 0 {
				args = rhs[idx+1 : len(rhs)-1] // already closed
			} else {
				// Multi-line — the arg text starts from rhs[idx+1:]
				argText := rhs[idx+1:]
				if !strings.HasSuffix(argText, "\n") {
					argText += "\n"
				}
				for j := i + 1; j < len(lines); j++ {
					next := lines[j]
					for _, ch := range next {
						switch ch {
						case '(':
							depth++
						case ')':
							depth--
						}
					}
					if depth == 0 {
						// Last line may have trailing ')'
						if last := strings.LastIndexByte(next, ')'); last >= 0 {
							argText += next[:last]
						} else {
							argText += next
						}
						break
					}
					argText += next + "\n"
				}
				if depth == 0 {
					args = strings.TrimSpace(argText)
				}
			}
		}
		assignments = append(assignments, pythonAssignmentInfo{
			target: target,
			value:  rhs,
			args:   args,
			line:   i + 1,
		})
	}
	return assignments
}

func isPythonIdent(s string) bool {
	if s == "" {
		return false
	}
	first, size := utf8.DecodeRuneInString(s)
	if !isPythonIdentStart(first) {
		return false
	}
	for i := size; i < len(s); {
		r, sz := utf8.DecodeRuneInString(s[i:])
		if !isPythonIdentPart(r) {
			return false
		}
		i += sz
	}
	return true
}

func isPythonIdentByte(ch byte) bool {
	return ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isPythonIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isPythonIdentPart(r rune) bool {
	return isPythonIdentStart(r) || unicode.IsDigit(r)
}

// pythonImported checks whether source imports the given module (top-level or
// submodule). Aliases are resolved so "from fastapi import APIRouter" makes
// "fastapi" visible through its alias.
func pythonImported(source pythonSource, module string) bool {
	for _, qualified := range source.imports {
		if qualified == module || strings.HasPrefix(qualified, module+".") {
			return true
		}
	}
	return false
}

// pythonReceiverPrefixes builds a map from variable name → prefix valueExpr by
// tracking constructor calls.
func pythonReceiverPrefixes(source pythonSource) map[string]valueExpr {
	prefixes := make(map[string]valueExpr, len(source.assignments))
	for _, assignment := range source.assignments {
		if constructor, _, _ := strings.Cut(assignment.value, "("); !isPythonRouterConstructor(constructor) {
			continue
		}
		if prefix, found := pythonKeywordString(assignment.args, pyPrefixArgRe); found {
			prefixes[assignment.target] = literalValue(prefix)
		} else {
			prefixes[assignment.target] = literalValue("")
		}
	}
	return prefixes
}

func isPythonRouterConstructor(name string) bool {
	switch name {
	case "FastAPI", "APIRouter", "Flask", "Blueprint":
		return true
	default:
		return false
	}
}

// pythonHandlerAfter returns the function name of the first def after the
// decorator at the given line. Returns "" if not found.
func pythonHandlerAfter(source pythonSource, decoratorLine int) string {
	best := ""
	bestDist := 0
	for _, fn := range source.functions {
		if fn.line > decoratorLine {
			dist := fn.line - decoratorLine
			if best == "" || dist < bestDist {
				best = fn.name
				bestDist = dist
			}
		}
	}
	return best
}

// ---- FastAPI adapter ----

var pythonFastAPIAdapter = endpointAdapter{
	language:  "python",
	framework: "fastapi",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(pythonSource)
		return ok && pythonImported(syntax, "fastapi")
	},
	scan: scanFastAPI,
}

func scanFastAPI(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(pythonSource)
	if !ok {
		return nil
	}
	prefixes := pythonReceiverPrefixes(syntax)
	methodDecorators := map[string]string{
		"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE",
		"patch": "PATCH", "head": "HEAD", "options": "OPTIONS",
	}
	var candidates []endpointCandidate
	for _, decorator := range syntax.decorators {
		method, ok := methodDecorators[decorator.name]
		if !ok {
			continue
		}
		prefix := prefixes[decorator.receiver]
		if prefix.kind == 0 {
			prefix = literalValue("")
		}
		route := pythonDecoratorPath(decorator.args)
		handler := pythonHandlerAfter(syntax, decorator.line)
		candidates = append(candidates, sourceEndpointCandidate(
			source, "fastapi",
			[]valueExpr{literalValue(method)},
			[]valueExpr{joinPathValues(prefix, route)},
			strings.TrimSuffix(filepath.Base(source.rel), ".py"), handler,
			decorator.line, 0.85,
		))
	}
	return candidates
}

// pythonDecoratorPath extracts the path value from a decorator's arguments.
// It handles positional strings, path= keyword args, empty strings, and
// commas separated by newlines.
func pythonDecoratorPath(args string) valueExpr {
	// First try path= keyword using regex
	if path, ok := pythonKeywordString(args, pyPathArgRe); ok {
		return literalValue(path)
	}
	// Split into positional/keyword arguments at depth 0
	parts := splitPythonArgs(args)
	if len(parts) == 0 {
		return literalValue("")
	}
	first := strings.TrimSpace(parts[0])
	// If the first arg is a keyword arg (without string content), try path= or
	// return unresolved.
	if isPythonKeyword(first) {
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if name, val, ok := strings.Cut(part, "="); ok && strings.TrimSpace(name) == "path" {
				return literalValue(extractPythonString(val))
			}
		}
		return literalValue("")
	}
	// Positional: extract the string value (empty string "" is valid).
	return literalValue(extractPythonString(first))
}

// splitPythonArgs splits a comma-separated argument list respecting nesting
// depth (parentheses, brackets, braces) and string boundaries.
func splitPythonArgs(s string) []string {
	var parts []string
	depth := 0
	inString := false
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				inString = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inString = true
			quote = c
		case '(', '{', '[':
			depth++
		case ')', '}', ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

// isPythonKeyword returns true when s looks like "name=value" (no quoted string
// in the name).
func isPythonKeyword(s string) bool {
	if idx := strings.IndexByte(s, '='); idx > 0 {
		// Check there's no string marker before the '='
		return !strings.ContainsAny(s[:idx], "\"'")
	}
	return false
}

// extractPythonStringLiteral extracts the value from a quoted Python string.
func extractPythonStringLiteral(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	quote := s[0]
	if (quote != '"' && quote != '\'') || s[len(s)-1] != quote {
		return "", false
	}
	return s[1 : len(s)-1], true
}

// extractPythonString extracts the inner value of a quoted Python string token.
func extractPythonString(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return ""
}

// ---- Flask adapter ----

var pythonFlaskAdapter = endpointAdapter{
	language:  "python",
	framework: "flask",
	applies: func(source endpointSource) bool {
		syntax, ok := source.syntax.(pythonSource)
		return ok && pythonImported(syntax, "flask")
	},
	scan: scanFlask,
}

func scanFlask(source endpointSource) []endpointCandidate {
	syntax, ok := source.syntax.(pythonSource)
	if !ok {
		return nil
	}
	prefixes := pythonReceiverPrefixes(syntax)
	methodDecorators := map[string]string{
		"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE",
		"patch": "PATCH", "head": "HEAD", "options": "OPTIONS",
	}
	var candidates []endpointCandidate
	for _, decorator := range syntax.decorators {
		prefix := prefixes[decorator.receiver]
		if prefix.kind == 0 {
			prefix = literalValue("")
		}
		var methods []valueExpr
		var route valueExpr
		if decorator.name == "route" {
			// @app.route(path, methods=["GET", "POST"])
			route = pythonDecoratorPath(decorator.args)
			if methodList, ok := pythonKeywordString(decorator.args, pyMethodsArgRe); ok {
				methods = pythonMethodListValues(methodList)
			} else {
				methods = []valueExpr{literalValue("GET")}
			}
		} else if method, ok := methodDecorators[decorator.name]; ok {
			methods = []valueExpr{literalValue(method)}
			route = pythonDecoratorPath(decorator.args)
		} else {
			continue
		}
		handler := pythonHandlerAfter(syntax, decorator.line)
		candidates = append(candidates, sourceEndpointCandidate(
			source, "flask",
			methods,
			[]valueExpr{joinPathValues(prefix, route)},
			strings.TrimSuffix(filepath.Base(source.rel), ".py"), handler,
			decorator.line, 0.8,
		))
	}
	return candidates
}

// pythonMethodListValues parses a Python list like "GET", "POST" into valueExprs.
func pythonMethodListValues(s string) []valueExpr {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	var methods []valueExpr
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if v, ok := extractPythonStringLiteral(part); ok {
			methods = append(methods, literalValue(strings.ToUpper(v)))
		} else {
			return []valueExpr{unresolvedValue(s)}
		}
	}
	if len(methods) == 0 {
		return []valueExpr{literalValue("GET")}
	}
	return methods
}

var pyMethodsArgRe = regexp.MustCompile(`\bmethods\s*=\s*(\[[^\]]*\])`)

var pyPrefixArgRe = regexp.MustCompile(`(?s)\bprefix\s*=\s*(?:"([^"]*)"|'([^']*)')`)
var pyPathArgRe = regexp.MustCompile(`(?s)\bpath\s*=\s*(?:"([^"]*)"|'([^']*)')`)
var pyURLPrefixArgRe = regexp.MustCompile(`(?s)\burl_prefix\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// pythonKeywordString extracts a string value from a keyword argument using the
// given regex. The regex must have two capturing groups for double and single quotes.
func pythonKeywordString(args string, re *regexp.Regexp) (string, bool) {
	match := re.FindStringSubmatch(args)
	if match == nil {
		return "", false
	}
	if match[1] != "" {
		return match[1], true
	}
	return match[2], true
}

// pythonCallArgs keeps decorators intact when their metadata contains nested
// calls, comments, or multiline strings.
func pythonCallArgs(text string, open int) (string, int, bool) {
	if open < 0 || open >= len(text) || text[open] != '(' {
		return "", 0, false
	}
	depth := 0
	var quote byte
	triple := false
	comment := false
	for i := open; i < len(text); i++ {
		c := text[i]
		if comment {
			if c == '\n' {
				comment = false
			}
			continue
		}
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if triple {
				if i+2 < len(text) && text[i] == quote && text[i+1] == quote && text[i+2] == quote {
					quote = 0
					triple = false
					i += 2
				}
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '#':
			comment = true
		case '\'', '"':
			quote = c
			triple = i+2 < len(text) && text[i+1] == c && text[i+2] == c
			if triple {
				i += 2
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return text[open+1 : i], i + 1, true
			}
		}
	}
	return "", 0, false
}

var (
	pyAppNameRe = regexp.MustCompile(`(?m)^APP_NAME=(.+)$`)
	pyPortRe    = regexp.MustCompile(`(?m)^UVICORN_PORT=(\d+)$`)
)

func readPythonAppName(moduleRoot string) string {
	text := readFile(filepath.Join(moduleRoot, ".env.example"))
	if m := pyAppNameRe.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func readPythonPorts(moduleRoot string) []int {
	text := readFile(filepath.Join(moduleRoot, ".env.example"))
	if m := pyPortRe.FindStringSubmatch(text); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return []int{n}
		}
	}
	return []int{}
}

func findPythonModuleRoot(root, file string) string {
	current := filepath.Dir(file)
	for strings.HasPrefix(current, root) {
		for _, marker := range []string{"pyproject.toml", "setup.py", "setup.cfg"} {
			if _, err := os.Stat(filepath.Join(current, marker)); err == nil {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	current = filepath.Dir(file)
	for {
		base := filepath.Base(current)
		if base != "router" && base != "routers" && base != "route" && base != "routes" &&
			base != "api" {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return current
}

var (
	pyProjectNameRe = regexp.MustCompile(`(?m)^name\s*=\s*["']([^"']+)["']`)
	pySetupNameRe   = regexp.MustCompile(`(?s)\bname\s*=\s*["']([^"']+)["']`)
)

func readPythonProjectName(moduleRoot string) string {
	if text := readFile(filepath.Join(moduleRoot, "pyproject.toml")); text != "" {
		if m := pyProjectNameRe.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	}
	if text := readFile(filepath.Join(moduleRoot, "setup.py")); text != "" {
		if m := pySetupNameRe.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	}
	return filepath.Base(moduleRoot)
}

func findPythonEntrypoint(moduleRoot string) string {
	for _, candidate := range []string{"main.py", "app/main.py", "src/main.py"} {
		path := filepath.Join(moduleRoot, candidate)
		text := readFile(path)
		if strings.Contains(text, "FastAPI(") || strings.Contains(text, "uvicorn.run(") {
			return path
		}
	}
	return ""
}
