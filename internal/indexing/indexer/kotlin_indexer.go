package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanKotlinServices registers each build module containing Kotlin source so
// controllers and clients in library modules keep service ownership.
func scanKotlinServices(root string, dirs []string) []domain.ServiceRecord {
	files := walkFiles(root, dirs, hasSuffix(".kt"))
	var records []domain.ServiceRecord
	byModule := make(map[string]int)
	for _, file := range files {
		text := readFile(file)
		rel := relativeTo(root, file)
		moduleRoot := findKotlinModuleRoot(root, file)
		modulePath := inferModulePathFromRel(rel)
		serviceName := filepath.Base(modulePath)
		if moduleRoot != "" {
			if readPackaging(readFile(filepath.Join(moduleRoot, "pom.xml"))) == "pom" {
				continue
			}
			if _, err := os.Stat(filepath.Join(moduleRoot, "src", "main", "AndroidManifest.xml")); err == nil {
				continue
			}
			modulePath = relativeTo(root, moduleRoot)
			serviceName = readKotlinArtifactID(moduleRoot)
		}
		moduleKey := canonicalPath(modulePath)
		idx, exists := byModule[moduleKey]
		if !exists {
			records = append(records, domain.ServiceRecord{
				ServiceName: serviceName,
				Repo:        topSegment(rel),
				Layer:       "server",
				Scope:       inferLayer(serviceName, modulePath),
				ModulePath:  modulePath,
				Language:    "kotlin",
				Tags:        []string{"code-scan"},
				Docs:        []string{},
				Confidence:  0.7,
			})
			idx = len(records) - 1
			byModule[moduleKey] = idx
		}
		if !strings.Contains(text, "@SpringBootApplication") &&
			!strings.Contains(text, "SpringApplication.run") &&
			!strings.Contains(text, "fun main") &&
			!strings.Contains(text, "embeddedServer") {
			continue
		}
		layer := inferLayer(serviceName, modulePath)
		rec := &records[idx]
		rec.Layer = "server"
		rec.Scope = layer
		rec.Runtime = "spring-boot"
		rec.SourceOfTruth = append(rec.SourceOfTruth, rel)
		rec.Entrypoints = append(rec.Entrypoints, domain.Evidence{Path: rel, Kind: domain.SourceCodeScan})
		rec.Ports = append(rec.Ports, readKotlinPorts(moduleRoot)...)
		rec.Confidence = 0.9
	}
	return records
}

// scanKotlinEndpoints finds Spring annotation routes + Ktor DSL + Javalin routes.
func scanKotlinEndpoints(root string, dirs []string) []domain.EndpointRecord {
	files := walkFiles(root, dirs, hasSuffix(".kt"))
	var records []domain.EndpointRecord
	for _, file := range files {
		text := readFile(file)
		rel := relativeTo(root, file)
		serviceName := inferKotlinServiceName(root, file)
		handler := strings.TrimSuffix(filepath.Base(file), ".kt")
		lines := strings.Split(text, "\n")

		// Spring-style: @RestController / @Controller with @GetMapping etc.
		if strings.Contains(text, "@RestController") || strings.Contains(text, "@Controller") {
			classPrefix := extractKotlinClassPrefix(text)
			for i, line := range lines {
				method := mappingMethod(line)
				if method == "" {
					continue
				}
				if method == "ANY" && isClassLevelMapping(lines, i) {
					continue
				}
				annotation := collectAnnotation(lines, i)
				route := extractFirstString(annotation)
				conf := 0.85
				if route == "" {
					conf = 0.6
				}
				records = append(records, domain.EndpointRecord{
					ServiceName: serviceName, Repo: topSegment(rel),
					Method: method, Path: joinPaths(classPrefix, route),
					Handler: handler, HandlerMethod: kotlinMethodName(lines, i),
					File: rel, Line: i + 1, Source: domain.SourceCodeScan, Confidence: conf,
				})
			}
		}

		// Ktor DSL: routing { get("/path") { } }, route("/prefix") { get("/sub") { } }
		if strings.Contains(text, "routing") || strings.Contains(text, "route(") {
			routes := scanKtorRouting(text, handler, rel, serviceName)
			records = append(records, routes...)
		}

		// Javalin: app.get("/path") { ctx -> }, app.routes { get("/path") { } }
		if strings.Contains(text, "Javalin") || strings.Contains(text, "javalin") {
			for _, re := range javalinPatterns {
				for _, m := range re.FindAllStringSubmatch(text, -1) {
					if len(m) >= 3 {
						records = append(records, domain.EndpointRecord{
							ServiceName: serviceName, Repo: topSegment(rel),
							Method: strings.ToUpper(m[1]), Path: m[2],
							Handler: handler, File: rel, Source: domain.SourceCodeScan, Confidence: 0.8,
						})
					}
				}
			}
		}
	}
	return records
}

