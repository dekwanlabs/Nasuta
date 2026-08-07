package indexer

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanNodeJSServices finds Node.js service entrypoints (package.json + main file).
func scanNodeJSServices(root string, dirs []string) []domain.ServiceRecord {
	files := walkFiles(root, dirs, func(name string) bool { return name == "package.json" })
	var records []domain.ServiceRecord
	for _, file := range files {
		rel := relativeTo(root, file)
		if isTestSourcePath(rel) {
			continue
		}
		moduleRoot := filepath.Dir(file)
		modulePath := relativeTo(root, moduleRoot)
		serviceName := readNodeJSPackageName(moduleRoot)
		runtime := ""
		confidence := 0.7
		entrypoints := []domain.Evidence(nil)
		if nodePackageHasRuntimeEvidence(moduleRoot) {
			runtime = "nodejs"
			confidence = 0.85
			entrypoints = []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}}
		}
		records = append(records, domain.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "",
			Scope:         "",
			ModulePath:    modulePath,
			Language:      "nodejs",
			Runtime:       runtime,
			Tags:          []string{"code-scan"},
			Docs:          []string{},
			SourceOfTruth: []string{rel},
			Entrypoints:   entrypoints,
			Ports:         readNodeJSPorts(moduleRoot),
			Confidence:    confidence,
		})
	}
	return records
}

// scanNodeJSEndpoints finds Express/Fastify/Koa route registrations.
func scanNodeJSEndpoints(root string, dirs []string) []domain.EndpointRecord {
	files := walkFiles(root, dirs, func(name string) bool {
		return strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".ts") ||
			strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".cjs")
	})
	var records []domain.EndpointRecord
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !strings.Contains(text, ".get(") && !strings.Contains(text, ".post(") &&
			!strings.Contains(text, "router.") && !strings.Contains(text, "Router(") &&
			!strings.Contains(text, "@Get(") && !strings.Contains(text, "@Post(") &&
			!strings.Contains(text, "@Controller(") && !strings.Contains(text, "server.route(") &&
			!strings.Contains(text, "Route.get(") && !strings.Contains(text, "Route.post(") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findNodeJSModuleRoot(root, file)
		serviceName := filepath.Base(relativeTo(root, moduleRoot))
		if moduleRoot != "" {
			serviceName = readNodeJSPackageName(moduleRoot)
		}
		handler := strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		lines := strings.Split(text, "\n")
		controllerPrefix := extractNestControllerPrefix(text)
		for i, line := range lines {
			for _, route := range parseNodeJSRoutes(line, controllerPrefix) {
				records = append(records, domain.EndpointRecord{
					ServiceName:   serviceName,
					Repo:          topSegment(rel),
					Method:        route.method,
					Path:          route.path,
					Handler:       handler,
					HandlerMethod: nodejsHandlerName(lines, i),
					File:          rel,
					Line:          i + 1,
					Source:        domain.SourceCodeScan,
					Confidence:    0.85,
				})
			}
		}
	}
	return records
}

// scanNodeJSDependencies finds HTTP client calls (axios, fetch, node-fetch).
func scanNodeJSDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, func(name string) bool {
		return strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".ts") ||
			strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".cjs")
	})
	var edges []domain.DependencyEdge
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !strings.Contains(text, "axios") && !strings.Contains(text, "fetch(") &&
			!strings.Contains(text, "request(") {
			continue
		}
		rel := relativeTo(root, file)
		caller := dependencyIdentity(root, file)
		for _, m := range nodejsHTTPCallRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := strings.TrimPrefix(m[1], "http://")
				target = strings.TrimPrefix(target, "https://")
				target, _, _ = strings.Cut(target, "/")
				if target != "" && !strings.Contains(target, "localhost") && !strings.Contains(target, "127.0.0.1") {
					edges = append(edges, domain.DependencyEdge{
						CallerServiceKey: caller.Key,
						From:             caller.Name,
						To:               target,
						Type:             domain.EdgeHTTP,
						Evidence:         []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
						Confidence:       0.5,
					})
				}
			}
		}
	}
	return edges
}

// ---- helpers ----

