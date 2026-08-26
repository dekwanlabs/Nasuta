package investigation

import (
	"sort"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
)

const (
	defaultEvidenceSummaryTokens = 256
	defaultEvidenceContextTokens = 6000
	defaultEvidenceBundleTokens  = 8000
)

// EvidenceContextBudget bounds how much evidence text may enter a Verifier or
// Composer model input. It is a sub-budget of InputTokens, not a new total
// token setting.
type EvidenceContextBudget struct {
	// MaxSummaryTokens limits each evidence summary. Zero falls back to the
	// default summary limit.
	MaxSummaryTokens int
	// MaxContextTokens limits the deduplicated evidence context in one input.
	// Zero falls back to the default context limit.
	MaxContextTokens int64
	// MaxBundleTokens limits the complete verified bundle sent to the composer.
	// Zero falls back to the default bundle limit.
	MaxBundleTokens int64
}

func (budget EvidenceContextBudget) effective() (summaryTokens int, contextTokens, bundleTokens int64) {
	summaryTokens = budget.MaxSummaryTokens
	if summaryTokens <= 0 {
		summaryTokens = defaultEvidenceSummaryTokens
	}
	contextTokens = budget.MaxContextTokens
	if contextTokens <= 0 {
		contextTokens = defaultEvidenceContextTokens
	}
	bundleTokens = budget.MaxBundleTokens
	if bundleTokens <= 0 {
		bundleTokens = defaultEvidenceBundleTokens
	}
	return summaryTokens, contextTokens, bundleTokens
}

type evidenceSummaryView struct {
	Kind        string                     `json:"kind"`
	Reference   string                     `json:"reference"`
	Summary     string                     `json:"summary"`
	ContentHash string                     `json:"content_hash,omitempty"`
	Identity    *agentapi.EvidenceIdentity `json:"identity,omitempty"`
}

type evidenceOmissionView struct {
	EvidenceID string `json:"evidence_id"`
	Reason     string `json:"reason"`
}

type evidenceContextView struct {
	BudgetTokens  int64 `json:"budget_tokens"`
	UsedTokens    int   `json:"used_tokens"`
	OmittedTokens int   `json:"omitted_tokens,omitempty"`
}

type evidenceContextResult struct {
	selected  []EvidenceUnit
	lookup    map[string]evidenceSummaryView
	context   evidenceContextView
	omissions []evidenceOmissionView
}

