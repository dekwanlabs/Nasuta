package indexer

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanAndroidServices finds Android app modules (via AndroidManifest.xml / build.gradle.kts).
func scanAndroidServices(root string, dirs []string) []domain.ServiceRecord {
	// Find AndroidManifest.xml files — the definitive Android project marker
	files := walkFiles(root, dirs, func(name string) bool { return name == "AndroidManifest.xml" })
	var records []domain.ServiceRecord
	for _, file := range files {
		rel := relativeTo(root, file)
		// AndroidManifest is typically at src/main/AndroidManifest.xml — go up to module root
		moduleRoot := filepath.Dir(filepath.Dir(filepath.Dir(file))) // src/main → module
		text := readFile(file)
		if !strings.Contains(text, "<manifest") {
			continue
		}
		// Try to get actual module root by finding build.gradle.kts
		if gr := findKotlinModuleRoot(root, file); gr != "" {
			moduleRoot = gr
		}
		modulePath := relativeTo(root, moduleRoot)
		appID := readAndroidAppID(moduleRoot)
		serviceName := appID
		if serviceName == "" {
			serviceName = filepath.Base(moduleRoot)
		}
		records = append(records, domain.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "app",
			Scope:         "mobile",
			ModulePath:    modulePath,
			Language:      readAndroidLang(moduleRoot),
			Runtime:       "android",
			Tags:          []string{"code-scan", "mobile", "android"},
			Docs:          []string{},
			SourceOfTruth: []string{rel},
			Entrypoints:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
			Ports:         nil,
			Confidence:    0.9,
		})
	}
	return records
}

