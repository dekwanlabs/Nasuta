package retrieval

import (
	"container/heap"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// WebPassage is one question-relevant section selected from a fetched page.
type WebPassage struct {
	Heading string
	Content string
	Score   float64
}

// WebPassageSelection carries bounded evidence without hiding page coverage.
type WebPassageSelection struct {
	Title         string
	Passages      []WebPassage
	TotalPassages int
	Fallback      string
}

type passageCandidate struct {
	heading string
	content string
	order   int
	length  int
	tf      map[string]int
	score   float64
}

// SelectWebPassages ranks one page locally; external model calls are unnecessary.
func SelectWebPassages(markdown, query string, budget int) WebPassageSelection {
	title, candidates := splitWebPassages(markdown)
	selection := WebPassageSelection{Title: title, TotalPassages: len(candidates), Fallback: markdown}
	queryTokens := tokenize(query)
	if len(candidates) == 0 || len(queryTokens) == 0 || budget <= 0 {
		return selection
	}

	df := make(map[string]int, len(queryTokens))
	querySet := make(map[string]struct{}, len(queryTokens))
	for _, token := range queryTokens {
		querySet[token] = struct{}{}
	}
	totalLength := 0
	for i := range candidates {
		candidate := &candidates[i]
		tokens := tokenizeDocument(candidate.content)
		candidate.length = len(tokens)
		candidate.tf = make(map[string]int, len(queryTokens))
		seen := make(map[string]struct{}, len(queryTokens))
		for _, token := range tokens {
			if _, wanted := querySet[token]; !wanted {
				continue
			}
			candidate.tf[token]++
			if _, ok := seen[token]; !ok {
				seen[token] = struct{}{}
				df[token]++
			}
		}
		totalLength += candidate.length
	}
	avgLength := float64(totalLength) / float64(len(candidates))
	for i := range candidates {
		candidate := &candidates[i]
		candidate.score = bm25PassageScore(queryTokens, candidate, df, len(candidates), avgLength)
		candidate.score += 0.8 * weightedTokenOverlap(queryTokens, tokenize(candidate.heading), df, len(candidates))
		candidate.score += 0.3 * weightedTokenOverlap(queryTokens, tokenize(title), df, len(candidates))
	}
	ranked := topPassageCandidates(candidates, 48)

	selected := make([]WebPassage, 0, 6)
	headingCount := make(map[string]int)
	used := 0
	for _, candidate := range ranked {
		if candidate.score <= 0 || headingCount[candidate.heading] >= 2 {
			continue
		}
		length := utf8.RuneCountInString(candidate.content)
		if used > 0 && used+length > budget {
			continue
		}
		selected = append(selected, WebPassage{Heading: candidate.heading, Content: candidate.content, Score: candidate.score})
		headingCount[candidate.heading]++
		used += length
		if used >= budget || len(selected) == cap(selected) {
			break
		}
	}
	selection.Passages = selected
	return selection
}

func splitWebPassages(markdown string) (string, []passageCandidate) {
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	title, heading := "", ""
	blocks := make([]passageCandidate, 0, len(lines)/2)
	seen := make(map[string]struct{}, len(lines)/2)
	var text strings.Builder
	flush := func() {
		content := strings.TrimSpace(text.String())
		text.Reset()
		if utf8.RuneCountInString(content) < 16 || linkHeavy(content) {
			return
		}
		key := strings.ToLower(strings.Join(strings.Fields(content), " "))
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		for _, part := range splitLongPassage(content, 900) {
			blocks = append(blocks, passageCandidate{heading: heading, content: part, order: len(blocks)})
		}
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			flush()
			value := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
			if title == "" {
				title = value
			}
			heading = value
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		text.WriteString(trimmed)
	}
	flush()
	return title, blocks
}

func splitLongPassage(content string, maxRunes int) []string {
	if utf8.RuneCountInString(content) <= maxRunes {
		return []string{content}
	}
	runes := []rune(content)
	parts := make([]string, 0, len(runes)/maxRunes+1)
	for len(runes) > 0 {
		end := min(maxRunes, len(runes))
		if end < len(runes) {
			for i := end; i > maxRunes/2; i-- {
				if strings.ContainsRune("。！？.!?\n", runes[i-1]) {
					end = i
					break
				}
			}
		}
		parts = append(parts, strings.TrimSpace(string(runes[:end])))
		runes = runes[end:]
	}
	return parts
}

func linkHeavy(content string) bool {
	links := strings.Count(content, "http://") + strings.Count(content, "https://")
	return links >= 4 && links*50 > utf8.RuneCountInString(content)
}

func bm25PassageScore(query []string, candidate *passageCandidate, df map[string]int, total int, avgLength float64) float64 {
	const k1, b = 1.2, 0.75
	length := float64(candidate.length)
	if avgLength == 0 {
		avgLength = 1
	}
	score := 0.0
	for _, token := range query {
		frequency := float64(candidate.tf[token])
		if frequency == 0 {
			continue
		}
		idf := math.Log(1 + (float64(total-df[token])+0.5)/(float64(df[token])+0.5))
		score += idf * frequency * (k1 + 1) / (frequency + k1*(1-b+b*length/avgLength))
	}
	return score
}

type passageCandidateHeap []passageCandidate

func (h passageCandidateHeap) Len() int { return len(h) }
func (h passageCandidateHeap) Less(i, j int) bool {
	if h[i].score == h[j].score {
		return h[i].order > h[j].order
	}
	return h[i].score < h[j].score
}
func (h passageCandidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *passageCandidateHeap) Push(value any) {
	*h = append(*h, value.(passageCandidate))
}
func (h *passageCandidateHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func topPassageCandidates(candidates []passageCandidate, limit int) []passageCandidate {
	if limit <= 0 || len(candidates) == 0 {
		return nil
	}
	if limit > len(candidates) {
		limit = len(candidates)
	}
	top := make(passageCandidateHeap, 0, limit)
	for _, candidate := range candidates {
		if len(top) < limit {
			heap.Push(&top, candidate)
			continue
		}
		worst := top[0]
		if candidate.score > worst.score || (candidate.score == worst.score && candidate.order < worst.order) {
			heap.Pop(&top)
			heap.Push(&top, candidate)
		}
	}
	out := make([]passageCandidate, len(top))
	copy(out, top)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score == out[j].score {
			return out[i].order < out[j].order
		}
		return out[i].score > out[j].score
	})
	return out
}

func weightedTokenOverlap(query, field []string, df map[string]int, total int) float64 {
	fieldSet := make(map[string]struct{}, len(field))
	for _, token := range field {
		fieldSet[token] = struct{}{}
	}
	score := 0.0
	for _, token := range query {
		if _, ok := fieldSet[token]; ok {
			score += math.Log(1 + (float64(total-df[token])+0.5)/(float64(df[token])+0.5))
		}
	}
	return score
}
