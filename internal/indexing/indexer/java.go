package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/platform"
)

// scanJavaServices registers every Maven module under root as a service. A
// module is a directory with pom.xml, excluding <packaging>pom</packaging>
// aggregators. Modules hosting a Spring Boot entrypoint (@SpringBootApplication
// or a main method) are decorated with runtime=spring-boot plus entrypoint and
// port evidence; library modules keep runtime="" so their controllers and Feign
// clients resolve to them instead of being dropped during canonicalization.
func scanJavaServices(root string, dirs []string) []domain.ServiceRecord {
	type module struct {
		rec domain.ServiceRecord
		dir string
	}
	var modules []module
	byDir := make(map[string]int)
	for _, pom := range walkFiles(root, dirs, isPom) {
		text := readFile(pom)
		if readPackaging(text) == "pom" {
			continue
		}
		dir := filepath.Dir(pom)
		name := readArtifactIDFromText(text, pom)
		modulePath := relativeTo(root, dir)
		modules = append(modules, module{
			rec: domain.ServiceRecord{
				ServiceName: name,
				Repo:        topSegment(relativeTo(root, pom)),
				Layer:       "server",
				Scope:       inferLayer(name, modulePath),
				ModulePath:  modulePath,
				Language:    "java",
				Tags:        []string{"code-scan"},
				Confidence:  0.7,
			},
			dir: dir,
		})
		byDir[dir] = len(modules) - 1
	}

	for _, file := range walkFiles(root, dirs, hasSuffix(".java")) {
		text := readFile(file)
		if !strings.Contains(text, "@SpringBootApplication") &&
			!strings.Contains(text, "SpringApplication.run") &&
			!strings.Contains(text, "public static void main") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findModuleRoot(root, file, "pom.xml")
		if moduleRoot == "" {
			moduleRoot = findModuleRoot(root, file, "build.gradle")
		}
		if moduleRoot == "" {
			moduleRoot = findModuleRoot(root, file, "build.gradle.kts")
		}
		if idx, ok := byDir[moduleRoot]; ok {
			rec := &modules[idx].rec
			rec.Runtime = "spring-boot"
			rec.Entrypoints = append(rec.Entrypoints, domain.Evidence{Path: rel, Kind: domain.SourceCodeScan})
			rec.Ports = append(rec.Ports, readPorts(moduleRoot)...)
			rec.SourceOfTruth = append(rec.SourceOfTruth, rel)
			if rec.Confidence < 0.9 {
				rec.Confidence = 0.9
			}
			continue
		}
		// Entrypoint outside any pom module (Gradle or bare checkout): register
		// it directly, mirroring the legacy entrypoint-only behavior.
		modules = append(modules, module{rec: entrypointService(root, rel, moduleRoot), dir: moduleRoot})
	}

	out := make([]domain.ServiceRecord, 0, len(modules))
	for _, m := range modules {
		out = append(out, m.rec)
	}
	return out
}

// entrypointService builds a record for a Spring Boot entrypoint that has no
// pom.xml module (e.g. a Gradle project or a bare checkout).
func entrypointService(root, rel, moduleRoot string) domain.ServiceRecord {
	modulePath := inferModulePathFromRel(rel)
	serviceName := filepath.Base(modulePath)
	if moduleRoot != "" {
		modulePath = relativeTo(root, moduleRoot)
		if serviceName = readArtifactID(filepath.Join(moduleRoot, "pom.xml")); serviceName == filepath.Base(moduleRoot) {
			serviceName = readKotlinArtifactID(moduleRoot)
		}
	}
	return domain.ServiceRecord{
		ServiceName:   serviceName,
		Repo:          topSegment(rel),
		Layer:         "server",
		Scope:         inferLayer(serviceName, modulePath),
		ModulePath:    modulePath,
		Language:      "java",
		Runtime:       "spring-boot",
		Tags:          []string{"code-scan"},
		Docs:          []string{},
		SourceOfTruth: []string{rel},
		Entrypoints:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
		Ports:         readPorts(moduleRoot),
		Confidence:    0.9,
	}
}

// scanFeignClients preserves Feign configuration until dependency resolution.
func scanFeignClients(root string, dirs []string) []feignReference {
	files := walkFiles(root, dirs, hasSuffix(".java"))
	var records []feignReference
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "@FeignClient") {
			continue
		}
		text = stripJVMComments(text)
		rel := relativeTo(root, file)
		caller := inferJavaServiceName(root, file)
		modulePath := ""
		if moduleRoot := findModuleRoot(root, file, "pom.xml"); moduleRoot != "" {
			modulePath = relativeTo(root, moduleRoot)
		}
		iface := strings.TrimSuffix(filepath.Base(file), ".java")
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
				From: caller, ModulePath: modulePath, ClientName: clientName, URL: targetURL,
				Evidence: []domain.Evidence{{
					Path: rel, Line: i + 1, Symbol: iface, Kind: domain.SourceCodeScan,
				}},
				Confidence: conf,
			})
		}
	}
	return records
}

