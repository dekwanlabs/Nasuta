package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanCSharpServices finds .NET applications by scanning .cs files for
// WebApplication / Host builder patterns — no filename assumption.
func scanCSharpServices(root string, dirs []string) []domain.ServiceRecord {
	files := walkFiles(root, dirs, hasSuffix(".cs"))
	var records []domain.ServiceRecord
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "WebApplication") &&
			!strings.Contains(text, "CreateHostBuilder") &&
			!strings.Contains(text, "CreateDefaultBuilder") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findCSharpModuleRoot(root, file)
		modulePath := inferModulePathFromRel(rel)
		serviceName := filepath.Base(modulePath)
		if moduleRoot != "" {
			modulePath = relativeTo(root, moduleRoot)
			serviceName = readCSharpProjectName(moduleRoot)
		}
		layer := inferLayer(serviceName, modulePath)
		records = append(records, domain.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "server",
			Scope:         layer,
			ModulePath:    modulePath,
			Language:      "csharp",
			Runtime:       "dotnet",
			Tags:          []string{"code-scan"},
			Docs:          []string{},
			SourceOfTruth: []string{rel},
			Entrypoints:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
			Ports:         readCSharpPorts(moduleRoot),
			Confidence:    0.85,
		})
	}
	return records
}

// scanCSharpEndpoints finds ASP.NET Core controller endpoints + ServiceStack routes.
func scanCSharpEndpoints(root string, dirs []string) []domain.EndpointRecord {
	files := walkFiles(root, dirs, hasSuffix(".cs"))
	var records []domain.EndpointRecord
	for _, file := range files {
		base := strings.ToLower(filepath.Base(file))
		if strings.Contains(base, ".designer.") || strings.Contains(base, ".generated.") || strings.Contains(base, ".g.cs") {
			continue
		}
		text := readFile(file)
		rel := relativeTo(root, file)
		moduleRoot := findCSharpModuleRoot(root, file)
		serviceName := filepath.Base(relativeTo(root, moduleRoot))
		if moduleRoot != "" {
			serviceName = readCSharpProjectName(moduleRoot)
		}
		handler := strings.TrimSuffix(filepath.Base(file), ".cs")

		// ASP.NET Core: ControllerBase / [ApiController] / Minimal API
		if strings.Contains(text, "[ApiController]") || strings.Contains(text, "ControllerBase") ||
			strings.Contains(text, "MapGet") || strings.Contains(text, "MapPost") {
			classRoute := extractCSharpClassRoute(text)
			lines := strings.Split(text, "\n")
			for i, line := range lines {
				for _, re := range csharpEndpointPatterns {
					m := re.FindStringSubmatch(line)
					if m == nil {
						continue
					}
					records = append(records, domain.EndpointRecord{
						ServiceName: serviceName, Repo: topSegment(rel),
						Method: strings.ToUpper(m[1]), Path: joinPaths(classRoute, m[2]),
						Handler: handler, HandlerMethod: csharpMethodName(lines, i),
						File: rel, Line: i + 1, Source: domain.SourceCodeScan, Confidence: 0.85,
					})
				}
			}
			for _, re := range csharpMinimalAPIPatterns {
				for _, m := range re.FindAllStringSubmatch(text, -1) {
					if len(m) > 2 {
						records = append(records, domain.EndpointRecord{
							ServiceName: serviceName, Repo: topSegment(rel),
							Method: strings.ToUpper(m[1]), Path: m[2],
							Handler: handler, File: rel,
							Source: domain.SourceCodeScan, Confidence: 0.8,
						})
					}
				}
			}
		}

		// ServiceStack: [Route("/path", "GET")] on request DTOs
		if strings.Contains(text, "[Route(") && strings.Contains(text, "ServiceStack") || strings.Contains(text, "IReturn") {
			for _, m := range serviceStackRouteRe.FindAllStringSubmatch(text, -1) {
				if len(m) > 2 {
					records = append(records, domain.EndpointRecord{
						ServiceName: serviceName, Repo: topSegment(rel),
						Method: strings.ToUpper(m[2]), Path: m[1],
						Handler: handler, File: rel, Source: domain.SourceCodeScan, Confidence: 0.8,
					})
				}
			}
		}
	}
	return records
}

