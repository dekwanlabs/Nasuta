package retrieval

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/tokenestimate"
	"github.com/dekwanlabs/nasuta/tool"
)

func evidenceHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum)
}

func evidenceFacets(source, kind string) []string {
	facets := domain.ProvidedFacetsFor(source, kind)
	out := make([]string, len(facets))
	for i, facet := range facets {
		out[i] = string(facet)
	}
	return out
}

func evidenceUnitsForCodeDoc(doc codeDoc, content string) []tool.EvidenceUnit {
	if len(doc.evidenceUnits) > 0 {
		return evidence.CloneUnits(doc.evidenceUnits)
	}
	coverage := doc.coverage
	if coverage.Included == 0 {
		coverage.Included = 1
	}
	if doc.source == "code" || doc.source == "codegraph" {
		coverage.Partial = true
		unit, ok := evidence.CodeUnit(
			doc.source, doc.filePath, doc.startLine, doc.endLine, doc.text, "", "",
			doc.evidenceClass, doc.trustTier, coverage,
		)
		if !ok {
			return nil
		}
		return []tool.EvidenceUnit{unit}
	}
	if !coverage.Complete {
		coverage.Partial = true
	}
	return []tool.EvidenceUnit{{
		SourceKind: doc.source, Target: doc.docID,
		Sections:    append([]string(nil), doc.sections...),
		ContentHash: evidenceHash(content), Coverage: coverage,
		Facets:    evidenceFacets(doc.source, doc.kind),
		TrustTier: doc.trustTier, EvidenceClass: doc.evidenceClass,
		TokenCost: tokenestimate.Count(content),
	}}
}

func evidenceUnitForPart(source, target, content string, coverage tool.EvidenceCoverage) tool.EvidenceUnit {
	trustTier := domain.TrustServiceMeta
	evidenceClass := domain.EvidenceClassServiceMeta
	return tool.EvidenceUnit{
		SourceKind: source, Target: target, ContentHash: evidenceHash(content),
		Coverage: coverage, Facets: evidenceFacets(source, ""),
		TrustTier: trustTier, EvidenceClass: evidenceClass,
		TokenCost: tokenestimate.Count(content),
	}
}

func evidenceUnitKey(unit tool.EvidenceUnit) string {
	expanded := evidence.Expand([]tool.EvidenceUnit{unit})
	if len(expanded) == 0 {
		return ""
	}
	key, ok := evidence.UnitKey(expanded[0])
	if !ok {
		return ""
	}
	return key.String()
}

func selectOverviewEvidence(parts []partial, required []domain.EvidenceFacet) []partial {
	if len(parts) == 0 {
		return nil
	}
	remaining := append([]partial(nil), parts...)
	sort.SliceStable(remaining, func(i, j int) bool {
		left, right := overviewPartRank(remaining[i]), overviewPartRank(remaining[j])
		if left.spine != right.spine {
			return left.spine
		}
		if left.trust != right.trust {
			return left.trust > right.trust
		}
		if remaining[i].score != remaining[j].score {
			return remaining[i].score > remaining[j].score
		}
		return left.identity < right.identity
	})

	requiredSet := make(map[string]struct{}, len(required))
	for _, facet := range required {
		requiredSet[string(facet)] = struct{}{}
	}
	covered := make(map[string]struct{}, len(required))
	selected := make([]partial, 0, min(len(parts), 8))
	for i, candidate := range remaining {
		if !overviewPartRank(candidate).spine {
			continue
		}
		selected = append(selected, candidate)
		addCoveredFacets(covered, candidate.units, requiredSet)
		remaining = append(remaining[:i], remaining[i+1:]...)
		break
	}
	for len(remaining) > 0 && len(selected) < 8 {
		best := -1
		bestNew := 0
		bestTrust := -1
		bestScore := -1.0
		bestIdentity := ""
		for i, candidate := range remaining {
			added := newFacetCount(candidate.units, requiredSet, covered)
			rank := overviewPartRank(candidate)
			if added == 0 {
				continue
			}
			if best == -1 || rank.trust > bestTrust ||
				rank.trust == bestTrust && added > bestNew ||
				rank.trust == bestTrust && added == bestNew && candidate.score > bestScore ||
				rank.trust == bestTrust && added == bestNew && candidate.score == bestScore && rank.identity < bestIdentity {
				best, bestNew, bestTrust, bestScore, bestIdentity = i, added, rank.trust, candidate.score, rank.identity
			}
		}
		if best == -1 {
			break
		}
		selected = append(selected, remaining[best])
		addCoveredFacets(covered, remaining[best].units, requiredSet)
		remaining = append(remaining[:best], remaining[best+1:]...)
		if len(covered) == len(requiredSet) {
			break
		}
	}
	return selected
}

type overviewRank struct {
	spine    bool
	trust    int
	identity string
}

func overviewPartRank(part partial) overviewRank {
	rank := overviewRank{}
	for _, unit := range part.units {
		if unit.TrustTier > rank.trust {
			rank.trust = unit.TrustTier
		}
		if rank.identity == "" || evidenceUnitKey(unit) < rank.identity {
			rank.identity = evidenceUnitKey(unit)
		}
		if unit.SourceKind == "runbook" && hasFacet(unit.Facets, string(domain.FacetSystemBoundary)) &&
			hasFacet(unit.Facets, string(domain.FacetCoreFlow)) {
			rank.spine = true
		}
	}
	return rank
}

func newFacetCount(units []tool.EvidenceUnit, required, covered map[string]struct{}) int {
	added := make(map[string]struct{})
	for _, unit := range units {
		for _, facet := range unit.Facets {
			if _, needed := required[facet]; !needed {
				continue
			}
			if _, exists := covered[facet]; exists {
				continue
			}
			added[facet] = struct{}{}
		}
	}
	return len(added)
}

func addCoveredFacets(covered map[string]struct{}, units []tool.EvidenceUnit, required map[string]struct{}) {
	for _, unit := range units {
		for _, facet := range unit.Facets {
			if _, needed := required[facet]; needed {
				covered[facet] = struct{}{}
			}
		}
	}
}

func hasFacet(facets []string, wanted string) bool {
	for _, facet := range facets {
		if facet == wanted {
			return true
		}
	}
	return false
}