// scanJavaEndpoints finds Controller mapping annotations -> endpoint records.
func scanJavaEndpoints(root string, dirs []string) []domain.EndpointRecord {
	files := walkFiles(root, dirs, hasSuffix(".java"))
	var records []domain.EndpointRecord
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "@RestController") && !strings.Contains(text, "@Controller") {
			continue
		}
		rel := relativeTo(root, file)
		serviceName := inferJavaServiceName(root, file)
		handler := strings.TrimSuffix(filepath.Base(file), ".java")
		lines := strings.Split(text, "\n")
		classLine := javaClassLine(lines)
		classPrefixes := extractClassRequestPrefixes(lines, classLine)
		for i, line := range lines {
			annotationName := mappingAnnotationName(line)
			if annotationName == "" || i < classLine {
				continue
			}
			annotation := collectAnnotation(lines, i)
			methods := javaMappingMethods(annotationName, annotation)
			routes := javaMappingPaths(annotation)
			handlerMethod := javaMethodName(lines, i)
			for _, classPrefix := range classPrefixes {
				for _, route := range routes {
					conf := 0.85
					if route == "" {
						conf = 0.6
					}
					for _, method := range methods {
						records = append(records, domain.EndpointRecord{
							ServiceName:   serviceName,
							Repo:          topSegment(rel),
							Method:        method,
							Path:          joinPaths(classPrefix, route),
							Handler:       handler,
							HandlerMethod: handlerMethod,
							File:          rel,
							Line:          i + 1,
							Source:        domain.SourceCodeScan,
							Confidence:    conf,
						})
					}
				}
			}
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
	parentRe    = regexp.MustCompile(`(?s)<parent>.*?</parent>`)
	artifactRe  = regexp.MustCompile(`<artifactId>([^<]+)</artifactId>`)
	depSplitRe  = regexp.MustCompile(`<dependencies>|<dependencyManagement>|<build>`)
	portRe      = regexp.MustCompile(`port:\s*(?:\$\{port:)?(\d{3,5})`)
	packagingRe = regexp.MustCompile(`<packaging>\s*([\w-]+)\s*</packaging>`)
)

func isPom(name string) bool { return name == "pom.xml" }

// readArtifactID extracts the own artifactId from a pom.xml on disk.
func readArtifactID(pomPath string) string {
	return readArtifactIDFromText(readFile(pomPath), pomPath)
}

