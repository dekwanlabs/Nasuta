package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanCSharpServices registers each .NET project so controller libraries keep
// stable ownership even when they do not host the process entrypoint.
func scanCSharpServices(root string, dirs []string) []domain.ServiceRecord {
	projects := walkFiles(root, dirs, hasSuffix(".csproj"))
	var records []domain.ServiceRecord
	byModule := make(map[string]int, len(projects))
	for _, project := range projects {
		moduleRoot := filepath.Dir(project)
		modulePath := relativeTo(root, moduleRoot)
		serviceName := readCSharpProjectName(moduleRoot)
		rel := relativeTo(root, project)
		records = append(records, domain.ServiceRecord{
			ServiceName: serviceName, Repo: topSegment(rel),
			ModulePath: modulePath,
			Language:   "csharp", Tags: []string{"code-scan"}, Docs: []string{},
			SourceOfTruth: []string{rel}, Confidence: 0.7,
		})
		byModule[canonicalPath(modulePath)] = len(records) - 1
	}
	for _, file := range walkFiles(root, dirs, hasSuffix(".cs")) {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !strings.Contains(text, "WebApplication") &&
			!strings.Contains(text, "CreateHostBuilder") &&
			!strings.Contains(text, "CreateDefaultBuilder") {
			continue
		}
		moduleRoot := findCSharpModuleRoot(root, file)
		idx, ok := byModule[canonicalPath(relativeTo(root, moduleRoot))]
		if !ok {
			continue
		}
		rel := relativeTo(root, file)
		rec := &records[idx]
		rec.Runtime = "dotnet"
		rec.Entrypoints = append(rec.Entrypoints, domain.Evidence{Path: rel, Kind: domain.SourceCodeScan})
		rec.SourceOfTruth = append(rec.SourceOfTruth, rel)
		rec.Ports = append(rec.Ports, readCSharpPorts(moduleRoot)...)
		rec.Confidence = 0.85
	}
	return records
}

// scanCSharpEndpoints finds ASP.NET Core controller endpoints + ServiceStack routes.
func scanCSharpEndpoints(root string, dirs []string) []domain.EndpointRecord {
	files := walkFiles(root, dirs, hasSuffix(".cs"))
	var records []domain.EndpointRecord
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
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
	type refitMethod struct {
		name string
		line int
	}
	type refitClient struct {
		interfaceName string
		target        string
		methods       map[string]refitMethod
		path          string
	}
	var clients []refitClient
	files := walkFiles(root, dirs, hasSuffix(".cs"))
	interfaceRe := regexp.MustCompile(`(?s)\binterface\s+(\w+)\s*(?:\([^)]*\))?\s*\{(.*?)\}`)
	methodRe := regexp.MustCompile(`(?m)\[(?:Get|Post|Put|Delete|Patch|Head)\s*\([^)]*\)\][\s\r\n]*(?:public\s+)?(?:static\s+)?(?:[\w<>,.?\[\]\s]+\s+)?(\w+)\s*\(`)
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !strings.Contains(text, "interface") ||
			(!strings.Contains(text, "[Get") && !strings.Contains(text, "[Post") &&
				!strings.Contains(text, "[Put") && !strings.Contains(text, "[Delete")) {
			continue
		}
		rel := relativeTo(root, file)
		matches := interfaceRe.FindAllStringSubmatchIndex(text, -1)
		for _, match := range matches {
			if len(match) < 6 {
				continue
			}
			name := text[match[2]:match[3]]
			body := text[match[4]:match[5]]
			methods := make(map[string]refitMethod)
			for _, method := range methodRe.FindAllStringSubmatchIndex(body, -1) {
				if len(method) < 4 {
					continue
				}
				methodName := body[method[2]:method[3]]
				line := 1 + strings.Count(text[:match[4]+method[0]], "\n")
				methods[methodName] = refitMethod{name: methodName, line: line}
			}
			if len(methods) == 0 {
				continue
			}
			target := extractRefitTarget(text)
			if target == "" {
				target = refitRegistrationTarget(files, name)
			}
			if target == "" {
				// A Refit interface without a resolvable base address is still
				// a declaration candidate, but cannot become a service edge.
				continue
			}
			clients = append(clients, refitClient{
				interfaceName: name, target: target, methods: methods, path: rel,
			})
		}
	}

	var records []domain.DependencyEdge
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if strings.Contains(text, "interface") && strings.Contains(text, "[Get") {
			// Interface declarations describe capability, not usage. They are
			// inspected above but can never activate their own dependency.
			continue
		}
		rel := relativeTo(root, file)
		caller := dependencyIdentity(root, file)
		for _, client := range clients {
			if !strings.Contains(text, client.interfaceName) {
				continue
			}
			bindings := make(map[string]struct{})
			bindingRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(client.interfaceName) + `\s+(\w+)`)
			for _, match := range bindingRe.FindAllStringSubmatch(text, -1) {
				if len(match) > 1 {
					bindings[match[1]] = struct{}{}
				}
			}
			if len(bindings) == 0 {
				continue
			}
			for receiver := range bindings {
				for methodName, method := range client.methods {
					callRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(receiver) + `\s*\.\s*` + regexp.QuoteMeta(methodName) + `\s*\(`)
					for _, call := range callRe.FindAllStringIndex(text, -1) {
						line := 1 + strings.Count(text[:call[0]], "\n")
						records = append(records, domain.DependencyEdge{
							CallerServiceKey: caller.Key,
							From:             caller.Name,
							To:               client.target,
							Type:             domain.EdgeHTTP,
							Evidence: []domain.Evidence{
								{Path: rel, Line: line, Symbol: receiver + "." + methodName, Kind: domain.SourceCodeScan},
								{Path: client.path, Line: method.line, Symbol: client.interfaceName + "." + methodName, Kind: domain.SourceCodeScan},
							},
							Confidence: 0.75,
						})
					}
				}
			}
		}
	}
	return records
}

