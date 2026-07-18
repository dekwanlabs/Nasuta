package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/platform"
)

// scanJavaServices finds Java applications by scanning .java files for
// @SpringBootApplication or a main method — no filename assumption.
func scanJavaServices(root string, dirs []string) []types.ServiceRecord {
	files := walkFiles(root, dirs, hasSuffix(".java"))
	var records []types.ServiceRecord
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "@SpringBootApplication") &&
			!strings.Contains(text, "SpringApplication.run") &&
			!strings.Contains(text, "public static void main") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findModuleRoot(root, file, "pom.xml")
		// If no pom.xml found, also try Gradle
		if moduleRoot == "" {
			moduleRoot = findModuleRoot(root, file, "build.gradle")
		}
		if moduleRoot == "" {
			moduleRoot = findModuleRoot(root, file, "build.gradle.kts")
		}
		modulePath := inferModulePathFromRel(rel)
		serviceName := filepath.Base(modulePath)
		if moduleRoot != "" {
			modulePath = relativeTo(root, moduleRoot)
			// Try Maven pom.xml first, then Gradle settings
			if serviceName = readArtifactID(filepath.Join(moduleRoot, "pom.xml")); serviceName == filepath.Base(moduleRoot) {
				serviceName = readKotlinArtifactID(moduleRoot) // handles both Kotlin & Java Gradle projects
			}
		}
		layer := inferLayer(serviceName, modulePath)
		records = append(records, types.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "server",
			Scope:         layer,
			ModulePath:    modulePath,
			Language:      "java",
			Runtime:       "spring-boot",
			Tags:          []string{"code-scan"},
			Docs:          []string{},
			SourceOfTruth: []string{rel},
			Entrypoints:   []types.Evidence{{Path: rel, Kind: types.SourceCodeScan}},
			Ports:         readPorts(moduleRoot),
			Confidence:    0.9,
		})
	}
	return records
}

// scanFeignClients finds @FeignClient declarations -> caller/target edges.
func scanFeignClients(root string, dirs []string) []types.DependencyEdge {
	files := walkFiles(root, dirs, hasSuffix(".java"))
	var records []types.DependencyEdge
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "@FeignClient") {
			continue
		}
		rel := relativeTo(root, file)
		caller := inferJavaServiceName(root, file)
		iface := strings.TrimSuffix(filepath.Base(file), ".java")
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			if !strings.Contains(line, "@FeignClient") {
				continue
			}
			annotation := collectAnnotation(lines, i)
			target := extractAnnotationValue(annotation, "value", "name")
			if target == "" {
				target = extractFirstString(annotation)
			}
			if target == "" {
				continue
			}
			conf := 0.9
			if caller == "unknown" {
				conf = 0.65
			}
			records = append(records, types.DependencyEdge{
				From: caller,
				To:   target,
				Type: types.EdgeFeign,
				Evidence: []types.Evidence{{
					Path: rel, Line: i + 1, Symbol: iface, Kind: types.SourceCodeScan,
				}},
				Confidence: conf,
			})
		}
	}
	return records
}

// scanJavaEndpoints finds Controller mapping annotations -> endpoint records.
func scanJavaEndpoints(root string, dirs []string) []types.EndpointRecord {
	files := walkFiles(root, dirs, hasSuffix(".java"))
	var records []types.EndpointRecord
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "@RestController") && !strings.Contains(text, "@Controller") {
			continue
		}
		rel := relativeTo(root, file)
		serviceName := inferJavaServiceName(root, file)
		classPrefix := extractClassRequestPrefix(text)
		handler := strings.TrimSuffix(filepath.Base(file), ".java")
		lines := strings.Split(text, "\n")
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
			records = append(records, types.EndpointRecord{
				ServiceName:   serviceName,
				Repo:          topSegment(rel),
				Method:        method,
				Path:          joinPaths(classPrefix, route),
				Handler:       handler,
				HandlerMethod: javaMethodName(lines, i), // ★ codegraph anchor
				File:          rel,
				Line:          i + 1,
				Source:        types.SourceCodeScan,
				Confidence:    conf,
			})
		}
	}
	return records
}

