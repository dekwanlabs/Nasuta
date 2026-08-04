package docgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCollectProjectFilesSkipsAgentMetadataDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".claude", ".codex"} {
		metadataDir := filepath.Join(dir, name)
		if err := os.MkdirAll(metadataDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(metadataDir, "instructions.md"), []byte("ignore me\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := collectProjectFiles(dir)
	if !strings.Contains(got, "main.go") {
		t.Fatalf("project files omitted source: %s", got)
	}
	for _, name := range []string{".claude", ".codex"} {
		if strings.Contains(got, name) {
			t.Fatalf("project files included %s metadata: %s", name, got)
		}
	}
}