// scanKtorRouting extracts routes from Ktor's routing DSL.
func scanKtorRouting(text, handler, rel, serviceName string) []domain.EndpointRecord {
	var records []domain.EndpointRecord
	// Top-level routes: get("/path") { ... }, post("/path") { ... }
	for _, re := range ktorRoutePatterns {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			if len(m) >= 3 {
				records = append(records, domain.EndpointRecord{
					ServiceName: serviceName, Repo: topSegment(rel),
					Method: strings.ToUpper(m[1]), Path: m[2],
					Handler: handler, File: rel, Source: domain.SourceCodeScan, Confidence: 0.8,
				})
			}
		}
	}
	return records
}

var ktorRoutePatterns = []*regexp.Regexp{
	// get("/path") { }, post("/path") { } — inside routing { } block
	regexp.MustCompile(`\b(get|post|put|delete|patch|head|options)\s*\(\s*"([^"]+)"`),
}

var javalinPatterns = []*regexp.Regexp{
	// app.get("/path", handler) or app.routes { get("/path") { } }
	regexp.MustCompile(`app\.(get|post|put|delete|patch|head|options)\s*\(\s*"([^"]+)"`),
	// Javalin 6+: get("/path") { ctx -> } inside app.routes { }
	regexp.MustCompile(`\bApiBuilder\.(get|post|put|delete|patch)\s*\(\s*"([^"]+)"`),
}

// scanKotlinFeigns preserves Feign configuration until dependency resolution.
func scanKotlinFeigns(root string, dirs []string) []feignReference {
	files := walkFiles(root, dirs, hasSuffix(".kt"))
	var records []feignReference
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "@FeignClient") {
			continue
		}
		rel := relativeTo(root, file)
		caller := inferKotlinServiceName(root, file)
		iface := strings.TrimSuffix(filepath.Base(file), ".kt")
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			if !strings.Contains(line, "@FeignClient") {
				continue
			}
			annotation := collectAnnotation(lines, i)
			clientName := extractAnnotationValue(annotation, "value", "name")
			if clientName == "" {
				clientName = extractFirstString(annotation)
			}
			targetURL := extractAnnotationValue(annotation, "url")
			if clientName == "" && targetURL == "" {
				continue
			}
			conf := 0.9
			if caller == "unknown" {
				conf = 0.65
			}
			records = append(records, feignReference{
				From: caller, ClientName: clientName, URL: targetURL,
				Evidence: []domain.Evidence{{
					Path: rel, Line: i + 1, Symbol: iface, Kind: domain.SourceCodeScan,
				}},
				Confidence: conf,
			})
		}
	}
	return records
}

// ---- helpers ----

func findKotlinModuleRoot(root, file string) string {
	current := filepath.Dir(file)
	for strings.HasPrefix(current, root) {
		for _, marker := range []string{"build.gradle.kts", "build.gradle", "pom.xml"} {
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
	return ""
}

func readKotlinArtifactID(dir string) string {
	// Try gradle settings first
	text := readFile(filepath.Join(dir, "settings.gradle.kts"))
	if text == "" {
		text = readFile(filepath.Join(dir, "settings.gradle"))
	}
	if m := regexp.MustCompile(`rootProject\.name\s*=\s*"([^"]+)"`).FindStringSubmatch(text); m != nil {
		return m[1]
	}
	// Try pom.xml (some Kotlin projects use Maven)
	text = readFile(filepath.Join(dir, "pom.xml"))
	if text != "" {
		withoutParent := parentRe.ReplaceAllString(text, "")
		ownHeader := depSplitRe.Split(withoutParent, 2)[0]
		if m := artifactRe.FindStringSubmatch(ownHeader); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return filepath.Base(dir)
}

func inferKotlinServiceName(root, file string) string {
	moduleRoot := findKotlinModuleRoot(root, file)
	if moduleRoot != "" {
		return readKotlinArtifactID(moduleRoot)
	}
	return "unknown"
}

func extractKotlinClassPrefix(text string) string {
	// Same pattern as Java — @RequestMapping on class
	beforeClass := regexp.MustCompile(`(?:class|object)\s+`).Split(text, 2)[0]
	matches := requestMappingRe.FindAllStringSubmatch(beforeClass, -1)
	if len(matches) == 0 {
		return ""
	}
	return extractFirstString(matches[len(matches)-1][1])
}

var kotlinFuncRe = regexp.MustCompile(`fun\s+(\w+)\s*\(`)

func kotlinMethodName(lines []string, index int) string {
	end := index + 8
	if end > len(lines) {
		end = len(lines)
	}
	for i := index; i < end; i++ {
		line := lines[i]
		if strings.Contains(line, "@") && !strings.Contains(line, "(") {
			continue
		}
		if m := kotlinFuncRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

func readKotlinPorts(dir string) []int {
	if dir == "" {
		return nil
	}
	portRe := regexp.MustCompile(`port:\s*(?:\$\{port:)?(\d{3,5})`)
	candidates := []string{
		"src/main/resources/application.yml", "src/main/resources/application.yaml",
		"src/main/resources/bootstrap.yml", "src/main/resources/bootstrap.yaml",
	}
	for _, c := range candidates {
		text := readFile(filepath.Join(dir, c))
		if m := portRe.FindStringSubmatch(text); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				return []int{n}
			}
		}
	}
	return nil
}
