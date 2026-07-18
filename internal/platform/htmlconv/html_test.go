package htmlconv

import (
	"strings"
	"testing"
)

func TestMarkdownExtractsDocumentContent(t *testing.T) {
	source := `<html><head><title>Ignored title</title></head><body>
<nav>Ignored nav</nav><main><h1>Guide</h1><p>Read <a href="/docs">the docs</a>.</p>
<pre>go test ./...</pre><script>ignored()</script></main><footer>Ignored footer</footer></body></html>`

	got := Markdown(source)
	for _, want := range []string{"# Guide", "the docs (/docs)", "```\ngo test ./...\n```"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Markdown() missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Ignored title", "Ignored nav", "Ignored footer", "ignored()"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("Markdown() retained %q:\n%s", unwanted, got)
		}
	}
}
