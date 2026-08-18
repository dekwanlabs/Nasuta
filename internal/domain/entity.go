package domain

import (
	"strings"
	"unicode"
)

const MaxCanonicalEntities = 8

// EntitySpec carries the planner's bounded description of one comparison
// subject. ID is the stable join key; the other fields preserve disambiguation
// context without making downstream code infer a role from prose.
type EntitySpec struct {
	ID      string
	Label   string
	Role    string
	Aliases []string
}

// CanonicalEntitySpecs normalizes planner-provided subjects and merges
// duplicate aliases onto the first occurrence.
func CanonicalEntitySpecs(specs []EntitySpec) []EntitySpec {
	result := make([]EntitySpec, 0, min(len(specs), MaxCanonicalEntities))
	byID := make(map[string]int, min(len(specs), MaxCanonicalEntities))
	for _, spec := range specs {
		id := canonicalEntityID(spec.ID)
		if id == "" {
			id = canonicalEntityID(spec.Label)
		}
		if id == "" {
			continue
		}
		index, exists := byID[id]
		if !exists {
			if len(result) == MaxCanonicalEntities {
				break
			}
			byID[id] = len(result)
			result = append(result, EntitySpec{ID: id})
			index = len(result) - 1
		}
		current := &result[index]
		if current.Label == "" {
			current.Label = strings.TrimSpace(spec.Label)
		}
		if current.Role == "" {
			current.Role = strings.TrimSpace(spec.Role)
		}
		current.Aliases = mergeEntityAliases(current.Aliases, spec.Aliases, current.Label)
	}
	return result
}

func mergeEntityAliases(existing, candidates []string, label string) []string {
	seen := make(map[string]struct{}, len(existing)+len(candidates)+1)
	aliases := make([]string, 0, len(existing)+len(candidates))
	for _, candidate := range append(append([]string(nil), existing...), candidates...) {
		value := strings.TrimSpace(candidate)
		key := strings.ToLower(value)
		if value == "" || key == strings.ToLower(strings.TrimSpace(label)) {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		aliases = append(aliases, value)
	}
	return aliases
}

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
