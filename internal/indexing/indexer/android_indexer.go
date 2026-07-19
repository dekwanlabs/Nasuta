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
func scanAndroidDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, func(name string) bool {
		return strings.HasSuffix(name, ".kt") || strings.HasSuffix(name, ".java")
	})
	var edges []domain.DependencyEdge
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "@GET") && !strings.Contains(text, "@POST") &&
			!strings.Contains(text, "Retrofit") && !strings.Contains(text, "retrofit") &&
			!strings.Contains(text, "OkHttp") && !strings.Contains(text, "okhttp") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findKotlinModuleRoot(root, file)
		caller := filepath.Base(relativeTo(root, moduleRoot))
		if moduleRoot != "" {
			if id := readAndroidAppID(moduleRoot); id != "" {
				caller = id
			}
		}

		// Retrofit interfaces: @GET("api/users") / @POST("api/orders")
		for _, re := range retroFitPatterns {
			for _, m := range re.FindAllStringSubmatch(text, -1) {
				if len(m) >= 2 {
					target := extractServiceFromPath(m[len(m)-1])
					if target != "" {
						edges = append(edges, domain.DependencyEdge{
							From:       caller,
							To:         target,
							Type:       domain.EdgeHTTP,
							Evidence:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
							Confidence: 0.65,
						})
					}
				}
			}
		}
		// OkHttp direct URLs: "https://api.example.com/..." / BASE_URL = "https://..."
		for _, m := range androidURLRe.FindAllStringSubmatch(text, -1) {
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
		if err != nil || hasKt || d.IsDir() {
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
	// Retrofit with base path placeholder: @GET("{userId}/profile")
	regexp.MustCompile(`@(?:GET|POST|PUT|DELETE|PATCH)\s*\(\s*"\{?(\w+)\}?[^"]*"\s*\)`),
}

var androidURLRe = regexp.MustCompile(`https?://([^\s"'\)]+)`)

// extractServiceFromPath tries to extract a service name from an API path.
// e.g. "/api/users" → "users-service", "/user/v1/profile" → "user-service"
func extractServiceFromPath(path string) string {
	path = strings.TrimPrefix(path, "/")
	parts := strings.Split(path, "/")
	// Skip common prefixes: api, v1, v2, rest
	idx := 0
	for idx < len(parts) && isCommonPrefix(parts[idx]) {
		idx++
	}
	if idx < len(parts) {
		return parts[idx] + "-service"
	}
	if len(parts) > 0 {
		return parts[0] + "-service"
	}
	return ""
}

func isCommonPrefix(s string) bool {
	s = strings.ToLower(s)
	switch s {
	case "api", "rest", "v1", "v2", "v3", "v4", "v5", "graphql", "public", "internal":
		return true
	}
	return strings.HasPrefix(s, "v") && len(s) == 2 // v1..v9
}
