package indexer

import (
	"go/ast"
	"go/parser"
	"go/token"
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
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !goHasMain(text) {
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
		runtime := readGoVersion(moduleRoot)
		records = append(records, domain.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "",
			Scope:         "",
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

// scanGoDependencies finds HTTP client calls and gRPC client usage in Go code.
func scanGoDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, hasSuffix(".go"))
	var edges []domain.DependencyEdge
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !strings.Contains(text, "http.NewRequest") && !strings.Contains(text, "resty.") &&
			!strings.Contains(text, "http.Client") && !strings.Contains(text, "pb.New") &&
			!strings.Contains(text, "grpc.Dial") {
			continue
		}
		rel := relativeTo(root, file)
		caller := dependencyIdentity(root, file)
		// HTTP URL-based dependencies
		for _, m := range goHTTPCallRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := strings.TrimPrefix(m[1], "http://")
				target = strings.TrimPrefix(target, "https://")
				target, _, _ = strings.Cut(target, "/")
				target = strings.TrimSuffix(target, ":8080")
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
		// gRPC client connections: pb.NewXxxClient(conn) or grpc.Dial("target")
		for _, m := range goGRPCClientRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := m[1]
				if target != "" && !strings.Contains(target, "localhost") && !strings.Contains(target, "127.0.0.1") {
					edges = append(edges, domain.DependencyEdge{
						CallerServiceKey: caller.Key,
						From:             caller.Name,
						To:               target,
						Type:             domain.EdgeGRPC,
						Evidence:         []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
						Confidence:       0.55,
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

func goHasMain(text string) bool {
	file, err := parser.ParseFile(token.NewFileSet(), "", text, 0)
	if err != nil || file.Name.Name != "main" {
		return false
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && function.Name.Name == "main" {
			return true
		}
	}
	return false
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
var goHTTPCallRe = regexp.MustCompile(`https?://([^\s"'\)]+)`)

var goGRPCClientRe = regexp.MustCompile(`pb\.New(\w+)Client|grpc\.Dial\s*\(\s*"([^"]+)"`)
