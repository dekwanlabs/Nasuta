package tooloutput

import (
	"encoding/json"
	"fmt"
	"strings"
)

type numberedLine struct {
	number int
	text   string
}

func buildJSONLChunks(content string) ([]chunk, bool) {
	lines := splitNumberedLines(content)
	nonempty := 0
	valid := 0
	validLines := make(map[int]json.RawMessage)
	for _, line := range lines {
		if strings.TrimSpace(line.text) == "" {
			continue
		}
		nonempty++
		var raw json.RawMessage
		if json.Unmarshal([]byte(line.text), &raw) == nil {
			valid++
			validLines[line.number] = compactJSON(raw)
		}
	}
	if valid < 2 || valid*2 < nonempty {
		return nil, false
	}

	chunks := make([]chunk, 0, nonempty)
	for _, line := range lines {
		if strings.TrimSpace(line.text) == "" {
			continue
		}
		ref := fmt.Sprintf("lines:%d-%d", line.number, line.number)
		if raw, ok := validLines[line.number]; ok {
			chunks = append(chunks, chunk{
				ref: ref, kind: "json", raw: raw, ordinal: len(chunks),
			})
			continue
		}
		chunks = append(chunks, chunk{
			ref: ref, kind: "text", text: line.text, ordinal: len(chunks),
		})
	}
	return chunks, true
}

func buildTextChunks(content string, target int) []chunk {
	lines := splitNumberedLines(content)
	chunks := make([]chunk, 0, len(lines)/4+1)
	for start := 0; start < len(lines); {
		for start < len(lines) && strings.TrimSpace(lines[start].text) == "" {
			start++
		}
		if start >= len(lines) {
			break
		}
		end := start
		for end+1 < len(lines) && strings.TrimSpace(lines[end+1].text) != "" {
			end++
		}
		appendTextRange(&chunks, lines[start:end+1], target)
		start = end + 1
	}
	return chunks
}

func appendTextRange(chunks *[]chunk, lines []numberedLine, target int) {
	text := joinLines(lines)
	if EstimateTokens(text) <= target {
		appendTextChunk(chunks, lines[0].number, lines[len(lines)-1].number, text)
		return
	}

	start := 0
	currentTokens := 0
	for i, line := range lines {
		lineTokens := EstimateTokens(line.text)
		separatorTokens := 0
		if i > start {
			separatorTokens = EstimateTokens("\n")
		}
		if i > start && currentTokens+separatorTokens+lineTokens > target {
			appendTextChunk(chunks, lines[start].number, lines[i-1].number, joinLines(lines[start:i]))
			start = i
			currentTokens = lineTokens
			continue
		}
		currentTokens += separatorTokens + lineTokens
	}
	if start < len(lines) {
		appendTextChunk(chunks, lines[start].number, lines[len(lines)-1].number, joinLines(lines[start:]))
	}
}

func appendTextChunk(chunks *[]chunk, start, end int, content string) {
	*chunks = append(*chunks, chunk{
		ref:     fmt.Sprintf("lines:%d-%d", start, end),
		kind:    "text",
		text:    content,
		ordinal: len(*chunks),
	})
}

func splitNumberedLines(content string) []numberedLine {
	raw := strings.Split(content, "\n")
	lines := make([]numberedLine, len(raw))
	for i, line := range raw {
		lines[i] = numberedLine{number: i + 1, text: line}
	}
	return lines
}

func joinLines(lines []numberedLine) string {
	var out strings.Builder
	size := len(lines) - 1
	for _, line := range lines {
		size += len(line.text)
	}
	out.Grow(max(0, size))
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(line.text)
	}
	return out.String()
}