// scanCSharpRefits finds Refit interfaces (C# equivalent of @FeignClient).
func scanCSharpRefits(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, hasSuffix(".cs"))
	var records []domain.DependencyEdge
	for _, file := range files {
		text := readFile(file)
		// Refit interfaces use [Get("/path")], [Post("/path")] on interface methods
		if !strings.Contains(text, "interface") || !strings.Contains(text, "[Get") && !strings.Contains(text, "[Post") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findCSharpModuleRoot(root, file)
		caller := filepath.Base(relativeTo(root, moduleRoot))
		if moduleRoot != "" {
			caller = readCSharpProjectName(moduleRoot)
		}
		// Extract base URL from [BaseAddress("https://api.example.com")] or interface name
		target := extractRefitTarget(text)
		if target == "" {
			target = strings.TrimSuffix(filepath.Base(file), ".cs")
		}
		records = append(records, domain.DependencyEdge{
			From: caller,
			To:   target,
			Type: domain.EdgeHTTP,
			Evidence: []domain.Evidence{{
				Path: rel, Symbol: strings.TrimSuffix(filepath.Base(file), ".cs"), Kind: domain.SourceCodeScan,
			}},
			Confidence: 0.7,
		})
	}
	return records
}

// scanCSharpDependencies finds HttpClient calls in C# code.
func scanCSharpDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, hasSuffix(".cs"))
	var edges []domain.DependencyEdge
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "HttpClient") && !strings.Contains(text, "RestClient") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findCSharpModuleRoot(root, file)
		caller := filepath.Base(relativeTo(root, moduleRoot))
		if moduleRoot != "" {
			caller = readCSharpProjectName(moduleRoot)
		}
		for _, m := range csharpHTTPCallRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := strings.TrimPrefix(m[1], "http://")
				target = strings.TrimPrefix(target, "https://")
				target, _, _ = strings.Cut(target, "/")
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
	}
	return edges
}

// ---- helpers ----

func findCSharpModuleRoot(root, file string) string {
	current := filepath.Dir(file)
	for strings.HasPrefix(current, root) {
		entries, err := os.ReadDir(current)
		if err != nil {
			break
		}
		for _, e := range entries {
			name := strings.ToLower(e.Name())
			if strings.HasSuffix(name, ".csproj") || strings.HasSuffix(name, ".sln") {
				return current
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

func readCSharpProjectName(dir string) string {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".csproj") {
			continue
		}
		text := readFile(filepath.Join(dir, e.Name()))
		// Try AssemblyName first
		if m := regexp.MustCompile(`<AssemblyName>([^<]+)</AssemblyName>`).FindStringSubmatch(text); m != nil {
			return strings.TrimSpace(m[1])
		}
		// Fall back to csproj filename without extension
		return strings.TrimSuffix(e.Name(), ".csproj")
	}
	return filepath.Base(dir)
}

var csharpClassRouteRe = regexp.MustCompile(`\[Route\s*\(\s*"([^"]+)"\s*\)\]`)

func extractCSharpClassRoute(text string) string {
	if m := csharpClassRouteRe.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

var csharpEndpointPatterns = []*regexp.Regexp{
	// [HttpGet("path")] or [HttpPost("path")]
	regexp.MustCompile(`\[Http(Get|Post|Put|Delete|Patch|Head|Options)\s*\(\s*"([^"]+)"\s*\)\]`),
}

var csharpMinimalAPIPatterns = []*regexp.Regexp{
	// app.MapGet("/path", handler)
	regexp.MustCompile(`\.Map(Get|Post|Put|Delete|Patch)\s*\(\s*"([^"]+)"`),
}

var csharpMethodRe = regexp.MustCompile(`(?:public|private|protected|internal)\s+(?:static\s+)?(?:async\s+)?(?:\w+[\w<>\[\],\s]*)\s+(\w+)\s*\(`)

func csharpMethodName(lines []string, index int) string {
	end := index + 6
	if end > len(lines) {
		end = len(lines)
	}
	for i := index; i < end; i++ {
		line := lines[i]
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			continue
		}
		if m := csharpMethodRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

func readCSharpPorts(dir string) []int {
	if dir == "" {
		return nil
	}
	portRe := regexp.MustCompile(`"applicationUrl"\s*:\s*"[^"]*:(\d{3,5})"`)
	for _, cfg := range []string{
		"Properties/launchSettings.json",
		"appsettings.json", "appsettings.Development.json",
	} {
		text := readFile(filepath.Join(dir, cfg))
		for _, m := range portRe.FindAllStringSubmatch(text, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return []int{n}
			}
		}
	}
	return nil
}

var csharpHTTPCallRe = regexp.MustCompile(`https?://([^\s"'\)]+)`)

var serviceStackRouteRe = regexp.MustCompile(`\[Route\s*\(\s*"([^"]+)"\s*,\s*"([A-Za-z]+)"\s*\)\]`)

func extractRefitTarget(text string) string {
	// Try to find base URL from Refit attributes
	if m := regexp.MustCompile(`\[BaseAddress\s*\(\s*"(?:https?://)?([^"/]+)`).FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}
