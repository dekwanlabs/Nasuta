package docgen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractDocHash(t *testing.T) {
	// the real header format written by buildMarkdown
	doc := []byte("<!-- hash:15dfb0c7e1e89e4f5fc03d89b6d142d6 -->\n# Title\nbody\n")
	got, ok := extractDocHash(doc)
	if !ok {
		t.Fatal("expected ok")
	}
	if got != "15dfb0c7e1e89e4f5fc03d89b6d142d6" {
		t.Fatalf("got %q (the ` -->` suffix must be stripped)", got)
	}

	// round-trip: what buildMarkdown writes must compare-equal to hashModule output
	hash := "abc123"
	written := "<!-- hash:" + hash + " -->\n# X\n"
	if h, ok := extractDocHash([]byte(written)); !ok || h != hash {
		t.Fatalf("round-trip mismatch: got %q ok=%v, want %q", h, ok, hash)
	}

	if _, ok := extractDocHash([]byte("# no header\n")); ok {
		t.Error("expected no hash for header-less doc")
	}
}

func TestHashModuleIgnoresAgentMetadataDirectories(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := hashModule(dir)

	for _, name := range []string{".claude", ".codex"} {
		metadataDir := filepath.Join(dir, name)
		if err := os.MkdirAll(metadataDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(metadataDir, "ignored.go"), []byte("package ignored\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if after := hashModule(dir); after != before {
		t.Fatalf("agent metadata changed module hash: before=%s after=%s", before, after)
	}

	if err := os.WriteFile(source, []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if after := hashModule(dir); after == before {
		t.Fatal("source change did not change module hash")
	}
}
