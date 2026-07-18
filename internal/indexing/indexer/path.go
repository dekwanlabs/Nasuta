package indexer

import "path/filepath"

// toPosix converts an OS path to forward-slash form.
func toPosix(p string) string {
	return filepath.ToSlash(p)
}

// relativeTo returns filePath relative to root in posix form.
func relativeTo(root, filePath string) string {
	rel, err := filepath.Rel(root, filePath)
	if err != nil {
		return toPosix(filePath)
	}
	return toPosix(rel)
}
