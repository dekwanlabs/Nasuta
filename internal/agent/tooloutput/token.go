package tooloutput

import (
	"fmt"
	"strings"
)

const (
	tokenUnits         = 30
	asciiTokenUnits    = 11
	nonASCIITokenUnits = 66
)

// EstimateTokens provides one conservative, provider-independent input estimate.
func EstimateTokens(value string) int {
	units := 0
	for _, r := range value {
		units += runeTokenUnits(r)
	}
	return (units + tokenUnits - 1) / tokenUnits
}

// Truncate preserves both evidence boundaries when structured compression cannot proceed.
func Truncate(value string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	originalTokens := EstimateTokens(value)
	return truncate(value, maxTokens, originalTokens)
}

func truncate(value string, maxTokens, originalTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	if originalTokens <= maxTokens {
		return value
	}

	marker := fmt.Sprintf(
		"\n... [tool output truncated: original ~%d tokens, %d lines] ...\n",
		originalTokens,
		strings.Count(value, "\n")+1,
	)
	markerTokens := EstimateTokens(marker)
	if markerTokens > maxTokens {
		return truncateWithoutMarker(value, maxTokens)
	}
	contentTokens := maxTokens - markerTokens
	headTokens := contentTokens / 2
	tailTokens := contentTokens - headTokens
	runes := []rune(value)
	headEnd := prefixEnd(runes, headTokens)
	tailStart := suffixStart(runes, tailTokens)
	if tailStart < headEnd {
		tailStart = headEnd
	}
	return string(runes[:headEnd]) + marker + string(runes[tailStart:])
}

func truncateWithoutMarker(value string, maxTokens int) string {
	headTokens := maxTokens / 2
	tailTokens := maxTokens - headTokens
	runes := []rune(value)
	headEnd := prefixEnd(runes, headTokens)
	tailStart := suffixStart(runes, tailTokens)
	if tailStart < headEnd {
		tailStart = headEnd
	}
	return string(runes[:headEnd]) + string(runes[tailStart:])
}

func prefixEnd(runes []rune, maxTokens int) int {
	remainingUnits := maxTokens * tokenUnits
	for i, r := range runes {
		cost := runeTokenUnits(r)
		if cost > remainingUnits {
			return i
		}
		remainingUnits -= cost
	}
	return len(runes)
}

func suffixStart(runes []rune, maxTokens int) int {
	remainingUnits := maxTokens * tokenUnits
	for i := len(runes) - 1; i >= 0; i-- {
		cost := runeTokenUnits(runes[i])
		if cost > remainingUnits {
			return i + 1
		}
		remainingUnits -= cost
	}
	return 0
}

func runeTokenUnits(r rune) int {
	if r <= 127 {
		return asciiTokenUnits
	}
	return nonASCIITokenUnits
}
