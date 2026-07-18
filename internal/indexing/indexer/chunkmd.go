package indexer

import (
	"crypto/sha1"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DocChunk is a markdown chunk ready for embedding.
type DocChunk struct {
	DocID         string // parent document ID
	ChunkIndex    int    // 0-based index within the document
	Title         string // document title
	SectionHeader string // current ##/### section heading (for semantic context)
	Text          string // chunk content
}

// DocChunkConfig tunes the document chunker.
type DocChunkConfig struct {
	MaxChars     int // max characters per chunk (default 5000)
	MinChars     int // minimum chunk size before merging (default 200)
	OverlapChars int // overlap between consecutive hard-split chunks (default 500)
}

// DefaultDocChunkConfig returns sensible defaults for document chunking.
func DefaultDocChunkConfig() DocChunkConfig {
	return DocChunkConfig{
		MaxChars:     5000,
		MinChars:     200,
		OverlapChars: 500,
	}
}

// mdHeadingRe matches markdown headings (##, ###, ####) at line start.
var mdHeadingRe = regexp.MustCompile(`(?m)^#{2,4}\s+(.+)$`)

// ChunkMarkdown splits a markdown document into semantic chunks.
func ChunkMarkdown(docID, title, content string, cfg DocChunkConfig) []DocChunk {
	if cfg.MaxChars <= 0 {
		cfg = DefaultDocChunkConfig()
	}
	if cfg.MinChars <= 0 {
		cfg.MinChars = 200
	}
	if cfg.OverlapChars <= 0 {
		cfg.OverlapChars = 500
	}

	sections := splitByHeadingsWithHeader(content)
	if len(sections) == 0 {
		return nil
	}

	var chunks []DocChunk

	for _, sec := range sections {
		secChunks := chunkSection(docID, title, sec.header, sec.body, cfg)
		chunks = append(chunks, secChunks...)
	}

	chunks = mergeShortChunks(chunks, cfg.MinChars)

	for i := range chunks {
		chunks[i].ChunkIndex = i
	}

	return chunks
}

// splitByHeadingsWithHeader splits content at heading boundaries.
func splitByHeadingsWithHeader(content string) []struct {
	header string
	body   string
} {
	locs := mdHeadingRe.FindAllStringSubmatchIndex(content, -1)
	if len(locs) == 0 {
		return []struct {
			header string
			body   string
		}{{header: "", body: content}}
	}

	var out []struct {
		header string
		body   string
	}
	prev := 0
	var prevHeader string
	for _, loc := range locs {
		start := loc[0]
		headerText := content[loc[2]:loc[3]]
		if start > prev {
			body := strings.TrimSpace(content[prev:start])
			if body != "" {
				out = append(out, struct {
					header string
					body   string
				}{header: prevHeader, body: body})
			}
		}
		prev = start
		prevHeader = strings.TrimSpace(headerText)
	}
	if prev < len(content) {
		body := strings.TrimSpace(content[prev:])
		if body != "" {
			out = append(out, struct {
				header string
				body   string
			}{header: prevHeader, body: body})
		}
	}
	return out
}

func chunkSection(docID, title, sectionHeader, body string, cfg DocChunkConfig) []DocChunk {
	runes := len([]rune(body))
	if runes <= cfg.MaxChars {
		return []DocChunk{{
			DocID:         docID,
			Title:         title,
			SectionHeader: sectionHeader,
			Text:          buildChunkText(title, sectionHeader, body),
		}}
	}

	paras := strings.Split(body, "\n\n")
	var chunks []DocChunk
	var buf strings.Builder
	bufRunes := 0

	flush := func() {
		if buf.Len() > 0 {
			chunks = append(chunks, DocChunk{
				DocID:         docID,
				Title:         title,
				SectionHeader: sectionHeader,
				Text:          buildChunkText(title, sectionHeader, strings.TrimSpace(buf.String())),
			})
			buf.Reset()
			bufRunes = 0
		}
	}

	for _, para := range paras {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		paraRunes := len([]rune(para))

		if paraRunes > cfg.MaxChars {
			flush()
			subs := splitBySentences(para, cfg)
			for _, sub := range subs {
				chunks = append(chunks, DocChunk{
					DocID:         docID,
					Title:         title,
					SectionHeader: sectionHeader,
					Text:          buildChunkText(title, sectionHeader, sub),
				})
			}
			continue
		}

		if bufRunes+paraRunes > cfg.MaxChars && bufRunes > 0 {
			flush()
		}

		if buf.Len() > 0 {
			buf.WriteString("\n\n")
			bufRunes += 2
		}
		buf.WriteString(para)
		bufRunes += paraRunes
	}
	flush()

	return chunks
}

// splitBySentences splits text at sentence boundaries near MaxChars.
func splitBySentences(text string, cfg DocChunkConfig) []string {
	runes := []rune(text)
	if len(runes) <= cfg.MaxChars {
		return []string{text}
	}

	var parts []string
	start := 0

	for start < len(runes) {
		target := start + cfg.MaxChars
		if target >= len(runes) {
			parts = append(parts, string(runes[start:]))
			break
		}

		searchStart := start + cfg.MaxChars*7/10
		if searchStart < start {
			searchStart = start + 1
		}

		cut := findSentenceBoundary(runes, searchStart, target)
		if cut <= start {
			cut = target
		}

		parts = append(parts, string(runes[start:cut]))

		start = cut - cfg.OverlapChars/2
		if start <= 0 {
			start = 1
		}
	}

	return parts
}

// findSentenceBoundary scans from hi down to lo looking for a sentence end.
func findSentenceBoundary(runes []rune, lo, hi int) int {
	if hi >= len(runes) {
		hi = len(runes) - 1
	}
	if lo >= hi {
		return -1
	}

	// Scan backwards from hi → lo.
	for i := hi; i >= lo; i-- {
		ch := runes[i]
		// Strong sentence terminators
		if ch == '。' || ch == '！' || ch == '？' {
			return i + utf8.RuneLen(ch) // cut after full-width punctuation
		}
		if ch == '.' || ch == '!' || ch == '?' {
			// Check it's a real sentence end, not an abbreviation like "e.g."
			if i+1 < len(runes) && (runes[i+1] == ' ' || runes[i+1] == '\n') {
				return i + 2 // cut after ". "
			}
		}
		if ch == '\n' {
			return i + 1 // cut after newline
		}
	}
	return -1
}

// mergeShortChunks merges tail chunks shorter than minChars into the previous chunk.
func mergeShortChunks(chunks []DocChunk, minChars int) []DocChunk {
	if len(chunks) < 2 {
		return chunks
	}

	var out []DocChunk
	out = append(out, chunks[0])

	for i := 1; i < len(chunks); i++ {
		prev := &out[len(out)-1]
		curr := chunks[i]
		currLen := len([]rune(curr.Text))

		if currLen < minChars {
			// Merge current into previous.
			prev.Text = prev.Text + "\n\n" + curr.Text
		} else {
			out = append(out, curr)
		}
	}

	return out
}

// buildChunkText assembles the final text for a chunk, including document title
// and section heading for retrieval-time semantic context.
func buildChunkText(title, sectionHeader, body string) string {
	var prefix strings.Builder
	if title != "" {
		prefix.WriteString("# ")
		prefix.WriteString(title)
	}
	if sectionHeader != "" {
		if prefix.Len() > 0 {
			prefix.WriteString("\n")
		}
		prefix.WriteString("## ")
		prefix.WriteString(sectionHeader)
	}

	if prefix.Len() == 0 {
		return body
	}
	prefix.WriteString("\n\n")
	prefix.WriteString(body)
	return prefix.String()
}

// DocChunkID generates a stable, deterministic Qdrant point ID for a document chunk.
// Format: UUID v5-like (8-4-4-4-12 hex chars).
func DocChunkID(docID string, chunkIndex int) string {
	hash := sha1.Sum([]byte(fmt.Sprintf("doc:%s:%d", docID, chunkIndex)))
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
}

// DocID returns the stable ID for a (title, filename) pair.
// It is used as the MySQL document primary key and the Qdrant doc_id payload.
// All document ingest paths use the same formula.
func DocID(title, filename string) string {
	h := sha1.Sum([]byte(title + ":" + filename))
	return fmt.Sprintf("doc-%x", h[:8])
}
