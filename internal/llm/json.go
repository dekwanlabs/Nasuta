package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// StripFences removes markdown code fences (```json, ```) and surrounding space.
func StripFences(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// extractJSON isolates one balanced top-level object or array without treating
// brackets inside strings as structure. An incomplete span is returned with ok=false
// so RepairJSON can close it while ParseJSONLoose still rejects truncation.
func extractJSON(s string) (string, bool) {
	s = strings.TrimSpace(s)
	objStart, arrStart := strings.Index(s, "{"), strings.Index(s, "[")
	var start int
	switch {
	case objStart < 0 && arrStart < 0:
		return "", false
	case objStart < 0:
		start = arrStart
	case arrStart < 0:
		start = objStart
	default:
		if objStart < arrStart {
			start = objStart
		} else {
			start = arrStart
		}
	}
	stack := make([]byte, 0, 8)
	inString, escaped := false, false
	for i := start; i < len(s); i++ {
		char := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, char)
		case '}', ']':
			if len(stack) == 0 || !matchingBrackets(stack[len(stack)-1], char) {
				return s[start : i+1], false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				end := i + 1
				rest := strings.TrimSpace(s[end:])
				if strings.HasPrefix(rest, "{") || strings.HasPrefix(rest, "[") {
					return s[start:], true
				}
				return s[start:end], true
			}
		}
	}
	return s[start:], false
}

func matchingBrackets(open, close byte) bool {
	return open == '{' && close == '}' || open == '[' && close == ']'
}

// ParseJSONLoose decodes JSON that may be wrapped in fences or prose. It tries
// a direct unmarshal first, then falls back to the outermost JSON span.
func ParseJSONLoose(source string, out any) error {
	return parseJSONLoose(source, out, false)
}

func parseJSONLoose(source string, out any, disallowUnknownFields bool) error {
	stripped := StripFences(source)
	if err := decodeJSON([]byte(stripped), out, disallowUnknownFields); err == nil {
		return nil
	}
	span, ok := extractJSON(stripped)
	if !ok {
		return fmt.Errorf("no JSON object in response")
	}
	return decodeJSON([]byte(span), out, disallowUnknownFields)
}

func decodeJSON(raw []byte, out any, disallowUnknownFields bool) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if disallowUnknownFields {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

// RepairJSON best-effort repairs common LLM-JSON defects that encoding/json
// rejects, without ever corrupting string contents: trailing commas, // and
// /* */ comments, unquoted keys, bare control chars inside strings, and
// truncated closers when output was cut by max_tokens. The returned string is
// re-unmarshalled by the caller; unmarshal remains the final arbiter.
//
// Not repaired (fall through to a model reprompt): single-quoted strings,
// bareword values, concatenated objects. Single-quote conversion is intentionally
// skipped because the apostrophe-in-word case (don't, it's) cannot be
// distinguished from a string delimiter without a full grammar.
func RepairJSON(source string) string {
	s := strings.TrimSpace(StripFences(source))
	span, _ := extractJSON(s)
	if span == "" {
		span = s
	}
	return repairScan(span)
}

// repairScan is the string-aware single pass. O(n); structural edits happen only
// outside string literals, so string contents are never mutated except that
// bare control chars are escaped.
func repairScan(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var stack []byte
	var lastStruct byte // last structural char written outside a string
	inStr, escape := false, false
	inLine, inBlock := false, false
	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				b.WriteByte(c)
			}
			i++
		case inBlock:
			if c == '*' && i+1 < n && s[i+1] == '/' {
				inBlock = false
				i += 2
			} else {
				i++
			}
		case inStr:
			if escape {
				escape = false
				b.WriteByte(c)
				i++
				continue
			}
			switch c {
			case '\\':
				escape = true
				b.WriteByte(c)
			case '"':
				inStr = false
				b.WriteByte(c)
			case '\n':
				b.WriteString(`\n`)
			case '\t':
				b.WriteString(`\t`)
			case '\r':
				b.WriteString(`\r`)
			default:
				if c < 0x20 {
					fmt.Fprintf(&b, `\u%04x`, c)
				} else {
					b.WriteByte(c)
				}
			}
			i++
		default:
			switch c {
			case '"':
				inStr = true
				b.WriteByte(c)
				i++
			case '/':
				switch {
				case i+1 < n && s[i+1] == '/':
					inLine = true
					i += 2
				case i+1 < n && s[i+1] == '*':
					b.WriteByte(' ')
					inBlock = true
					i += 2
				default:
					b.WriteByte(c)
					i++
				}
			case '{', '[':
				stack = append(stack, c)
				lastStruct = c
				b.WriteByte(c)
				i++
			case '}', ']':
				if len(stack) > 0 {
					top := stack[len(stack)-1]
					if (c == '}' && top == '{') || (c == ']' && top == '[') {
						stack = stack[:len(stack)-1]
					}
				}
				lastStruct = c
				b.WriteByte(c)
				i++
			case ',':
				if j := nextNonSpace(s, i+1); j >= n || s[j] == '}' || s[j] == ']' {
					i++ // drop trailing comma
				} else {
					lastStruct = c
					b.WriteByte(c)
					i++
				}
			case ':':
				lastStruct = c
				b.WriteByte(c)
				i++
			default:
				// Unquoted key: identifier at a key position (after '{' or ',') that
				// is followed by ':'. Bareword values are left untouched.
				if isIdentStart(c) && (lastStruct == '{' || lastStruct == ',') {
					start := i
					for i < n && isIdentPart(s[i]) {
						i++
					}
					ident := s[start:i]
					if j := nextNonSpace(s, i); j < n && s[j] == ':' {
						b.WriteByte('"')
						b.WriteString(ident)
						b.WriteByte('"')
						continue // leave ':' for the main loop
					}
					b.WriteString(ident)
					continue
				}
				b.WriteByte(c)
				i++
			}
		}
	}
	if inStr {
		b.WriteByte('"') // close an unterminated string
	}
	for k := len(stack) - 1; k >= 0; k-- {
		switch stack[k] {
		case '{':
			b.WriteByte('}')
		case '[':
			b.WriteByte(']')
		}
	}
	return b.String()
}

func nextNonSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	return i
}

func isIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == '$'
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9') || b == '-' || b == '.'
}