// scanCSharpDependencies finds HttpClient calls in C# code.
func scanCSharpDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, hasSuffix(".cs"))
	var edges []domain.DependencyEdge
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !strings.Contains(text, "HttpClient") && !strings.Contains(text, "RestClient") {
			continue
		}
		rel := relativeTo(root, file)
		caller := dependencyIdentity(root, file)
		for _, match := range csharpHTTPCallRe.FindAllStringSubmatchIndex(text, -1) {
			if len(match) < 4 || !httpURLUsedByClient(text, match[0], match[1], csharpClientCallRe) {
				continue
			}
			target := text[match[2]:match[3]]
			target = strings.TrimPrefix(strings.TrimPrefix(target, "http://"), "https://")
			target, _, _ = strings.Cut(target, "/")
			if skipDependencyTarget(target) {
				continue
			}
			edges = append(edges, protocolEdge(caller, target, domain.EdgeHTTP, rel, lineAt(text, match[0]), 0.5))
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
var csharpClientCallRe = regexp.MustCompile(`(?i)(?:GetAsync|PostAsync|PutAsync|DeleteAsync|SendAsync|GetStringAsync|GetByteArrayAsync)\s*\(`)

var serviceStackRouteRe = regexp.MustCompile(`\[Route\s*\(\s*"([^"]+)"\s*,\s*"([A-Za-z]+)"\s*\)\]`)

func refitRegistrationTarget(files []string, interfaceName string) string {
	pattern := regexp.MustCompile(`(?s)RestService\.For\s*<\s*` + regexp.QuoteMeta(interfaceName) + `\s*>\s*\(\s*["']https?://([^"'/]+)`)
	for _, file := range files {
		if match := pattern.FindStringSubmatch(readFile(file)); len(match) > 1 {
			return match[1]
		}
	}
	return ""
}

func extractRefitTarget(text string) string {
	// Try to find base URL from Refit attributes
	if m := regexp.MustCompile(`\[BaseAddress\s*\(\s*"(?:https?://)?([^"/]+)`).FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}
