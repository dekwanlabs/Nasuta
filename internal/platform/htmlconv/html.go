// Package htmlconv converts HTML into compact Markdown for retrieval and import.
package htmlconv

import (
	"html"
	"strings"
	"unicode"

	nethtml "golang.org/x/net/html"
)

// Markdown extracts readable content and preserves common document structure.
func Markdown(source string) string {
	w := &writer{}
	tokenizer := nethtml.NewTokenizer(strings.NewReader(source))
	skipDepth := 0
	preDepth := 0
	mainDepth := 0
	foundMain := false

	for {
		switch tokenizer.Next() {
		case nethtml.ErrorToken:
			return strings.TrimSpace(strings.ReplaceAll(w.String(), "\r\n", "\n"))
		case nethtml.TextToken:
			if skipDepth == 0 && (!foundMain || mainDepth > 0) {
				w.Text(string(tokenizer.Text()), preDepth > 0)
			}
		case nethtml.StartTagToken:
			name, hasAttr := tokenizer.TagName()
			tag := strings.ToLower(string(name))
			if isSkippedTag(tag) {
				skipDepth++
				continue
			}
			if tag == "main" || tag == "article" {
				foundMain = true
				mainDepth++
				continue
			}
			if skipDepth > 0 || (foundMain && mainDepth == 0) {
				continue
			}
			if tag == "a" {
				w.StartLink(attribute(tokenizer, hasAttr, "href"))
				continue
			}
			w.StartTag(tag)
			if tag == "pre" {
				preDepth++
			}
		case nethtml.SelfClosingTagToken:
			name, _ := tokenizer.TagName()
			w.SelfClosingTag(strings.ToLower(string(name)))
		case nethtml.EndTagToken:
			name, _ := tokenizer.TagName()
			tag := strings.ToLower(string(name))
			if skipDepth > 0 {
				if isSkippedTag(tag) {
					skipDepth--
				}
				continue
			}
			if tag == "main" || tag == "article" {
				mainDepth--
				continue
			}
			if foundMain && mainDepth == 0 {
				continue
			}
			if tag == "pre" && preDepth > 0 {
				preDepth--
			}
			w.EndTag(tag)
		}
	}
}

func isSkippedTag(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "head", "nav", "header", "footer", "aside":
		return true
	default:
		return false
	}
}

func attribute(tokenizer *nethtml.Tokenizer, more bool, name string) string {
	for more {
		key, value, hasMore := tokenizer.TagAttr()
		if strings.EqualFold(string(key), name) {
			return html.UnescapeString(string(value))
		}
		more = hasMore
	}
	return ""
}

type writer struct {
	b     strings.Builder
	links []string
}

func (w *writer) String() string { return w.b.String() }

func (w *writer) StartTag(tag string) {
	switch tag {
	case "title", "h1":
		w.ensureBlankLine()
		w.b.WriteString("# ")
	case "h2":
		w.ensureBlankLine()
		w.b.WriteString("## ")
	case "h3":
		w.ensureBlankLine()
		w.b.WriteString("### ")
	case "h4", "h5", "h6":
		w.ensureBlankLine()
		w.b.WriteString("#### ")
	case "li":
		w.ensureNewline()
		w.b.WriteString("- ")
	case "pre":
		w.ensureBlankLine()
		w.b.WriteString("```\n")
	case "blockquote":
		w.ensureBlankLine()
		w.b.WriteString("> ")
	case "tr":
		w.ensureNewline()
	case "td", "th":
		w.ensureCellBoundary()
	default:
		if isBlockTag(tag) {
			w.ensureNewline()
		}
	}
}

func (w *writer) SelfClosingTag(tag string) {
	if tag == "br" || tag == "hr" || isBlockTag(tag) {
		w.ensureNewline()
	}
}

func (w *writer) EndTag(tag string) {
	switch tag {
	case "a":
		w.EndLink()
	case "title", "h1", "h2", "h3", "h4", "h5", "h6", "blockquote":
		w.ensureBlankLine()
	case "pre":
		w.ensureNewline()
		w.b.WriteString("```\n")
		w.ensureBlankLine()
	case "li", "p", "tr":
		w.ensureNewline()
	case "td", "th":
		return
	default:
		if isBlockTag(tag) {
			w.ensureNewline()
		}
	}
}

func (w *writer) StartLink(href string) {
	w.links = append(w.links, strings.TrimSpace(href))
}

func (w *writer) EndLink() {
	if len(w.links) == 0 {
		return
	}
	href := w.links[len(w.links)-1]
	w.links = w.links[:len(w.links)-1]
	if href != "" {
		w.b.WriteString(" (")
		w.b.WriteString(href)
		w.b.WriteByte(')')
	}
}

func (w *writer) Text(text string, pre bool) {
	text = html.UnescapeString(text)
	if !pre {
		text = collapseInline(text)
	}
	if strings.TrimSpace(text) == "" {
		if !w.lastIsSpace() {
			w.b.WriteByte(' ')
		}
		return
	}
	if !pre && w.b.Len() > 0 && !w.lastIsSpace() && !startsWithSpaceOrPunct(text) {
		w.b.WriteByte(' ')
	}
	w.b.WriteString(text)
}

func (w *writer) ensureNewline() {
	if w.b.Len() == 0 || w.lastByte() == '\n' {
		return
	}
	w.b.WriteByte('\n')
}

func (w *writer) ensureBlankLine() {
	if w.b.Len() == 0 || strings.HasSuffix(w.b.String(), "\n\n") {
		return
	}
	w.ensureNewline()
	w.b.WriteByte('\n')
}

func (w *writer) ensureCellBoundary() {
	if w.b.Len() == 0 || w.lastByte() == '\n' || strings.HasSuffix(w.b.String(), " | ") {
		return
	}
	w.b.WriteString(" | ")
}

func (w *writer) lastByte() byte {
	if w.b.Len() == 0 {
		return 0
	}
	value := w.b.String()
	return value[len(value)-1]
}

func (w *writer) lastIsSpace() bool {
	return w.b.Len() > 0 && unicode.IsSpace(rune(w.lastByte()))
}

func collapseInline(value string) string {
	if value == "" {
		return ""
	}
	runes := []rune(value)
	leading := unicode.IsSpace(runes[0])
	trailing := unicode.IsSpace(runes[len(runes)-1])
	out := strings.Join(strings.Fields(value), " ")
	if leading {
		out = " " + out
	}
	if trailing {
		out += " "
	}
	return out
}

func startsWithSpaceOrPunct(value string) bool {
	for _, r := range value {
		return unicode.IsSpace(r) || strings.ContainsRune(".,;:!?)]}", r)
	}
	return false
}

func isBlockTag(tag string) bool {
	switch tag {
	case "address", "article", "aside", "blockquote", "body", "caption", "dd",
		"details", "dialog", "div", "dl", "dt", "fieldset", "figcaption", "figure",
		"footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "head", "header",
		"html", "li", "main", "nav", "ol", "p", "pre", "section", "table",
		"tbody", "td", "tfoot", "th", "thead", "tr", "ul":
		return true
	default:
		return false
	}
}
