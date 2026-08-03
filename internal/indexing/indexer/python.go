package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

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

type pythonRouteDecorator struct {
	receiver string
	method   string
	args     string
	handler  string
	line     int
	end      int
}

var (
	pyRouteDecoratorRe = regexp.MustCompile(`(?m)^[ \t]*@([A-Za-z_]\w*)\.(get|post|put|delete|patch|head|options)\s*\(`)
	pyRouterAssignRe   = regexp.MustCompile(`(?m)^[ \t]*([A-Za-z_]\w*)\s*=\s*APIRouter\s*\(`)
	pyPrefixArgRe      = regexp.MustCompile(`(?s)\bprefix\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	pyPathArgRe        = regexp.MustCompile(`(?s)\bpath\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	pyDefRe            = regexp.MustCompile(`(?m)^[ \t]*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
)

// scanPythonEndpoints finds FastAPI router routes.
func scanPythonEndpoints(root string, dirs []string) []domain.EndpointRecord {
	files := walkFiles(root, dirs, hasSuffix(".py"))
	var records []domain.EndpointRecord
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "APIRouter") && !strings.Contains(text, "@router.") &&
			!strings.Contains(text, "@app.") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findPythonModuleRoot(root, file)
		serviceName := readPythonAppName(moduleRoot)
		if serviceName == "" {
			serviceName = readPythonProjectName(moduleRoot)
		}
		routerPrefixes := pythonRouterPrefixes(text)
		handler := strings.TrimSuffix(filepath.Base(file), ".py")
		for _, route := range scanPythonRouteDecorators(text) {
			records = append(records, domain.EndpointRecord{
				ServiceName:   serviceName,
				Repo:          topSegment(rel),
				Method:        strings.ToUpper(route.method),
				Path:          joinPaths(routerPrefixes[route.receiver], pythonRoutePath(route.args)),
				Handler:       handler,
				HandlerMethod: route.handler,
				File:          rel,
				Line:          route.line,
				Source:        domain.SourceCodeScan,
				Confidence:    0.85,
			})
		}
	}
	return records
}

func scanPythonRouteDecorators(text string) []pythonRouteDecorator {
	matches := pyRouteDecoratorRe.FindAllStringSubmatchIndex(text, -1)
	routes := make([]pythonRouteDecorator, 0, len(matches))
	line, lineStart, consumedUntil := 1, 0, 0
	for _, match := range matches {
		start := match[0]
		line += strings.Count(text[lineStart:start], "\n")
		lineStart = start
		if start < consumedUntil {
			continue
		}
		args, end, ok := pythonCallArgs(text, match[1]-1)
		if !ok {
			continue
		}
		consumedUntil = end
		routes = append(routes, pythonRouteDecorator{
			receiver: text[match[2]:match[3]],
			method:   text[match[4]:match[5]],
			args:     args,
			line:     line,
			end:      end,
		})
	}

	definitions := pyDefRe.FindAllStringSubmatchIndex(text, -1)
	definitionIndex := 0
	for i := range routes {
		for definitionIndex < len(definitions) && definitions[definitionIndex][0] < routes[i].end {
			definitionIndex++
		}
		if definitionIndex < len(definitions) {
			match := definitions[definitionIndex]
			routes[i].handler = text[match[2]:match[3]]
		}
	}
	return routes
}

func pythonRouterPrefixes(text string) map[string]string {
	matches := pyRouterAssignRe.FindAllStringSubmatchIndex(text, -1)
	prefixes := make(map[string]string, len(matches))
	consumedUntil := 0
	for _, match := range matches {
		if match[0] < consumedUntil {
			continue
		}
		args, end, ok := pythonCallArgs(text, match[1]-1)
		if !ok {
			continue
		}
		consumedUntil = end
		if prefix, ok := pythonKeywordString(args, pyPrefixArgRe); ok {
			prefixes[text[match[2]:match[3]]] = prefix
		}
	}
	return prefixes
}

func pythonRoutePath(args string) string {
	if path, ok := pythonKeywordString(args, pyPathArgRe); ok {
		return path
	}
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || (trimmed[0] != '"' && trimmed[0] != '\'') {
		return ""
	}
	return extractFirstString(trimmed)
}

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
