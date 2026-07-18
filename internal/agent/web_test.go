package agent

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDecodeWebBodyUsesHTMLMetaCharset(t *testing.T) {
	body := append([]byte(`<html><head><meta http-equiv="Content-Type" content="text/html; charset=gb2312"></head><body>`),
		[]byte{0xc8, 0xba, 0xd3, 0xf1, 0xb4, 0xe5}...)
	body = append(body, []byte(`</body></html>`)...)

	got, encoding, err := decodeWebBody(body, "text/html")
	if err != nil {
		t.Fatalf("decodeWebBody() error = %v", err)
	}
	if encoding != "gbk" {
		t.Fatalf("encoding = %q, want gbk", encoding)
	}
	if !strings.Contains(got, "群玉村") {
		t.Fatalf("decoded body = %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatal("decoded body is not valid UTF-8")
	}
}

func TestDecodeWebBodyKeepsUndeclaredUTF8(t *testing.T) {
	want := "<html><body>海南临高</body></html>"
	got, encoding, err := decodeWebBody([]byte(want), "text/html")
	if err != nil {
		t.Fatalf("decodeWebBody() error = %v", err)
	}
	if encoding != "utf-8" || got != want {
		t.Fatalf("decodeWebBody() = (%q, %q), want (%q, utf-8)", got, encoding, want)
	}
}

func TestTruncateRunesPreservesUTF8(t *testing.T) {
	got := truncateRunes(strings.Repeat("中", 9000), 8000)
	if !utf8.ValidString(got) {
		t.Fatal("truncateRunes() split a UTF-8 rune")
	}
	if !strings.HasSuffix(got, "\n...(truncated)") {
		t.Fatalf("truncateRunes() missing marker: %q", got[len(got)-30:])
	}
}
