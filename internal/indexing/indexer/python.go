package indexer

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanPythonServices finds FastAPI main.py entrypoints.
func scanPythonServices(root string, dirs []string) []domain.ServiceRecord {
	files := walkFiles(root, dirs, func(name string) bool { return name == "main.py" })
	var records []domain.ServiceRecord
	for _, file := range files {
		rel := relativeTo(root, file)
		moduleRoot := filepath.Dir(file)
		modulePath := relativeTo(root, moduleRoot)
		serviceName := readPythonAppName(moduleRoot)
		if serviceName == "" {
			serviceName = filepath.Base(moduleRoot)
		}
		layer := inferLayer(serviceName, modulePath)
		records = append(records, domain.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "server",
			Scope:         layer,
			ModulePath:    modulePath,
			Language:      "python",
			Runtime:       "fastapi",
			Tags:          []string{"code-scan", "ai"},
			Docs:          []string{},
			SourceOfTruth: []string{rel},
			Entrypoints:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
			Ports:         readPythonPorts(moduleRoot),
			Confidence:    0.85,
		})
	}
	return records
}

var pyRouteRe = regexp.MustCompile(`@router\.(get|post|put|delete|patch)\(([^)]*)\)`)

// scanPythonEndpoints finds FastAPI router routes.
func scanPythonEndpoints(root string, dirs []string) []domain.EndpointRecord {
	files := walkFiles(root, dirs, hasSuffix(".py"))
	var records []domain.EndpointRecord
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "APIRouter") && !strings.Contains(text, "@router.") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := inferPythonModuleRoot(file)
		serviceName := readPythonAppName(moduleRoot)
		if serviceName == "" {
			serviceName = filepath.Base(moduleRoot)
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

func inferPythonModuleRoot(file string) string {
	current := filepath.Dir(file)
	for filepath.Base(current) == "router" || strings.Contains(toPosix(current), "/router/") {
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return current
}

var pyRouterPrefixRe = regexp.MustCompile(`APIRouter\([^)]*prefix\s*=\s*"([^"]+)"`)

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