func readNodeJSPackageName(dir string) string {
	text := readFile(filepath.Join(dir, "package.json"))
	if text == "" {
		return filepath.Base(dir)
	}
	var pkg struct{ Name string }
	if err := json.Unmarshal([]byte(text), &pkg); err == nil && pkg.Name != "" {
		// Strip scope prefix e.g. @company/name → name
		if idx := strings.LastIndex(pkg.Name, "/"); idx >= 0 {
			return pkg.Name[idx+1:]
		}
		return pkg.Name
	}
	return filepath.Base(dir)
}

func nodePackageHasRuntimeEvidence(dir string) bool {
	text := readFile(filepath.Join(dir, "package.json"))
	if text == "" {
		return false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal([]byte(text), &pkg); err != nil {
		return false
	}
	return strings.TrimSpace(pkg.Scripts["start"]) != ""
}

func findNodeJSModuleRoot(root, file string) string {
	current := filepath.Dir(file)
	for strings.HasPrefix(current, root) {
		if readFile(filepath.Join(current, "package.json")) != "" {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

func readNodeJSPorts(dir string) []int {
	if dir == "" {
		return nil
	}
	// Check .env, .env.example, and source files for PORT=
	portEnvRe := regexp.MustCompile(`(?i)PORT\s*=\s*(\d{3,5})`)
	for _, cfg := range []string{".env", ".env.example", ".env.local"} {
		text := readFile(filepath.Join(dir, cfg))
		if m := portEnvRe.FindStringSubmatch(text); m != nil {
			return parseInts(m[1])
		}
	}
	// Also check package.json scripts for --port flags
	text := readFile(filepath.Join(dir, "package.json"))
	for _, m := range regexp.MustCompile(`--port[=\s]+(\d{3,5})`).FindAllStringSubmatch(text, -1) {
		return parseInts(m[1])
	}
	return nil
}

type nodeJSRoute struct {
	method string
	path   string
}

var (
	nodeMethodRouteRe = regexp.MustCompile(`(?i)\.(get|post|put|delete|patch|head|options)\s*\(\s*["']([^"']+)["']`)
	nestRouteRe       = regexp.MustCompile(`@(Get|Post|Put|Delete|Patch|Head|Options)\s*\(\s*(?:["']([^"']*)["'])?\s*\)`)
	nestControllerRe  = regexp.MustCompile(`@Controller\s*\(\s*(?:["']([^"']*)["'])?\s*\)`)
	hapiMethodFirstRe = regexp.MustCompile(`(?i)method\s*:\s*['"]([A-Z]+)['"].*?path\s*:\s*['"]([^'"]+)['"]`)
	hapiPathFirstRe   = regexp.MustCompile(`(?i)path\s*:\s*['"]([^'"]+)['"].*?method\s*:\s*['"]([A-Z]+)['"]`)
)

func parseNodeJSRoutes(line, controllerPrefix string) []nodeJSRoute {
	if m := nestRouteRe.FindStringSubmatch(line); m != nil {
		return []nodeJSRoute{{method: strings.ToUpper(m[1]), path: joinPaths(controllerPrefix, m[2])}}
	}
	if m := hapiMethodFirstRe.FindStringSubmatch(line); m != nil {
		return []nodeJSRoute{{method: strings.ToUpper(m[1]), path: m[2]}}
	}
	if m := hapiPathFirstRe.FindStringSubmatch(line); m != nil {
		return []nodeJSRoute{{method: strings.ToUpper(m[2]), path: m[1]}}
	}
	if m := nodeMethodRouteRe.FindStringSubmatch(line); m != nil {
		return []nodeJSRoute{{method: strings.ToUpper(m[1]), path: m[2]}}
	}
	return nil
}

func extractNestControllerPrefix(text string) string {
	beforeClass := regexp.MustCompile(`(?:export\s+)?class\s+`).Split(text, 2)[0]
	matches := nestControllerRe.FindAllStringSubmatch(beforeClass, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

var nodejsFuncRe = regexp.MustCompile(`(?:async\s+)?(?:function\s+)?(\w+)\s*\(`)

func nodejsHandlerName(lines []string, index int) string {
	end := index + 4
	if end > len(lines) {
		end = len(lines)
	}
	for i := index; i < end; i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "@") {
			continue
		}
		if m := nodejsFuncRe.FindStringSubmatch(lines[i]); m != nil {
			return m[1]
		}
	}
	return ""
}

var nodejsHTTPCallRe = regexp.MustCompile(`https?://([^\s"'\)]+)`)
