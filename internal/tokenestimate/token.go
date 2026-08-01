package tokenestimate

const (
	tokenUnits         = 30
	asciiTokenUnits    = 11
	nonASCIITokenUnits = 66
)

// Count provides one conservative, provider-independent input estimate.
func Count(value string) int {
	units := 0
	for _, r := range value {
		units += runeUnits(r)
	}
	return (units + tokenUnits - 1) / tokenUnits
}

// Prefix returns the longest UTF-8 prefix within maxTokens.
func Prefix(value string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	remaining := maxTokens * tokenUnits
	for i, r := range value {
		cost := runeUnits(r)
		if cost > remaining {
			return value[:i]
		}
		remaining -= cost
	}
	return value
}

func runeUnits(r rune) int {
	if r <= 127 {
		return asciiTokenUnits
	}
	return nonASCIITokenUnits
}
