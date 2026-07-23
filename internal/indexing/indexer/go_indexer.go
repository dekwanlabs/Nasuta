package indexer

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanGoServices finds Go applications by scanning .go files for package main + func main().
func scanGoServices(root string, dirs []string) []domain.ServiceRecord {
	files := walkFiles(root, dirs, hasSuffix(".go"))
	var records []domain.ServiceRecord
	seenModules := map[string]struct{}{}
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "package main") || !strings.Contains(text, "func main()") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findModuleRoot(root, file, "go.mod")
		modulePath := inferModulePathFromRel(rel)
		serviceName := filepath.Base(modulePath)
		if moduleRoot != "" {
			modulePath = relativeTo(root, moduleRoot)
			serviceName = readGoModuleName(moduleRoot)
		}
		// One service per module (may have multiple main packages in cmd/ subdirs)
		moduleKey := canonicalRepo(topSegment(rel)) + "\x00" + canonicalPath(modulePath)
		if _, ok := seenModules[moduleKey]; ok {
			continue
		}
		seenModules[moduleKey] = struct{}{}
		layer := inferLayer(serviceName, modulePath)
		runtime := readGoVersion(moduleRoot)
		records = append(records, domain.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "server",
			Scope:         layer,
			ModulePath:    modulePath,
			Language:      "go",
			Runtime:       runtime,
			Tags:          []string{"code-scan"},
			Docs:          []string{},
			SourceOfTruth: []string{rel},
			Entrypoints:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
			Ports:         readGoPorts(moduleRoot),
			Confidence:    0.85,
		})
	}
	return records
}

// scanGoEndpoints finds Go HTTP route registrations (Gin, Echo, net/http, Chi, Fiber).
func scanGoEndpoints(root string, dirs []string) []domain.EndpointRecord {
	files := walkFiles(root, dirs, hasSuffix(".go"))
	var records []domain.EndpointRecord
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, ".GET") && !strings.Contains(text, ".POST") &&
			!strings.Contains(text, ".HandleFunc") && !strings.Contains(text, ".Group(") &&
			!strings.Contains(text, "Handle(") && !strings.Contains(text, ".Get(") &&
			!strings.Contains(text, ".Post(") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findModuleRoot(root, file, "go.mod")
		serviceName := filepath.Base(relativeTo(root, moduleRoot))
		if moduleRoot != "" {
			serviceName = readGoModuleName(moduleRoot)
		}
		handler := strings.TrimSuffix(filepath.Base(file), ".go")
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			for _, re := range goEndpointPatterns {
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
				records = append(records, domain.EndpointRecord{
					ServiceName:   serviceName,
					Repo:          topSegment(rel),
					Method:        method,
					Path:          path,
					Handler:       handler,
					HandlerMethod: goHandlerName(lines, i),
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

// scanGoDependencies finds HTTP client calls and gRPC client usage in Go code.
func scanGoDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, hasSuffix(".go"))
	var edges []domain.DependencyEdge
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "http.NewRequest") && !strings.Contains(text, "resty.") &&
			!strings.Contains(text, "http.Client") && !strings.Contains(text, "pb.New") &&
			!strings.Contains(text, "grpc.Dial") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findModuleRoot(root, file, "go.mod")
		caller := filepath.Base(relativeTo(root, moduleRoot))
		if moduleRoot != "" {
			caller = readGoModuleName(moduleRoot)
		}
		// HTTP URL-based dependencies
		for _, m := range goHTTPCallRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := strings.TrimPrefix(m[1], "http://")
				target = strings.TrimPrefix(target, "https://")
				target, _, _ = strings.Cut(target, "/")
				target = strings.TrimSuffix(target, ":8080")
				if target != "" && !strings.Contains(target, "localhost") && !strings.Contains(target, "127.0.0.1") {
					edges = append(edges, domain.DependencyEdge{
						From:       caller,
						To:         target,
						Type:       domain.EdgeHTTP,
						Evidence:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
						Confidence: 0.5,
					})
				}
			}
		}
		// gRPC client connections: pb.NewXxxClient(conn) or grpc.Dial("target")
		for _, m := range goGRPCClientRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := m[1]
				if target != "" && !strings.Contains(target, "localhost") && !strings.Contains(target, "127.0.0.1") {
					edges = append(edges, domain.DependencyEdge{
						From:       caller,
						To:         target,
						Type:       domain.EdgeGRPC,
						Evidence:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
						Confidence: 0.55,
					})
				}
			}
		}
	}
	return edges
}

// ---- helpers ----

func readGoModuleName(dir string) string {
	text := readFile(filepath.Join(dir, "go.mod"))
	if m := regexp.MustCompile(`(?m)^module\s+(\S+)`).FindStringSubmatch(text); m != nil {
		parts := strings.Split(m[1], "/")
		return parts[len(parts)-1]
	}
	return filepath.Base(dir)
}

func readGoVersion(dir string) string {
	text := readFile(filepath.Join(dir, "go.mod"))
	if m := regexp.MustCompile(`(?m)^go\s+([\d.]+)`).FindStringSubmatch(text); m != nil {
		return "Go " + m[1]
	}
	return "go"
}

func readGoPorts(dir string) []int {
	if dir == "" {
		return nil
	}
	portRe := regexp.MustCompile(`(?i)(?:port|PORT)[:\s=]+"?(\d{3,5})"?`)
	for _, cfg := range []string{"config.yaml", "config.yml", "config.toml", ".env.example", ".env"} {
		text := readFile(filepath.Join(dir, cfg))
		for _, m := range portRe.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return []int{n}
			}
		}
	}
	return nil
}

var goEndpointPatterns = []*regexp.Regexp{
	// Gin: router.GET("/path", ...)  — uppercase method, immediate path
	regexp.MustCompile(`(?i)\.(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s*\(\s*"([^"]+)"`),
	// net/http: mux.HandleFunc("/path", handler)
	regexp.MustCompile(`\.HandleFunc\s*\(\s*"([^"]+)"`),
	// Chi/mux r.Get("/path") — mixed case
	regexp.MustCompile(`(?i)\.(Get|Post|Put|Delete|Patch|Head|Options)\s*\(\s*"([^"]+)"`),
	// gorilla/mux: r.HandleFunc("/path", handler).Methods("GET", "POST")
	regexp.MustCompile(`\.HandleFunc\s*\(\s*"([^"]+)"`),
	// httprouter: router.GET("/path", handler)
	regexp.MustCompile(`(?i)(?:router|mux|r)\.(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s*\(\s*"([^"]+)"`),
}

// parseInts is a shared helper that converts a numeric string to a single-element int slice.
func parseInts(s string) []int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return []int{n}
	}
	return nil
}

var goHandlerRe = regexp.MustCompile(`func\s+(?:\(\w+\s+\*?\w+\)\s+)?(\w+)\s*\(`)

func goHandlerName(lines []string, index int) string {
	end := index + 4
	if end > len(lines) {
		end = len(lines)
	}
	for i := index; i < end; i++ {
		if m := goHandlerRe.FindStringSubmatch(lines[i]); m != nil {
			return m[1]
		}
	}
	return ""
}

var goHTTPCallRe = regexp.MustCompile(`https?://([^\s"'\)]+)`)

var goGRPCClientRe = regexp.MustCompile(`pb\.New(\w+)Client|grpc\.Dial\s*\(\s*"([^"]+)"`)