// scanAndroidDependencies finds Retrofit/OkHttp API calls in Android code.
// Per architecture doc section 6.7: Retrofit's @GET/@POST must enter
// ClientCallCandidate, not masquerade as server endpoints. Unknown URLs must
// not be guessed as xxx-service.
func scanAndroidDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, func(name string) bool {
		return strings.HasSuffix(name, ".kt") || strings.HasSuffix(name, ".java")
	})

	type retrofitMethod struct {
		name string
		line int
	}
	type retrofitClient struct {
		interfaceName string
		target        string
		path          string
		methods       map[string]retrofitMethod
	}

	// An annotated Retrofit interface is only a declaration candidate. It must
	// be activated by a call on a bound instance below; the annotation itself
	// is not evidence that the application ever uses the remote service.
	var clients []retrofitClient
	interfaceRe := regexp.MustCompile(`(?s)\binterface\s+(\w+)\s*\{(.*?)\}`)
	methodRe := regexp.MustCompile(`(?m)@(?:GET|POST|PUT|DELETE|PATCH|HEAD)\s*\([^)]*\)[\s\r\n]*(?:suspend\s+)?(?:fun\s+)?(?:[\w<>,.?\[\]\s]+\s+)?(\w+)\s*\(`)
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !strings.Contains(text, "retrofit2.http") && !strings.Contains(text, "import retrofit2.") {
			continue
		}
		matches := interfaceRe.FindAllStringSubmatchIndex(text, -1)
		for _, match := range matches {
			if len(match) < 6 {
				continue
			}
			name := text[match[2]:match[3]]
			body := text[match[4]:match[5]]
			methods := make(map[string]retrofitMethod)
			for _, method := range methodRe.FindAllStringSubmatchIndex(body, -1) {
				if len(method) < 4 {
					continue
				}
				methodName := body[method[2]:method[3]]
				line := 1 + strings.Count(text[:match[4]+method[0]], "\n")
				methods[methodName] = retrofitMethod{name: methodName, line: line}
			}
			if len(methods) == 0 {
				continue
			}
			target := androidRetrofitTarget(text, body)
			clients = append(clients, retrofitClient{
				interfaceName: name,
				target:        target,
				path:          relativeTo(root, file),
				methods:       methods,
			})
		}
	}

	var edges []domain.DependencyEdge
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		rel := relativeTo(root, file)
		caller := dependencyIdentity(root, file)
		moduleRoot := findKotlinModuleRoot(root, file)
		if moduleRoot != "" && caller.Name == "unknown" {
			caller.Name = filepath.Base(moduleRoot)
		}

		for _, client := range clients {
			if !strings.Contains(text, client.interfaceName) {
				continue
			}
			bindings := androidRetrofitBindings(text, client.interfaceName)
			for receiver := range bindings {
				for methodName, method := range client.methods {
					callRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(receiver) + `\s*\.\s*` + regexp.QuoteMeta(methodName) + `\s*\(`)
					for _, call := range callRe.FindAllStringIndex(text, -1) {
						// The declaration file can contain the method signature but
						// never counts as a call; the call regex also protects against
						// merely injecting the client without invoking it.
						line := 1 + strings.Count(text[:call[0]], "\n")
						if client.target == "" || skipDependencyTarget(client.target) {
							continue
						}
						edges = append(edges, domain.DependencyEdge{
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

		// A URL literal is useful only when it is attached to an actual HTTP
		// request expression. Constants, comments, and documentation URLs do
		// not establish a dependency by themselves.
		if !strings.Contains(text, "OkHttp") && !strings.Contains(text, "okhttp") &&
			!strings.Contains(text, "URLSession") && !strings.Contains(text, "HttpURLConnection") {
			continue
		}
		for _, match := range androidURLRe.FindAllStringSubmatchIndex(text, -1) {
			if len(match) < 4 || !androidHTTPURLUsage(text, match[0], match[1]) {
				continue
			}
			target := text[match[2]:match[3]]
			target = strings.TrimPrefix(target, "http://")
			target = strings.TrimPrefix(target, "https://")
			target, _, _ = strings.Cut(target, "/")
			if skipDependencyTarget(target) {
				continue
			}
			edges = append(edges, domain.DependencyEdge{
				CallerServiceKey: caller.Key,
				From:             caller.Name,
				To:               target,
				Type:             domain.EdgeHTTP,
				Evidence:         []domain.Evidence{{Path: rel, Line: lineAt(text, match[0]), Kind: domain.SourceCodeScan}},
				Confidence:       0.6,
			})
		}
	}
	return edges
}

func androidRetrofitTarget(fileText, interfaceBody string) string {
	for _, text := range []string{interfaceBody, fileText} {
		for _, re := range retroFitPatterns {
			match := re.FindStringSubmatch(text)
			if len(match) < 2 {
				continue
			}
			path := match[len(match)-1]
			if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
				host := strings.TrimPrefix(strings.TrimPrefix(path, "http://"), "https://")
				host, _, _ = strings.Cut(host, "/")
				return host
			}
		}
	}
	// Retrofit normally keeps relative endpoint paths in the interface and
	// supplies the host through Retrofit.Builder.baseUrl(...). Resolve only a
	// literal base URL; do not invent a service name from the endpoint path.
	baseURLRe := regexp.MustCompile(`(?i)\.baseUrl\s*\(\s*["'](https?://[^"']+)["']`)
	if match := baseURLRe.FindStringSubmatch(fileText); len(match) > 1 {
		host := strings.TrimPrefix(strings.TrimPrefix(match[1], "http://"), "https://")
		host, _, _ = strings.Cut(host, "/")
		return host
	}
	return ""
}

func androidRetrofitBindings(text, interfaceName string) map[string]struct{} {
	bindings := make(map[string]struct{})
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\b` + regexp.QuoteMeta(interfaceName) + `\s+(\w+)`),
		regexp.MustCompile(`\b(\w+)\s*:\s*` + regexp.QuoteMeta(interfaceName) + `\b`),
	}
	for _, re := range patterns {
		for _, match := range re.FindAllStringSubmatch(text, -1) {
			if len(match) > 1 {
				bindings[match[1]] = struct{}{}
			}
		}
	}
	return bindings
}

func androidHTTPURLUsage(text string, start, end int) bool {
	lineStart := strings.LastIndex(text[:start], "\n") + 1
	lineEnd := strings.Index(text[end:], "\n")
	if lineEnd < 0 {
		lineEnd = len(text)
	} else {
		lineEnd += end
	}
	line := strings.TrimSpace(text[lineStart:lineEnd])
	if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "*") || strings.HasPrefix(line, "/*") {
		return false
	}
	contextStart := start - 160
	if contextStart < 0 {
		contextStart = 0
	}
	contextEnd := end + 100
	if contextEnd > len(text) {
		contextEnd = len(text)
	}
	context := text[contextStart:contextEnd]
	return regexp.MustCompile(`(?i)(?:\.url\s*\(|Request\.Builder|newCall\s*\(|openConnection\s*\(|HttpURLConnection|URL\s*\(|\.execute\s*\(|\.enqueue\s*\()`).MatchString(context)
}

// ---- Android helpers ----

func readAndroidAppID(dir string) string {
	// Try build.gradle.kts first
	text := readFile(filepath.Join(dir, "build.gradle.kts"))
	if text == "" {
		text = readFile(filepath.Join(dir, "build.gradle"))
	}
	// applicationId "com.example.app" or namespace = "com.example.app"
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`applicationId\s*["']([^"']+)["']`),
		regexp.MustCompile(`namespace\s*=\s*"([^"]+)"`),
		regexp.MustCompile(`namespace\s+['"]([^'"]+)['"]`),
	} {
		if m := re.FindStringSubmatch(text); m != nil {
			return m[1]
		}
	}
	return ""
}

func readAndroidLang(dir string) string {
	// Check whether this Android project uses Kotlin or Java
	hasKt := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || hasKt {
			return nil
		}
		if d.IsDir() {
			if ignoredDirectory(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".kt") {
			hasKt = true
		}
		return nil
	})
	if hasKt {
		return "kotlin"
	}
	return "java"
}

var retroFitPatterns = []*regexp.Regexp{
	// Retrofit: @GET("api/users"), @POST("api/orders")
	regexp.MustCompile(`@(?:GET|POST|PUT|DELETE|PATCH|HEAD)\s*\(\s*"([^"]+)"\s*\)`),
}

var androidURLRe = regexp.MustCompile(`https?://([^\s"'\)]+)`)
