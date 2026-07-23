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

var pyRouteRe = regexp.MustCompile(`@\w+\.(get|post|put|delete|patch|head|options)\(([^)]*)\)`)

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
		routerPrefix := extractPythonRouterPrefix(text)
		handler := strings.TrimSuffix(filepath.Base(file), ".py")
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			m := pyRouteRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			route := extractFirstString(m[2])
			records = append(records, domain.EndpointRecord{
				ServiceName:   serviceName,
				Repo:          topSegment(rel),
				Method:        strings.ToUpper(m[1]),
				Path:          joinPaths(routerPrefix, route),
				Handler:       handler,
				HandlerMethod: pythonHandlerName(lines, i), // ★ codegraph anchor
				File:          rel,
				Line:          i + 1,
				Source:        domain.SourceCodeScan,
				Confidence:    0.85,
			})
		}
	}
	return records
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

var pyRouterPrefixRe = regexp.MustCompile(`APIRouter\([^)]*prefix\s*=\s*["']([^"']+)["']`)

func extractPythonRouterPrefix(text string) string {
	if m := pyRouterPrefixRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

var pyDefRe = regexp.MustCompile(`(?:async\s+)?def\s+(\w+)\s*\(`)

func pythonHandlerName(lines []string, index int) string {
	end := index + 4
	if end > len(lines) {
		end = len(lines)
	}
	for i := index; i < end; i++ {
		if m := pyDefRe.FindStringSubmatch(lines[i]); m != nil {
			return m[1]
		}
	}
	return ""
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
