package indexer

import (
	"os"
	"path/filepath"
	"testing"
)

func mustDiscoverScanDirs(t *testing.T, root string) []string {
	t.Helper()
	dirs, err := DiscoverScanDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func TestDiscoverScanDirsReportsMissingRepositoriesDirectory(t *testing.T) {
	if _, err := DiscoverScanDirs(t.TempDir()); err == nil {
		t.Fatal("DiscoverScanDirs accepted a missing repos directory")
	}
}

func TestScanInputsSkipSymbolicLinks(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "repos", "group", "service")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(repoDir, "main.go")
	if err := os.WriteFile(source, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("main.go", filepath.Join(repoDir, "linked.go")); err != nil {
		t.Skipf("create symbolic link: %v", err)
	}
	if err := os.Symlink("missing", filepath.Join(repoDir, "dangling.go")); err != nil {
		t.Fatal(err)
	}

	dirs := []string{filepath.Join("repos", "group", "service")}
	if err := ValidateScanInputs(root, dirs); err != nil {
		t.Fatalf("validate scan inputs: %v", err)
	}
	files := walkFiles(root, dirs, hasSuffix(".go"))
	if len(files) != 1 || files[0] != source {
		t.Fatalf("walkFiles = %v, want only %q", files, source)
	}
}

func TestWalkFilesSkipsAgentMetadataDirectories(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, "repos", "group", "service")
	source := filepath.Join(repoDir, "main.go")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".claude", ".codex"} {
		dir := filepath.Join(repoDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "ignored.go"), []byte("package ignored\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dirs := []string{filepath.Join("repos", "group", "service")}
	files := walkFiles(root, dirs, hasSuffix(".go"))
	if len(files) != 1 || files[0] != source {
		t.Fatalf("walkFiles = %v, want only %q", files, source)
	}
}

func TestLanguageDetectionSkipsAgentMetadataDirectories(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{".claude", ".codex"} {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "FakeApplication.kt"), []byte("class FakeApplication\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "FakeApplication.java"), []byte("class FakeApplication {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got := readAndroidLang(root); got != "java" {
		t.Fatalf("readAndroidLang = %q, want java", got)
	}
	if hasApplicationFile(root) {
		t.Fatal("hasApplicationFile found an entrypoint in agent metadata")
	}
}
