package indexer

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dekwanlabs/astris/internal/domain"
)

// scanNodeJSServices finds Node.js service entrypoints (package.json + main file).
func scanNodeJSServices(root string, dirs []string) []types.ServiceRecord {
	files := walkFiles(root, dirs, func(name string) bool { return name == "package.json" })
	var records []types.ServiceRecord
	for _, file := range files {
		rel := relativeTo(root, file)
		moduleRoot := filepath.Dir(file)
		modulePath := relativeTo(root, moduleRoot)
		serviceName := readNodeJSPackageName(moduleRoot)
		layer := inferLayer(serviceName, modulePath)
		records = append(records, types.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "front",
			Scope:         layer,
			ModulePath:    modulePath,
			Language:      "nodejs",
			Runtime:       "nodejs",
			Tags:          []string{"code-scan"},
			Docs:          []string{},
			SourceOfTruth: []string{rel},
			Entrypoints:   []types.Evidence{{Path: rel, Kind: types.SourceCodeScan}},
			Ports:         readNodeJSPorts(moduleRoot),
			Confidence:    0.85,
		})
	}
	return records
}

// scanNodeJSEndpoints finds Express/Fastify/Koa route registrations.
func scanNodeJSEndpoints(root string, dirs []string) []types.EndpointRecord {
	files := walkFiles(root, dirs, func(name string) bool {
		return strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".ts") ||
			strings.HasSuffix(name, ".mjs") || strings.HasSuffix(name, ".cjs")
	})
	var records []types.EndpointRecord
	for _, file := range files {
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
		for i, line := range lines {
			for _, re := range nodejsRoutePatterns {
				m := re.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				method := "ANY"
				path := ""
				if len(m) >= 3 {
					method = strings.ToUpper(m[1])
					path = m[2]
				} else if len(m) >= 2 {
					path = m[1]
				}
				if path == "" {
					continue
				}
				records = append(records, types.EndpointRecord{
					ServiceName:   serviceName,
					Repo:          topSegment(rel),
					Method:        method,
					Path:          path,
					Handler:       handler,
					HandlerMethod: nodejsHandlerName(lines, i),
					File:          rel,
					Line:          i + 1,
					Source:        types.SourceCodeScan,
					Confidence:    0.85,
				})
			}
		}
	}
	return records
}

// scanNodeJSDependencies finds HTTP client calls (axios, fetch, node-fetch).
func scanNodeJSDependencies(root string, dirs []string) []types.DependencyEdge {
	files := walkFiles(root, dirs, func(name string) bool {
		return strings.HasSuffix(name, ".js") || strings.HasSuffix(name, ".ts")
	})
	var edges []types.DependencyEdge
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "axios") && !strings.Contains(text, "fetch(") &&
			!strings.Contains(text, "request(") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findNodeJSModuleRoot(root, file)
		caller := filepath.Base(relativeTo(root, moduleRoot))
		if moduleRoot != "" {
			caller = readNodeJSPackageName(moduleRoot)
		}
		for _, m := range nodejsHTTPCallRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := strings.TrimPrefix(m[1], "http://")
				target = strings.TrimPrefix(target, "https://")
				target, _, _ = strings.Cut(target, "/")
				if target != "" && !strings.Contains(target, "localhost") && !strings.Contains(target, "127.0.0.1") {
					edges = append(edges, types.DependencyEdge{
						From:       caller,
						To:         target,
						Type:       types.EdgeHTTP,
						Evidence:   []types.Evidence{{Path: rel, Kind: types.SourceCodeScan}},
						Confidence: 0.5,
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

var nodejsRoutePatterns = []*regexp.Regexp{
	// Express: app.get('/path', handler) or router.get('/path', handler)
	regexp.MustCompile(`\.(get|post|put|delete|patch|head|options)\s*\(\s*["']([^"']+)["']`),
	// Express: app.use('/path', router)
	regexp.MustCompile(`\.use\s*\(\s*["']([^"']+)["']`),
	// Fastify/Koa: fastify.get('/path') / router.get('/path')
	regexp.MustCompile(`(?i)(?:fastify|router|server)\.(get|post|put|delete|patch|head|options)\s*\(\s*[']([^']+)[']`),
	// NestJS decorators: @Get('/path'), @Post('/path')  [TypeScript]
	regexp.MustCompile(`@(Get|Post|Put|Delete|Patch|Head|Options)\s*\(\s*[']([^']+)[']`),
	// NestJS: @Controller('prefix')  — class-level prefix
	regexp.MustCompile(`@Controller\s*\(\s*[']([^']+)[']`),
	// Hapi: server.route({ method: 'GET', path: '/path' })
	regexp.MustCompile(`method\s*:\s*['"]([A-Z]+)['"].*?path\s*:\s*['"]([^'"]+)['"]`),
	// Hapi: path first variant
	regexp.MustCompile(`path\s*:\s*['"]([^'"]+)['"].*?method\s*:\s*['"]([A-Z]+)['"]`),
	// AdonisJS: Route.get('/path', 'Controller.method') or Route.group(() => { Route.get('/path') })
	regexp.MustCompile(`Route\.(get|post|put|delete|patch)\s*\(\s*['"]([^'"]+)['"]`),
}

var nodejsFuncRe = regexp.MustCompile(`(?:async\s+)?(?:function\s+)?(\w+)\s*\(`)

func nodejsHandlerName(lines []string, index int) string {
	end := index + 4
	if end > len(lines) {
		end = len(lines)
	}
	for i := index; i < end; i++ {
		if m := nodejsFuncRe.FindStringSubmatch(lines[i]); m != nil {
			return m[1]
		}
	}
	return ""
}

var nodejsHTTPCallRe = regexp.MustCompile(`https?://([^\s"'\)]+)`)
