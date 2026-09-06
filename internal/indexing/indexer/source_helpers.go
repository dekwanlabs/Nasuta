package indexer

import (
	"os"
	"path/filepath"
	"strings"
)

// findModuleRootByMarkers walks upward once and preserves the caller's marker
// priority at each directory. This is important for nested multi-build roots.
func findModuleRootByMarkers(root, file string, markers ...string) string {
	current := filepath.Dir(file)
	for strings.HasPrefix(current, root) {
		for _, marker := range markers {
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

// directoryContainsFile is shared by module marker scans that need to ignore
// generated/vendor directories and stop after finding one matching file.
func directoryContainsFile(dir string, match func(os.DirEntry) bool) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if entry.IsDir() {
			if ignoredDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if match(entry) {
			found = true
		}
		return nil
	})
	return found
}

// stripTripleQuotedLiteral masks a triple-quoted literal while preserving byte
// offsets and line counts. Some languages treat the opening delimiter as part
// of the structural text, so callers choose whether to mask it.
func stripTripleQuotedLiteral(out []byte, start int, quote byte, maskOpening bool) int {
	if maskOpening {
		for i := start; i < start+3 && i < len(out); i++ {
			out[i] = ' '
		}
	}
	i := start + 3
	for i+2 < len(out) {
		if out[i] == quote && out[i+1] == quote && out[i+2] == quote {
			out[i], out[i+1], out[i+2] = ' ', ' ', ' '
			return i + 3
		}
		if out[i] != '\n' {
			out[i] = ' '
		}
		i++
	}
	return i
}

// stripQuotedLiteral masks a single- or double-quoted literal while preserving
// byte offsets and line counts for structural parsers.
func stripQuotedLiteral(out []byte, start int, quote byte) int {
	out[start] = ' '
	for i := start + 1; i < len(out); i++ {
		if out[i] == '\\' {
			out[i] = ' '
			if i+1 < len(out) {
				out[i+1] = ' '
				i++
			}
			continue
		}
		if out[i] == quote {
			out[i] = ' '
			return i + 1
		}
		if out[i] != '\n' {
			out[i] = ' '
		}
	}
	return len(out)
}