// readArtifactIDFromText extracts the own artifactId from already-read pom text,
// avoiding a second read when the caller already has the text.
func readArtifactIDFromText(text, pomPath string) string {
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

// readPackaging returns the module's own packaging (default jar). Aggregator
// parents declare <packaging>pom</packaging> and are skipped by scanJavaServices.
func readPackaging(text string) string {
	withoutParent := parentRe.ReplaceAllString(text, "")
	ownHeader := depSplitRe.Split(withoutParent, 2)[0]
	if m := packagingRe.FindStringSubmatch(ownHeader); m != nil {
		return strings.TrimSpace(m[1])
	}
	return "jar"
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
		if d.IsDir() {
			if ignoredDirectory(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "Application.java") {
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

func javaClassLine(lines []string) int {
	for i, line := range lines {
		if classDeclRe.MatchString(line) {
			return i
		}
	}
	return len(lines)
}

func extractClassRequestPrefixes(lines []string, classLine int) []string {
	prefixes := []string{""}
	for i := 0; i < classLine; i++ {
		if mappingAnnotationName(lines[i]) != "RequestMapping" {
			continue
		}
		prefixes = javaMappingPaths(collectAnnotation(lines, i))
	}
	return prefixes
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

func mappingAnnotationName(line string) string {
	switch {
	case strings.Contains(line, "@GetMapping"):
		return "GetMapping"
	case strings.Contains(line, "@PostMapping"):
		return "PostMapping"
	case strings.Contains(line, "@PutMapping"):
		return "PutMapping"
	case strings.Contains(line, "@DeleteMapping"):
		return "DeleteMapping"
	case strings.Contains(line, "@PatchMapping"):
		return "PatchMapping"
	case strings.Contains(line, "@RequestMapping"):
		return "RequestMapping"
	}
	return ""
}

func mappingMethod(line string) string {
	name := mappingAnnotationName(line)
	if name == "" {
		return ""
	}
	return javaMappingMethods(name, line)[0]
}

func isClassLevelMapping(lines []string, index int) bool {
	end := min(index+6, len(lines))
	return classDeclRe.MatchString(strings.Join(lines[index:end], "\n"))
}

var (
	classDeclRe          = regexp.MustCompile(`\b(class|interface)\s+\w+`)
	requestMappingRe     = regexp.MustCompile(`@RequestMapping\(([^)]*)\)`)
	javaRequestMethodRe  = regexp.MustCompile(`RequestMethod\.(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)`)
	javaQuotedStringRe   = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)
	javaAnnotationArgsRe = regexp.MustCompile(`(?s)^[^(]*\((.*)\)\s*$`)
)

func javaMappingMethods(name, annotation string) []string {
	switch name {
	case "GetMapping":
		return []string{"GET"}
	case "PostMapping":
		return []string{"POST"}
	case "PutMapping":
		return []string{"PUT"}
	case "DeleteMapping":
		return []string{"DELETE"}
	case "PatchMapping":
		return []string{"PATCH"}
	}
	methodArg, ok := javaNamedArgument(annotation, "method")
	if !ok {
		return []string{"ANY"}
	}
	matches := javaRequestMethodRe.FindAllStringSubmatch(methodArg, -1)
	if len(matches) == 0 {
		return []string{"ANY"}
	}
	methods := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if _, ok := seen[match[1]]; ok {
			continue
		}
		seen[match[1]] = struct{}{}
		methods = append(methods, match[1])
	}
	return methods
}

func javaMappingPaths(annotation string) []string {
	if path, ok := javaNamedArgument(annotation, "path"); ok {
		return javaStringValues(path)
	}
	if value, ok := javaNamedArgument(annotation, "value"); ok {
		return javaStringValues(value)
	}
	args := javaAnnotationArguments(annotation)
	if len(args) == 0 {
		return []string{""}
	}
	first := strings.TrimSpace(args[0])
	for len(first) >= 2 && first[0] == '(' && first[len(first)-1] == ')' {
		first = strings.TrimSpace(first[1 : len(first)-1])
	}
	if first == "" || (first[0] != '"' && first[0] != '{') {
		return []string{""}
	}
	return javaStringValues(first)
}

func javaNamedArgument(annotation, name string) (string, bool) {
	for _, arg := range javaAnnotationArguments(annotation) {
		left, right, ok := strings.Cut(arg, "=")
		if ok && strings.TrimSpace(left) == name {
			return strings.TrimSpace(right), true
		}
	}
	return "", false
}

func javaAnnotationArguments(annotation string) []string {
	match := javaAnnotationArgsRe.FindStringSubmatch(strings.TrimSpace(annotation))
	if match == nil {
		return nil
	}
	args := strings.TrimSpace(match[1])
	if args == "" {
		return nil
	}
	parts := make([]string, 0, 3)
	start, depth := 0, 0
	inString, escaped := false, false
	for i := 0; i < len(args); i++ {
		switch {
		case escaped:
			escaped = false
		case inString && args[i] == '\\':
			escaped = true
		case args[i] == '"':
			inString = !inString
		case inString:
		case args[i] == '{' || args[i] == '(' || args[i] == '[':
			depth++
		case args[i] == '}' || args[i] == ')' || args[i] == ']':
			depth--
		case args[i] == ',' && depth == 0:
			parts = append(parts, strings.TrimSpace(args[start:i]))
			start = i + 1
		}
	}
	parts = append(parts, strings.TrimSpace(args[start:]))
	return parts
}

func javaStringValues(expression string) []string {
	matches := javaQuotedStringRe.FindAllString(expression, -1)
	if len(matches) == 0 {
		return []string{""}
	}
	values := make([]string, 0, len(matches))
	for _, match := range matches {
		value, err := strconv.Unquote(match)
		if err != nil {
			continue
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return []string{""}
	}
	return values
}

// javaMethodName extracts the method/field name from the first declaration
// line at or after index (used as the codegraph anchor).
var javaMethodRe = regexp.MustCompile(`(?:public|protected|private)?\s*[\w<>\[\],\s\.]+?\s+(\w+)\s*\(`)

func javaMethodName(lines []string, index int) string {
	for i := index; i < len(lines); i++ {
		if i > index && mappingAnnotationName(lines[i]) != "" {
			return ""
		}
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
