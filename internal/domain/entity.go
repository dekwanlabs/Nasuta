package domain

import (
	"strings"
	"unicode"
)

const MaxCanonicalEntities = 8

// CanonicalEntityIDs establishes the stable identity used across QA boundaries.
func CanonicalEntityIDs(candidates []string) []string {
	entities := make([]string, 0, min(len(candidates), MaxCanonicalEntities))
	seen := make(map[string]struct{}, min(len(candidates), MaxCanonicalEntities))
	for _, candidate := range candidates {
		entity := canonicalEntityID(candidate)
		if entity == "" {
			continue
		}
		if _, exists := seen[entity]; exists {
			continue
		}
		seen[entity] = struct{}{}
		entities = append(entities, entity)
		if len(entities) == MaxCanonicalEntities {
			break
		}
	}
	return entities
}

// CanonicalQuestionEntities extracts entity-shaped literals using the same identity rules.
func CanonicalQuestionEntities(question string) []string {
	candidates := make([]string, 0, MaxCanonicalEntities)
	var token strings.Builder
	flush := func() {
		value := token.String()
		token.Reset()
		if isQuestionEntity(value) {
			candidates = append(candidates, value)
		}
	}
	for _, r := range question {
		if unicode.IsLetter(r) || unicode.IsDigit(r) ||
			strings.ContainsRune("._/:@-$", r) {
			token.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return CanonicalEntityIDs(candidates)
}

func canonicalEntityID(candidate string) string {
	value := strings.TrimSpace(candidate)
	value = strings.Trim(value, "`'\"")
	value = strings.TrimSpace(value)
	value = strings.TrimRight(value, ".,;!?，。；！？")
	value = strings.TrimSuffix(value, "()")
	return strings.ToLower(strings.TrimSpace(value))
}

func isQuestionEntity(value string) bool {
	value = strings.TrimSpace(value)
	if len([]rune(value)) < 3 {
		return false
	}
	hasDigit := false
	hasSeparator := false
	hasLower := false
	hasUpper := false
	upperAfterFirst := false
	index := 0
	for _, r := range value {
		hasDigit = hasDigit || unicode.IsDigit(r)
		hasSeparator = hasSeparator || strings.ContainsRune("._/:@-$", r)
		hasLower = hasLower || unicode.IsLower(r)
		hasUpper = hasUpper || unicode.IsUpper(r)
		upperAfterFirst = upperAfterFirst || index > 0 && unicode.IsUpper(r)
		index++
	}
	return hasDigit || hasSeparator || hasLower && upperAfterFirst ||
		hasUpper && !hasLower
}
