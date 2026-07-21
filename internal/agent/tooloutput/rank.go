package tooloutput

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

type relevanceSignals struct {
	exact []string
	terms map[string]struct{}
}

func rankChunks(chunks []chunk, question string, arguments map[string]any) []int {
	signals := buildSignals(question, arguments)
	ranked := make([]int, len(chunks))
	scores := make([]int, len(chunks))
	hasMatch := false
	for i := range chunks {
		scores[i] = scoreChunk(chunks[i], signals)
		hasMatch = hasMatch || scores[i] > 0
		ranked[i] = i
	}
	if !hasMatch {
		return fallbackOrder(len(chunks))
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		left := chunks[ranked[i]]
		right := chunks[ranked[j]]
		leftScore := scores[ranked[i]]
		rightScore := scores[ranked[j]]
		if leftScore == rightScore {
			return left.ordinal < right.ordinal
		}
		return leftScore > rightScore
	})
	return ranked
}

func buildSignals(question string, arguments map[string]any) relevanceSignals {
	signals := relevanceSignals{terms: make(map[string]struct{})}
	addTerms(signals.terms, question)
	collectArgumentSignals(arguments, &signals)
	return signals
}

func collectArgumentSignals(value any, signals *relevanceSignals) {
	switch typed := value.(type) {
	case map[string]any:
		for _, item := range typed {
			collectArgumentSignals(item, signals)
		}
	case []any:
		for _, item := range typed {
			collectArgumentSignals(item, signals)
		}
	case string:
		addExactSignal(typed, signals)
	case bool, float64, float32, int, int64, int32, uint, uint64:
		addExactSignal(fmt.Sprint(typed), signals)
	}
}

func addExactSignal(value string, signals *relevanceSignals) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return
	}
	signals.exact = append(signals.exact, value)
	addTerms(signals.terms, value)
}

func scoreChunk(candidate chunk, signals relevanceSignals) int {
	haystack := strings.ToLower(candidate.searchableText())
	score := 0
	for _, exact := range signals.exact {
		if strings.Contains(haystack, exact) {
			score += 1000
		}
	}
	chunkTerms := make(map[string]struct{})
	addTerms(chunkTerms, haystack)
	for term := range chunkTerms {
		if _, ok := signals.terms[term]; ok {
			score += 10
		}
	}
	return score
}

func addTerms(dst map[string]struct{}, value string) {
	var ascii strings.Builder
	var nonASCII []rune
	flushASCII := func() {
		if ascii.Len() >= 2 {
			dst[ascii.String()] = struct{}{}
		}
		ascii.Reset()
	}
	flushNonASCII := func() {
		for size := 2; size <= 3; size++ {
			for i := 0; i+size <= len(nonASCII); i++ {
				dst[string(nonASCII[i:i+size])] = struct{}{}
			}
		}
		nonASCII = nonASCII[:0]
	}

	for _, r := range strings.ToLower(value) {
		switch {
		case r <= 127 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'):
			flushNonASCII()
			ascii.WriteRune(r)
		case r > 127 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			flushASCII()
			nonASCII = append(nonASCII, r)
		default:
			flushASCII()
			flushNonASCII()
		}
	}
	flushASCII()
	flushNonASCII()
}

func fallbackOrder(size int) []int {
	if size <= 0 {
		return nil
	}
	order := make([]int, 0, size)
	seen := make(map[int]struct{}, size)
	add := func(index int) {
		if index < 0 || index >= size {
			return
		}
		if _, ok := seen[index]; ok {
			return
		}
		seen[index] = struct{}{}
		order = append(order, index)
	}
	add(0)
	add(size - 1)
	type interval struct{ left, right int }
	queue := []interval{{left: 1, right: size - 2}}
	for cursor := 0; cursor < len(queue); cursor++ {
		current := queue[cursor]
		if current.left > current.right {
			continue
		}
		middle := current.left + (current.right-current.left)/2
		add(middle)
		queue = append(queue,
			interval{left: current.left, right: middle - 1},
			interval{left: middle + 1, right: current.right},
		)
	}
	return order
}
