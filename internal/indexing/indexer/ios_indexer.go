package indexer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
)

// scanIOSServices finds iOS/macOS app modules (via .xcodeproj / Info.plist).
func scanIOSServices(root string, dirs []string) []domain.ServiceRecord {
	var records []domain.ServiceRecord

	// Find Info.plist files — the definitive iOS/macOS app marker
	plists := walkFiles(root, dirs, func(name string) bool { return name == "Info.plist" })
	// Also find .xcodeproj directories
	for _, dir := range dirs {
		fullDir := filepath.Join(root, dir)
		_ = filepath.WalkDir(fullDir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if _, skip := ignoredDirs[d.Name()]; skip {
					return filepath.SkipDir
				}
				if strings.HasSuffix(d.Name(), ".xcodeproj") || strings.HasSuffix(d.Name(), ".xcworkspace") {
					plists = append(plists, path)
					return filepath.SkipDir
				}
				return nil
			}
			return nil
		})
	}

	seen := map[string]struct{}{}
	for _, file := range plists {
		rel := relativeTo(root, file)
		moduleRoot := filepath.Dir(file)
		// For .xcodeproj, the project root is the parent
		if strings.HasSuffix(file, ".xcodeproj") {
			moduleRoot = filepath.Dir(file)
		}

		serviceName := readIOSAppName(moduleRoot)
		if serviceName == "" {
			serviceName = filepath.Base(moduleRoot)
		}
		key := canonicalRepo(topSegment(rel)) + "\x00" + canonicalPath(relativeTo(root, moduleRoot))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		lang := detectIOSLang(moduleRoot)
		runtime := detectIOSPlatform(moduleRoot)
		records = append(records, domain.ServiceRecord{
			ServiceName:   serviceName,
			Repo:          topSegment(rel),
			Layer:         "app",
			Scope:         "mobile",
			ModulePath:    relativeTo(root, moduleRoot),
			Language:      lang,
			Runtime:       runtime,
			Tags:          []string{"code-scan", "mobile", "ios"},
			Docs:          []string{},
			SourceOfTruth: []string{rel},
			Entrypoints:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
			Ports:         nil,
			Confidence:    0.9,
		})
	}
	return records
}

// scanIOSDependencies finds URLSession/Alamofire/Combine API calls in iOS code.
func scanIOSDependencies(root string, dirs []string) []domain.DependencyEdge {
	files := walkFiles(root, dirs, func(name string) bool {
		return strings.HasSuffix(name, ".swift") || strings.HasSuffix(name, ".m") || strings.HasSuffix(name, ".mm")
	})
	var edges []domain.DependencyEdge
	for _, file := range files {
		text := readFile(file)
		if !strings.Contains(text, "URLSession") && !strings.Contains(text, "Alamofire") &&
			!strings.Contains(text, "AF.request") && !strings.Contains(text, "Moya") &&
			!strings.Contains(text, "dataTask") {
			continue
		}
		rel := relativeTo(root, file)
		moduleRoot := findIOSModuleRoot(root, file)
		caller := filepath.Base(relativeTo(root, moduleRoot))
		if moduleRoot != "" {
			if name := readIOSAppName(moduleRoot); name != "" {
				caller = name
			}
		}

		// Alamofire: AF.request("https://api.example.com/path")
		for _, m := range iosAlamofireRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := extractIOSHost(m[1])
				if target != "" {
					edges = append(edges, domain.DependencyEdge{
						From:       caller,
						To:         target,
						Type:       domain.EdgeHTTP,
						Evidence:   []domain.Evidence{{Path: rel, Kind: domain.SourceCodeScan}},
						Confidence: 0.6,
					})
				}
			}
		}
		// Moya: enum with path patterns (e.g. .getUsers: return "/users")
		// Swift URLSession: URL(string: "https://api.example.com/path")
		for _, m := range iosURLRe.FindAllStringSubmatch(text, -1) {
			if len(m) > 1 {
				target := extractIOSHost(m[1])
				if target != "" {
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

// ---- iOS helpers ----

func readIOSAppName(dir string) string {
	// Try project.pbxproj for PRODUCT_NAME or INFOPLIST_FILE
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".xcodeproj") {
			pbx := filepath.Join(dir, e.Name(), "project.pbxproj")
			text := readFile(pbx)
			if m := regexp.MustCompile(`PRODUCT_NAME\s*=\s*(\w+)`).FindStringSubmatch(text); m != nil {
				return m[1]
			}
			if m := regexp.MustCompile(`INFOPLIST_FILE\s*=\s*"?(\w+)`).FindStringSubmatch(text); m != nil {
				return m[1]
			}
		}
	}
	// Try Info.plist for CFBundleName
	text := readFile(filepath.Join(dir, "Info.plist"))
	if m := regexp.MustCompile(`<key>CFBundleName</key>\s*<string>([^<]+)</string>`).FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	// Try Package.swift
	text = readFile(filepath.Join(dir, "Package.swift"))
	if m := regexp.MustCompile(`name:\s*"([^"]+)"`).FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

func detectIOSLang(dir string) string {
	// Check whether this iOS project uses Swift or Objective-C
	swiftCount, objcCount := 0, 0
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && (d.Name() == "Pods" || d.Name() == "DerivedData" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(d.Name()) {
		case ".swift":
			swiftCount++
		case ".m", ".mm":
			objcCount++
		}
		return nil
	})
	if swiftCount > objcCount {
		return "swift"
	}
	return "objective-c"
}

func detectIOSPlatform(dir string) string {
	// Check Info.plist / project files for platform indicators
	text := readFile(filepath.Join(dir, "Info.plist"))
	if strings.Contains(text, "UILaunchStoryboardName") || strings.Contains(text, "UIRequiredDeviceCapabilities") {
		// Check for watchOS / tvOS indicators
		if strings.Contains(text, "WKApplication") || strings.Contains(text, "WatchKit") {
			return "watchos"
		}
		if strings.Contains(text, "TVUIKit") {
			return "tvos"
		}
		return "ios"
	}
	// Check for macOS
	if strings.Contains(text, "NSApplication") || strings.Contains(text, "macOS") {
		return "macos"
	}
	return "ios"
}

func findIOSModuleRoot(root, file string) string {
	current := filepath.Dir(file)
	for strings.HasPrefix(current, root) {
		entries, _ := os.ReadDir(current)
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".xcodeproj") || strings.HasSuffix(name, ".xcworkspace") || name == "Info.plist" {
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

func extractIOSHost(url string) string {
	url = strings.TrimPrefix(url, "http://")
	url = strings.TrimPrefix(url, "https://")
	host, _, _ := strings.Cut(url, "/")
	host = strings.TrimSuffix(host, ":8080")
	host = strings.TrimSuffix(host, ":443")
	host = strings.TrimSuffix(host, ":3000")
	if host != "" && !strings.Contains(host, "localhost") && !strings.Contains(host, "127.0.0.1") {
		return host
	}
	return ""
}

var iosAlamofireRe = regexp.MustCompile(`AF\.request\s*\(\s*"(https?://[^"]+)"`)

var iosURLRe = regexp.MustCompile(`(?:URL|url)\s*\(\s*(?:string\s*:\s*)?["'](https?://[^"']+)["']`)
