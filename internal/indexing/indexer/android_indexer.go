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
	var edges []domain.DependencyEdge
	for _, file := range files {
		if isTestSourcePath(relativeTo(root, file)) {
			continue
		}
		text := readFile(file)
		if !strings.Contains(text, "@GET") && !strings.Contains(text, "@POST") &&
			!strings.Contains(text, "Retrofit") && !strings.Contains(text, "retrofit") &&
			!strings.Contains(text, "OkHttp") && !strings.Contains(text, "okhttp") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findKotlinModuleRoot(root, file)
		caller := dependencyIdentity(root, file)
		if moduleRoot != "" && caller.Name == "unknown" {
			caller.Name = filepath.Base(moduleRoot)
		}

		// Retrofit interfaces: @GET("api/users") / @POST("api/orders")
		// Only match on files that actually import retrofit2.
		hasRetrofitImport := strings.Contains(text, "retrofit2.http") ||
			strings.Contains(text, "import retrofit2.")

		if hasRetrofitImport {
			for _, re := range retroFitPatterns {
				for _, m := range re.FindAllStringSubmatch(text, -1) {
					if len(m) >= 2 {
						path := m[len(m)-1]
						// Don't guess service names from paths — use the whole path
						// as a diagnostic target only. If it looks like a URL, extract host.
						target := ""
						if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
							target = strings.TrimPrefix(path, "http://")
							target = strings.TrimPrefix(target, "https://")
							target, _, _ = strings.Cut(target, "/")
						}
						if target == "" {
							continue
						}
						if strings.Contains(target, "localhost") || strings.Contains(target, "127.0.0.1") {
							continue
						}
						edges = append(edges, domain.DependencyEdge{
							CallerServiceKey: caller.Key,
							From:             caller.Name,
							To:               target,
							Type:             domain.EdgeHTTP,
							Evidence:         []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
							Confidence:       0.65,
						})
					}
				}
			}
		}
		// OkHttp direct URLs
		for _, m := range androidURLRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := strings.TrimPrefix(m[1], "http://")
				target = strings.TrimPrefix(target, "https://")
				target, _, _ = strings.Cut(target, "/")
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
