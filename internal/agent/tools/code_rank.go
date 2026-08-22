package tools

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
)

const (
	codeBackendWeight  = 0.65
	codeLexicalWeight  = 0.25
	codeIdentityWeight = 0.10
	codeNoveltyWeight  = 0.15
	maxCodeQueryTerms  = 32
)

type rankedCodeHit struct {
	hit              semantic.Hit
	rankScore        float64
	lexicalCoverage  float64
	identityCoverage float64
	coveredQueryUnit map[string]struct{}
}

type codeRankCandidate struct {
	hit      semantic.Hit
	covered  map[string]struct{}
	identity map[string]struct{}
}

func rankCodeHits(query string, hits []semantic.Hit, limit int) []rankedCodeHit {
	if len(hits) == 0 || limit <= 0 {
		return nil
	}
	terms := codeQueryTerms(query)
	candidates, docFreq := collectCodeRankSignals(hits, terms)
	termWeights, totalTermWeight := codeTermWeights(docFreq, len(candidates))

	maxScore := float64(hits[0].Score)
	for _, hit := range hits[1:] {
		if score := float64(hit.Score); score > maxScore {
			maxScore = score
		}
	}
	if maxScore <= 0 {
		maxScore = 1
	}

	ranked := make([]rankedCodeHit, 0, len(candidates))
	for _, candidate := range candidates {
		lexicalCoverage := weightedCoverage(candidate.covered, termWeights, totalTermWeight)
		identityCoverage := weightedCoverage(candidate.identity, termWeights, totalTermWeight)
		if !admitCodeRankCandidate(candidate, lexicalCoverage, identityCoverage, len(terms)) {
			continue
		}
		baseScore := float64(candidate.hit.Score) / maxScore
		ranked = append(ranked, rankedCodeHit{
			hit:              candidate.hit,
			rankScore:        baseScore*codeBackendWeight + lexicalCoverage*codeLexicalWeight + identityCoverage*codeIdentityWeight,
			lexicalCoverage:  lexicalCoverage,
			identityCoverage: identityCoverage,
			coveredQueryUnit: candidate.covered,
		})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].rankScore == ranked[j].rankScore {
			return ranked[i].hit.Score > ranked[j].hit.Score
		}
		return ranked[i].rankScore > ranked[j].rankScore
	})
	return selectCodeCoverage(ranked, termWeights, totalTermWeight, limit)
}

func admitCodeRankCandidate(candidate codeRankCandidate, lexicalCoverage, identityCoverage float64, queryTerms int) bool {
	if candidate.hit.DenseRank > 0 || candidate.hit.SparseRank == 0 {
		return true
	}
	if queryTerms <= 1 || identityCoverage > 0 || len(candidate.covered) >= 2 {
		return true
	}
	return lexicalCoverage >= 0.5
}

func codeQueryTerms(query string) []string {
	raw := make([]string, 0, maxCodeQueryTerms)
	raw = append(raw, retrieval.ExtractTechTerms(query)...)
	raw = append(raw, retrieval.TokenizeQuery(query)...)
	seen := make(map[string]struct{}, min(len(raw), maxCodeQueryTerms))
	terms := make([]string, 0, min(len(raw), maxCodeQueryTerms))
	for _, term := range raw {
		term = normalizeCodeText(term)
		if term == "" {
			continue
		}
		if _, duplicate := seen[term]; duplicate {
			continue
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
		if len(terms) == maxCodeQueryTerms {
			break
		}
	}
	return terms
}

func collectCodeRankSignals(hits []semantic.Hit, terms []string) ([]codeRankCandidate, map[string]int) {
	candidates := make([]codeRankCandidate, 0, len(hits))
	docFreq := make(map[string]int, len(terms))
	for _, term := range terms {
		docFreq[term] = 0
	}
	for _, hit := range hits {
		content := normalizeCodeText(payloadString(hit.Metadata, "text"))
		identity := normalizeCodeText(payloadString(hit.Metadata, "path") + "\n" + payloadString(hit.Metadata, "repo"))
		covered := make(map[string]struct{}, len(terms))
		identityCovered := make(map[string]struct{}, len(terms))
		for _, term := range terms {
			inIdentity := strings.Contains(identity, term)
			if !inIdentity && !strings.Contains(content, term) {
				continue
			}
			covered[term] = struct{}{}
			docFreq[term]++
			if inIdentity {
				identityCovered[term] = struct{}{}
			}
		}
		candidates = append(candidates, codeRankCandidate{
			hit: hit, covered: covered, identity: identityCovered,
		})
	}
	return candidates, docFreq
}

func codeTermWeights(docFreq map[string]int, candidateCount int) (map[string]float64, float64) {
	weights := make(map[string]float64, len(docFreq))
	total := 0.0
	for term, frequency := range docFreq {
		weight := math.Log(float64(candidateCount+1)/float64(frequency+1)) + 1
		weights[term] = weight
		total += weight
	}
	return weights, total
}

func weightedCoverage(covered map[string]struct{}, weights map[string]float64, total float64) float64 {
	if total == 0 {
		return 0
	}
	matched := 0.0
	for term := range covered {
		matched += weights[term]
	}
	return matched / total
}

func selectCodeCoverage(ranked []rankedCodeHit, weights map[string]float64, totalWeight float64, limit int) []rankedCodeHit {
	count := min(limit, len(ranked))
	if count == 0 {
		return nil
	}
	if count == 1 || totalWeight == 0 {
		return ranked[:count]
	}

	selected := make([]rankedCodeHit, 0, count)
	selected = append(selected, ranked[0])
	used := make([]bool, len(ranked))
	used[0] = true
	covered := make(map[string]struct{}, len(weights))
	addCoveredTerms(covered, ranked[0].coveredQueryUnit, weights)

	for len(selected) < count {
		bestIndex := -1
		bestScore := -1.0
		for i := range ranked {
			if used[i] {
				continue
			}
			newWeight := 0.0
			for term := range ranked[i].coveredQueryUnit {
				if _, seen := covered[term]; !seen {
					newWeight += weights[term]
				}
			}
			score := ranked[i].rankScore + codeNoveltyWeight*newWeight/totalWeight
			if score > bestScore {
				bestIndex, bestScore = i, score
			}
		}
		if bestIndex < 0 {
			break
		}
		used[bestIndex] = true
		selected = append(selected, ranked[bestIndex])
		addCoveredTerms(covered, ranked[bestIndex].coveredQueryUnit, weights)
	}
	return selected
}

func addCoveredTerms(dst, src map[string]struct{}, weights map[string]float64) {
	for term := range src {
		if _, active := weights[term]; active {
			dst[term] = struct{}{}
		}
	}
}

func normalizeCodeText(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}