func buildEvidenceContext(
	units []EvidenceUnit,
	claims []VerifiedClaim,
	contract InvestigationContract,
	budget EvidenceContextBudget,
) evidenceContextResult {
	maxSummaryTokens, maxContextTokens, _ := budget.effective()
	required := requiredGoalIDs(contract)
	requiredFacets := requiredGoalFacets(contract)
	citations := make(map[string]int, len(units))
	requiredEvidence := make(map[string]bool, len(units))
	for _, claim := range claims {
		claimRequired := strings.TrimSpace(claim.GoalID) != "" && goalRequired(claim.GoalID, required)
		for _, ref := range claim.EvidenceRefs {
			citations[ref.EvidenceID]++
			if claimRequired {
				requiredEvidence[ref.EvidenceID] = true
			}
		}
	}

	ordered := make([]EvidenceUnit, 0, len(units))
	seen := make(map[string]struct{}, len(units))
	for _, unit := range units {
		if _, duplicate := seen[unit.ID]; duplicate {
			continue
		}
		seen[unit.ID] = struct{}{}
		ordered = append(ordered, unit)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i], ordered[j]
		leftRequired := requiredEvidence[left.ID] || evidenceMatchesRequiredFacet(left, requiredFacets)
		rightRequired := requiredEvidence[right.ID] || evidenceMatchesRequiredFacet(right, requiredFacets)
		if leftRequired != rightRequired {
			return leftRequired
		}
		if citations[left.ID] != citations[right.ID] {
			return citations[left.ID] > citations[right.ID]
		}
		if left.TrustTier != right.TrustTier {
			return left.TrustTier > right.TrustTier
		}
		return left.ID < right.ID
	})

	lookup := make(map[string]evidenceSummaryView, len(ordered))
	selected := make([]EvidenceUnit, 0, len(ordered))
	omissions := make([]evidenceOmissionView, 0)
	usedTokens := 0
	omittedTokens := 0
	for _, unit := range ordered {
		identity := evidenceIdentityForUnit(unit)
		content := strings.TrimSpace(unit.Content)
		if !isReadableEvidenceContent(content) {
			// Identity-only seed evidence remains useful for traceability, but its
			// hash is not evidence text and must never become model input.
			omissions = append(omissions, evidenceOmissionView{EvidenceID: unit.ID, Reason: "evidence_content_unavailable"})
			continue
		}
		originalTokens := tooloutput.EstimateTokens(content)
		available := maxSummaryTokens
		if maxContextTokens > 0 {
			remaining := int(maxContextTokens) - usedTokens
			if remaining <= 0 {
				omittedTokens += originalTokens
				omissions = append(omissions, evidenceOmissionView{EvidenceID: unit.ID, Reason: "evidence_context_budget"})
				continue
			}
			if available > remaining {
				available = remaining
			}
		}
		summary := evidenceSummaryForUnit(unit, available)
		cost := tooloutput.EstimateTokens(summary)
		if summary == "" || (maxContextTokens > 0 && usedTokens+cost > int(maxContextTokens)) {
			omittedTokens += originalTokens
			omissions = append(omissions, evidenceOmissionView{EvidenceID: unit.ID, Reason: "evidence_context_budget"})
			continue
		}
		usedTokens += cost
		if originalTokens > cost {
			omittedTokens += originalTokens - cost
		}
		lookup[unit.ID] = evidenceSummaryView{
			Kind:        firstNonEmpty(unit.SourceKind, fallbackEvidenceSourceKind),
			Reference:   firstNonEmpty(unit.Target, fallbackEvidenceTarget),
			Summary:     summary,
			ContentHash: unit.ContentHash,
			Identity:    &identity,
		}
		selected = append(selected, unit)
	}

	return evidenceContextResult{
		selected: selected,
		lookup:   lookup,
		context: evidenceContextView{
			BudgetTokens:  maxContextTokens,
			UsedTokens:    usedTokens,
			OmittedTokens: omittedTokens,
		},
		omissions: omissions,
	}
}

func requiredGoalFacets(contract InvestigationContract) map[string]struct{} {
	facets := make(map[string]struct{})
	for _, goal := range contract.EvidenceGoals {
		if !goal.Required {
			continue
		}
		for _, facet := range goal.Facets {
			if facet = strings.TrimSpace(facet); facet != "" {
				facets[facet] = struct{}{}
			}
		}
		if facet := strings.TrimSpace(goal.Kind); facet != "" {
			facets[facet] = struct{}{}
		}
	}
	return facets
}

func evidenceMatchesRequiredFacet(unit EvidenceUnit, requiredFacets map[string]struct{}) bool {
	for _, facet := range unit.Facets {
		if _, ok := requiredFacets[strings.TrimSpace(facet)]; ok {
			return true
		}
	}
	return false
}

func evidenceSummaryForUnit(unit EvidenceUnit, maxTokens int) string {
	return tooloutput.TruncateContent(strings.TrimSpace(unit.Content), maxTokens)
}

func evidenceIdentityForUnit(unit EvidenceUnit) agentapi.EvidenceIdentity {
	return agentapi.EvidenceIdentity{
		SourceKind: firstNonEmpty(unit.SourceKind, fallbackEvidenceSourceKind),
		Target:     firstNonEmpty(unit.Target, fallbackEvidenceTarget),
		Section:    unit.Section,
		Version:    unit.Version,
		TimeRange:  unit.TimeRange,
	}
}
