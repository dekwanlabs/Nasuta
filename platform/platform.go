package platform

import (
	"crypto/sha1"
	"fmt"
	"regexp"
	"strings"
)

var (
	nonAlnum   = regexp.MustCompile(`[_\s]+`)
	multiSlash = regexp.MustCompile(`/+`)
)

func Normalize(value string) string {
	return nonAlnum.ReplaceAllString(strings.ToLower(value), "-")
}

func TruncateForLog(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func CollapseSlashes(s string) string {
	return multiSlash.ReplaceAllString(s, "/")
}

func UUIDFromString(key string) string {
	h := sha1.Sum([]byte(key))
	var b [16]byte
	copy(b[:], h[:16])
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func Dedupe[T comparable](in []T) []T {
	seen := make(map[T]struct{}, len(in))
	out := make([]T, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// FirstNonEmpty returns the first value whose TrimSpace is non-empty.
func FirstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
