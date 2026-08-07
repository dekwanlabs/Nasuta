package docgen

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxTotalBytes = 50 * 1024 // hard cap on total file content sent to LLM

// sourceExts are file extensions we consider "readable source" for LLM analysis.
var sourceExts = map[string]bool{
	".c": true, ".h": true, ".cpp": true, ".cc": true, ".cxx": true, ".hpp": true, ".hxx": true,
	".swift": true, ".m": true, ".mm": true,
	".java": true, ".kt": true, ".kts": true,
	".go": true,
	".py": true, ".pyi": true,
	".rs": true,
	".js": true, ".ts": true, ".jsx": true, ".tsx": true,
	".dart":  true,
	".cs":    true,
	".rb":    true,
	".php":   true,
	".proto": true,
	".s":     true, ".S": true, ".ld": true, // linker scripts / startup
	".cmake": true, ".txt": true, // CMakeLists.txt, requirements.txt, etc.
	".yml": true, ".yaml": true, ".toml": true, ".json": true, ".xml": true,
	".gradle": true, ".podspec": true, ".podfile": true,
	".md": true, ".rst": true,
	".cfg": true, ".conf": true, ".ini": true,
	".sh": true, ".bash": true, ".zsh": true,
	".dockerfile": true, ".dockerignore": true,
	".env": true, ".gitignore": true,
}

// skipDirs are directory names we never descend into.
var skipDirs = map[string]bool{
	".git": true, "build": true, "cmake-build-debug": true, "cmake-build-release": true,
	"node_modules": true, ".venv": true, "venv": true, "__pycache__": true,
	".claude": true, ".codex": true,
	"Pods": true, ".build": true, "DerivedData": true, "Carthage": true,
	"target": true, ".gradle": true, ".idea": true, ".vscode": true,
	"vendor": true, "third_party": true, "thirdparty": true,
	"dist": true, ".next": true, ".nuxt": true, "out": true,
	"coverage": true, ".nyc_output": true,
	".tox": true, ".eggs": true, "*.egg-info": true,
}

func skipDirectory(name string) bool {
	return skipDirs[name]
}

// fileEntry is one collected file.
type fileEntry struct {
	path    string
	content string
	size    int
}

// collectFileTree walks dir and returns only the file tree (paths, no content).
// Used for lightweight classification where full file contents are unnecessary.
func collectFileTree(dir string) string {
	var paths []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && skipDirectory(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 200*1024 {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !sourceExts[ext] && !isSpecialFile(d.Name()) {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		paths = append(paths, rel)
		return nil
	})
	sort.Slice(paths, func(i, j int) bool {
		ai, aj := priority(paths[i]), priority(paths[j])
		if ai != aj {
			return ai < aj
		}
		return paths[i] < paths[j]
	})
	var sb strings.Builder
	sb.WriteString("## Project File Tree\n```\n")
	for _, p := range paths {
		sb.WriteString(p)
		sb.WriteString("\n")
	}
	sb.WriteString("```\n")
	return sb.String()
}

// collectProjectFiles walks dir and collects source files suitable for LLM analysis.
// Returns a formatted string with the file tree and key file contents.
func collectProjectFiles(dir string) string {
	var entries []fileEntry

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && skipDirectory(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 200*1024 { // skip files > 200KB
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		if !sourceExts[ext] && !isSpecialFile(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		content := string(data)
		rel, _ := filepath.Rel(dir, path)
		if len(content) > 8000 {
			content = content[:8000] + "\n... [truncated]"
		}
		entries = append(entries, fileEntry{path: rel, content: content, size: len(content)})
		return nil
	})

	// Sort: README first, then config files, then source, limit total bytes.
	sort.Slice(entries, func(i, j int) bool {
		ai, aj := priority(entries[i].path), priority(entries[j].path)
		if ai != aj {
			return ai < aj
		}
		return entries[i].path < entries[j].path
	})

	var sb strings.Builder
	sb.WriteString("## Project File Tree\n```\n")
	total := 0
	for _, e := range entries {
		sb.WriteString(e.path)
		sb.WriteString("\n")
		total += e.size
	}
	sb.WriteString("```\n\n")

	// Write file contents up to the byte cap.
	written := 0
	for _, e := range entries {
		if written+e.size > maxTotalBytes && written > 10000 {
			sb.WriteString("... [remaining " + itoa(len(entries)) + " files omitted to stay within context limit]\n")
			break
		}
		ext := strings.ToLower(filepath.Ext(e.path))
		lang := strings.TrimPrefix(ext, ".")
		if lang == "yml" {
			lang = "yaml"
		}
		if lang == "s" || lang == "S" {
			lang = "asm"
		}
		sb.WriteString("### ")
		sb.WriteString(e.path)
		sb.WriteString("\n```")
		sb.WriteString(lang)
		sb.WriteString("\n")
		sb.WriteString(e.content)
		if !strings.HasSuffix(e.content, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n\n")
		written += e.size
	}
	return sb.String()
}

func priority(path string) int {
	base := strings.ToLower(filepath.Base(path))
	// README first.
	if strings.HasPrefix(base, "readme") {
		return 0
	}
	// Config / build files next.
	if base == "cmakelists.txt" || base == "makefile" || base == "package.json" ||
		base == "go.mod" || base == "cargo.toml" || base == "pom.xml" ||
		base == "build.gradle" || base == "build.gradle.kts" ||
		base == "podfile" || base == "package.swift" ||
		base == "dockerfile" || strings.HasPrefix(base, "docker-compose") {
		return 1
	}
	// Main entry points next.
	if strings.HasPrefix(base, "main.") || base == "appdelegate.swift" ||
		base == "app.swift" || base == "index.js" || base == "index.ts" {
		return 2
	}
	return 3
}

func isSpecialFile(name string) bool {
	n := strings.ToLower(name)
	return n == "cmakelists.txt" || n == "makefile" || n == "dockerfile" ||
		n == "podfile" || n == "package.swift" || n == "gemfile" ||
		n == "rakefile" || n == "brewfile"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
