package indexer

import "strings"

// stripJVMComments preserves line positions while removing comment-only syntax.
func stripJVMComments(text string) string {
	const (
		jvmCode = iota
		jvmLineComment
		jvmBlockComment
		jvmString
		jvmChar
		jvmRawString
	)

	var out strings.Builder
	out.Grow(len(text))
	state := jvmCode
	blockDepth := 0
	escaped := false
	for i := 0; i < len(text); {
		switch state {
		case jvmCode:
			switch {
			case strings.HasPrefix(text[i:], `"""`):
				out.WriteString(`"""`)
				i += 3
				state = jvmRawString
			case strings.HasPrefix(text[i:], "//"):
				out.WriteString("  ")
				i += 2
				state = jvmLineComment
			case strings.HasPrefix(text[i:], "/*"):
				out.WriteString("  ")
				i += 2
				blockDepth = 1
				state = jvmBlockComment
			case text[i] == '"':
				out.WriteByte(text[i])
				i++
				escaped = false
				state = jvmString
			case text[i] == '\'':
				out.WriteByte(text[i])
				i++
				escaped = false
				state = jvmChar
			default:
				out.WriteByte(text[i])
				i++
			}
		case jvmLineComment:
			if text[i] == '\n' {
				out.WriteByte('\n')
				state = jvmCode
			} else {
				out.WriteByte(' ')
			}
			i++
		case jvmBlockComment:
			switch {
			case strings.HasPrefix(text[i:], "/*"):
				out.WriteString("  ")
				i += 2
				blockDepth++
			case strings.HasPrefix(text[i:], "*/"):
				out.WriteString("  ")
				i += 2
				blockDepth--
				if blockDepth == 0 {
					state = jvmCode
				}
			case text[i] == '\n':
				out.WriteByte('\n')
				i++
			default:
				out.WriteByte(' ')
				i++
			}
		case jvmString, jvmChar:
			ch := text[i]
			out.WriteByte(ch)
			i++
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case state == jvmString && ch == '"':
				state = jvmCode
			case state == jvmChar && ch == '\'':
				state = jvmCode
			}
		case jvmRawString:
			if strings.HasPrefix(text[i:], `"""`) {
				out.WriteString(`"""`)
				i += 3
				state = jvmCode
			} else {
				out.WriteByte(text[i])
				i++
			}
		}
	}
	return out.String()
}