func findModuleRoot(root, file, marker string) string {
	current := filepath.Dir(file)
	for strings.HasPrefix(current, root) {
		if _, err := os.Stat(filepath.Join(current, marker)); err == nil {
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

var (
	parentRe   = regexp.MustCompile(`(?s)<parent>.*?</parent>`)
	artifactRe = regexp.MustCompile(`<artifactId>([^<]+)</artifactId>`)
	depSplitRe = regexp.MustCompile(`<dependencies>|<dependencyManagement>|<build>`)
	portRe     = regexp.MustCompile(`port:\s*(?:\$\{port:)?(\d{3,5})`)
)

func readArtifactID(pomPath string) string {
	text := readFile(pomPath)
	if text == "" {
		return filepath.Base(filepath.Dir(pomPath))
	}
	withoutParent := parentRe.ReplaceAllString(text, "")
	ownHeader := depSplitRe.Split(withoutParent, 2)[0]
	if m := artifactRe.FindStringSubmatch(ownHeader); m != nil {
		return strings.TrimSpace(m[1])
	}
	return filepath.Base(filepath.Dir(pomPath))
}

func inferJavaServiceName(root, file string) string {
	if appRoot := findNearestApplicationModule(root, file); appRoot != "" {
		return readArtifactID(filepath.Join(appRoot, "pom.xml"))
	}
	if moduleRoot := findModuleRoot(root, file, "pom.xml"); moduleRoot != "" {
		return readArtifactID(filepath.Join(moduleRoot, "pom.xml"))
	}
	return "unknown"
}

func findNearestApplicationModule(root, file string) string {
	current := filepath.Dir(file)
	for strings.HasPrefix(current, root) {
		if _, err := os.Stat(filepath.Join(current, "pom.xml")); err == nil {
			appDir := filepath.Join(current, "src", "main", "java")
			if hasApplicationFile(appDir) {
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

func hasApplicationFile(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), "Application.java") {
			found = true
		}
		return nil
	})
	return found
}

func readPorts(moduleRoot string) []int {
	if moduleRoot == "" {
		return []int{}
	}
	candidates := []string{
		"src/main/resources/bootstrap.yml", "src/main/resources/bootstrap.yaml",
		"src/main/resources/application.yml", "src/main/resources/application.yaml",
	}
	var ports []int
	for _, c := range candidates {
		text := readFile(filepath.Join(moduleRoot, c))
		if text == "" {
			continue
		}
		if m := portRe.FindStringSubmatch(text); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				ports = append(ports, n)
			}
		}
	}
	return platform.Dedupe(ports)
}

var knownLayers = []string{"hsmf", "hsas", "hsds", "cdp"}

func inferLayer(serviceName, modulePath string) string {
	mp := toPosix(modulePath)
	for _, layer := range knownLayers {
		if strings.HasPrefix(serviceName, layer+"-") || (mp != "" && strings.HasPrefix(mp, layer+"/")) {
			return layer
		}
	}
	return ""
}

func inferModulePathFromRel(rel string) string {
	parts := strings.Split(toPosix(rel), "/")
	if len(parts) >= 2 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

var (
	requestMappingRe = regexp.MustCompile(`@RequestMapping\(([^)]*)\)`)
	classSplitRe     = regexp.MustCompile(`public\s+class|class\s+`)
)

func extractClassRequestPrefix(text string) string {
	beforeClass := classSplitRe.Split(text, 2)[0]
	matches := requestMappingRe.FindAllStringSubmatch(beforeClass, -1)
	if len(matches) == 0 {
		return ""
	}
	return extractFirstString(matches[len(matches)-1][1])
}

// collectAnnotation accumulates lines from start until parentheses balance,
// so multi-line annotations are captured as one string.
func collectAnnotation(lines []string, start int) string {
	acc := lines[start]
	open := strings.Count(acc, "(")
	close := strings.Count(acc, ")")
	idx := start
	for open > close && idx+1 < len(lines) {
		idx++
		line := lines[idx]
		acc += line
		open += strings.Count(line, "(")
		close += strings.Count(line, ")")
	}
	return acc
}

var annotationValueCache = map[string]*regexp.Regexp{}

func extractAnnotationValue(annotation string, names ...string) string {
	for _, name := range names {
		re, ok := annotationValueCache[name]
		if !ok {
			re = regexp.MustCompile(name + `\s*=\s*"([^"]+)"`)
			annotationValueCache[name] = re
		}
		if m := re.FindStringSubmatch(annotation); m != nil {
			return m[1]
		}
	}
	return ""
}

func mappingMethod(line string) string {
	switch {
	case strings.Contains(line, "@GetMapping"):
		return "GET"
	case strings.Contains(line, "@PostMapping"):
		return "POST"
	case strings.Contains(line, "@PutMapping"):
		return "PUT"
	case strings.Contains(line, "@DeleteMapping"):
		return "DELETE"
	case strings.Contains(line, "@PatchMapping"):
		return "PATCH"
	case strings.Contains(line, "@RequestMapping"):
		return "ANY"
	}
	return ""
}

var classDeclRe = regexp.MustCompile(`\b(class|interface)\s+\w+`)

func isClassLevelMapping(lines []string, index int) bool {
	end := min(index+6, len(lines))
	return classDeclRe.MatchString(strings.Join(lines[index:end], "\n"))
}

// javaMethodName extracts the method/field name from the first declaration
// line at or after index (used as the codegraph anchor).
var javaMethodRe = regexp.MustCompile(`(?:public|protected|private)?\s*[\w<>\[\],\s\.]+?\s+(\w+)\s*\(`)

func javaMethodName(lines []string, index int) string {
	end := min(index+8, len(lines))
	for i := index; i < end; i++ {
		line := lines[i]
		if strings.Contains(line, "@") && !strings.Contains(line, "(") {
			continue
		}
		if m := javaMethodRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}
