package docgen

import "testing"

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
