package execution

import (
	"regexp"
	"strings"
)

var entityPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+\b`),
	regexp.MustCompile(`\b\d{15,20}\b`),
	regexp.MustCompile(`(?i)\b[0-9a-f]{12,64}(?:\.\d+\.\d+)?\b`),
	regexp.MustCompile(`(?i)\b[a-z0-9]{8,}-[a-z0-9-]{8,}:[a-z0-9:]+\b`),
}

// hasEntityConflict prevents an explicit entity switch from
// turning unrelated history into retrieval evidence.
func hasEntityConflict(question, prior string) bool {
	if strings.TrimSpace(question) == "" || strings.TrimSpace(prior) == "" {
		return false
	}
	for _, pattern := range entityPatterns {
		current := entityMatches(pattern, question)
		previous := entityMatches(pattern, prior)
		if len(current) == 0 || len(previous) == 0 {
			continue
		}
		for value := range current {
			if _, ok := previous[value]; ok {
				return false
			}
		}
		return true
	}
	return false
}

func entityMatches(pattern *regexp.Regexp, text string) map[string]struct{} {
	matches := pattern.FindAllString(strings.ToLower(text), -1)
	out := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		out[match] = struct{}{}
	}
	return out
}
