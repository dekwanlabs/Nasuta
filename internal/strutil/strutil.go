// Package strutil holds small string canonicalization helpers shared across
// internal packages. It carries no business policy: callers decide which
// fields must be normalized.
package strutil

import (
	"sort"
	"strings"
)

// Canonical trims whitespace, drops empty values, removes duplicates, and
// returns a sorted copy. It is suitable for identifier lists where stable,
// ordered output is expected.
func Canonical(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
