package indexer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dekwanlabs/nasuta/platform"
)

var ignoredDirs = map[string]struct{}{
	"target": {}, ".git": {}, "node_modules": {}, ".venv": {}, "venv": {},
	"dist": {}, "build": {}, ".idea": {}, ".claude": {}, ".codex": {},
	platform.WorkspaceMetadataDir: {}, "__pycache__": {},
	"bin": {}, "obj": {}, // .NET/C# build output
	".dart_tool":  {},               // Dart/Flutter tool cache
	"DerivedData": {}, ".build": {}, // Swift/Xcode build output
	".gradle":           {},                            // Gradle/Kotlin cache
	"cmake-build-debug": {}, "cmake-build-release": {}, // C/C++ CMake build
}

func ignoredDirectory(name string) bool {
	_, ignored := ignoredDirs[name]
	return ignored
}

// DiscoverScanDirs lists project directories under root that should be scanned.
// It excludes ignored and hidden entries.
// The walk goes two levels deep under repos/<group>/<project>.
func DiscoverScanDirs(root string) ([]string, error) {
	reposDir := filepath.Join(root, "repos")
	groups, err := os.ReadDir(reposDir)
	if err != nil {
		return nil, fmt.Errorf("read repositories directory %q: %w", reposDir, err)
	}
	var dirs []string
	var scanErrs []error
	for _, g := range groups {
		if !g.IsDir() || strings.HasPrefix(g.Name(), ".") {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(reposDir, g.Name()))
		if err != nil {
			scanErrs = append(scanErrs, fmt.Errorf("read repository group %q: %w", g.Name(), err))
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			dirs = append(dirs, filepath.Join("repos", g.Name(), e.Name()))
		}
	}
	return dirs, errors.Join(scanErrs...)
}

// ValidateScanInputs prevents an unreadable workspace from producing a partial snapshot.
func ValidateScanInputs(root string, dirs []string) error {
	var scanErrs []error
	for _, dir := range dirs {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				scanErrs = append(scanErrs, fmt.Errorf("access %q: %w", path, err))
				if entry != nil && entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if entry.IsDir() {
				if ignoredDirectory(entry.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if sensitiveFile(path) {
				return nil
			}
			file, openErr := os.Open(path)
			if openErr != nil {
				scanErrs = append(scanErrs, fmt.Errorf("read %q: %w", path, openErr))
				return nil
			}
			if closeErr := file.Close(); closeErr != nil {
				scanErrs = append(scanErrs, fmt.Errorf("close %q: %w", path, closeErr))
			}
			return nil
		})
		if err != nil {
			scanErrs = append(scanErrs, fmt.Errorf("walk scan directory %q: %w", dir, err))
		}
	}
	return errors.Join(scanErrs...)
}

// sensitiveFile reports whether a path should never be indexed / embedded.
func sensitiveFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || strings.HasPrefix(base, ".env.") && base != ".env.example" {
		return true
	}
	for _, suf := range []string{".key", ".pem", ".p12", ".keystore", ".jks"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	return false
}

// walkFiles collects files under each root/dir whose name satisfies match.
func walkFiles(root string, dirs []string, match func(name string) bool) []string {
	var out []string
	for _, dir := range dirs {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); err != nil {
			continue
		}
		// WalkDir errors are unlikely after a successful os.Stat and are
		// individually handled inside the walk func — skip the top-level error.
		_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				if ignoredDirectory(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if match(d.Name()) && !sensitiveFile(path) {
				out = append(out, path)
			}
			return nil
		})
	}
	return out
}

func hasSuffix(suffix string) func(string) bool {
	return func(name string) bool { return strings.HasSuffix(name, suffix) }
}

func statSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func joinPaths(prefix, route string) string {
	var parts []string
	if p := strings.TrimSpace(prefix); p != "" {
		parts = append(parts, p)
	}
	if r := strings.TrimSpace(route); r != "" {
		parts = append(parts, r)
	}
	joined := platform.CollapseSlashes("/" + strings.Join(parts, "/"))
	if joined == "/" {
		return "/"
	}
	return strings.TrimSuffix(joined, "/")
}
