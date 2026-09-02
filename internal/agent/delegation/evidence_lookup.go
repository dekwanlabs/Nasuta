package delegation

import (
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	maxVerificationEvidenceSummaryBytes = 2_000
	maxVerificationEvidenceBodyBytes    = 8_000
	maxVerificationEvidenceBodyTotal    = 32_000
)

const evidenceMaterialUnavailable = "Evidence metadata is available, but no authoritative evidence body or bounded observation was admitted for semantic verification."

func buildEvidenceLookup(
	references []string,
	evidence map[string]tool.EvidenceUnit,
	contextIndex map[string]agentapi.ContextBlock,
	observations []agentapi.EvidenceObservation,
) map[string]agentapi.DelegationEvidenceLookup {
	if len(references) == 0 {
		return nil
	}
	lookup := make(map[string]agentapi.DelegationEvidenceLookup, len(references))
	remainingBodyBytes := maxVerificationEvidenceBodyTotal
	for _, reference := range references {
		unit, ok := evidence[reference]
		if !ok {
			continue
		}
		entry := agentapi.DelegationEvidenceLookup{
			Kind:      strings.TrimSpace(unit.SourceKind),
			Reference: reference,
		}
		if observation, ok := matchingEvidenceObservation(unit, observations); ok {
			entry.Summary = truncateText(
				strings.TrimSpace(observation.Summary),
				maxVerificationEvidenceSummaryBytes,
			)
		}
		if block, ok := contextBlockForEvidence(reference, unit, contextIndex); ok && remainingBodyBytes > 0 {
			bodyLimit := min(maxVerificationEvidenceBodyBytes, remainingBodyBytes)
			entry.Body = truncateText(strings.TrimSpace(block.Content), bodyLimit)
			remainingBodyBytes -= len(entry.Body)
			if entry.Summary == "" {
				entry.Summary = truncateText(entry.Body, maxVerificationEvidenceSummaryBytes)
			}
		}
		if entry.Summary == "" {
			entry.Summary = evidenceMaterialUnavailable
		}
		lookup[reference] = entry
	}
	if len(lookup) == 0 {
		return nil
	}
	return lookup
}

func evidenceMaterialAvailable(
	reference string,
	unit tool.EvidenceUnit,
	contextIndex map[string]agentapi.ContextBlock,
	observations []agentapi.EvidenceObservation,
) bool {
	if block, ok := contextBlockForEvidence(reference, unit, contextIndex); ok &&
		strings.TrimSpace(block.Content) != "" {
		return true
	}
	observation, ok := matchingEvidenceObservation(unit, observations)
	return ok && strings.TrimSpace(observation.Summary) != ""
}

func contextBlockForEvidence(
	reference string,
	unit tool.EvidenceUnit,
	contextIndex map[string]agentapi.ContextBlock,
) (agentapi.ContextBlock, bool) {
	if len(contextIndex) == 0 {
		return agentapi.ContextBlock{}, false
	}
	if block, ok := contextIndex[reference]; ok {
		return block, true
	}
	for _, alias := range evidenceAliases(unit) {
		if block, ok := contextIndex[alias]; ok {
			return block, true
		}
	}
	seen := make(map[string]struct{}, len(contextIndex))
	for _, block := range contextIndex {
		key := block.ContentHash + "\x00" + block.Source + "\x00" + block.Title
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if contextBlockMatchesEvidence(block, reference, unit) {
			return block, true
		}
	}
	return agentapi.ContextBlock{}, false
}

func contextBlockMatchesEvidence(
	block agentapi.ContextBlock,
	reference string,
	unit tool.EvidenceUnit,
) bool {
	if reference != "" && reference == strings.TrimSpace(block.ContentHash) {
		return true
	}
	for _, candidate := range block.Evidence {
		if evidenceUnitsShareIdentity(unit, candidate) {
			return true
		}
		for _, alias := range evidenceAliases(candidate) {
			if alias == reference {
				return true
			}
		}
	}
	for _, candidate := range block.References {
		target := strings.TrimSpace(candidate.Target)
		if target == reference || target != "" && target == strings.TrimSpace(unit.Target) {
			return true
		}
		if candidate.Type != "" && target != "" && candidate.Type+":"+target == reference {
			return true
		}
	}
	return false
}

func matchingEvidenceObservation(
	unit tool.EvidenceUnit,
	observations []agentapi.EvidenceObservation,
) (agentapi.EvidenceObservation, bool) {
	for _, observation := range observations {
		if evidenceObservationMatchesUnit(observation, unit) {
			return observation, true
		}
	}
	return agentapi.EvidenceObservation{}, false
}

func evidenceObservationMatchesUnit(
	observation agentapi.EvidenceObservation,
	unit tool.EvidenceUnit,
) bool {
	if strings.TrimSpace(observation.SourceKind) != strings.TrimSpace(unit.SourceKind) ||
		strings.TrimSpace(observation.Target) != strings.TrimSpace(unit.Target) {
		return false
	}
	if observation.Version != "" && unit.Version != "" && observation.Version != unit.Version {
		return false
	}
	if observation.TimeRange != "" && unit.TimeRange != "" && observation.TimeRange != unit.TimeRange {
		return false
	}
	if observation.Section == "" || len(unit.Sections) == 0 {
		return true
	}
	for _, section := range unit.Sections {
		if observation.Section == section {
			return true
		}
	}
	return false
}

func evidenceUnitsShareIdentity(left, right tool.EvidenceUnit) bool {
	if strings.TrimSpace(left.SourceKind) != strings.TrimSpace(right.SourceKind) ||
		strings.TrimSpace(left.Target) != strings.TrimSpace(right.Target) {
		return false
	}
	if left.Version != "" && right.Version != "" && left.Version != right.Version {
		return false
	}
	if left.TimeRange != "" && right.TimeRange != "" && left.TimeRange != right.TimeRange {
		return false
	}
	return true
}

func cloneEvidenceObservations(
	observations []agentapi.EvidenceObservation,
) []agentapi.EvidenceObservation {
	if len(observations) == 0 {
		return nil
	}
	out := make([]agentapi.EvidenceObservation, len(observations))
	for index, observation := range observations {
		observation.Facets = append([]string(nil), observation.Facets...)
		out[index] = observation
	}
	return out
}

func evidenceLookupDebugSummary(
	lookup map[string]agentapi.DelegationEvidenceLookup,
) string {
	available := 0
	for _, entry := range lookup {
		if entry.Body != "" || entry.Summary != evidenceMaterialUnavailable {
			available++
		}
	}
	return fmt.Sprintf("%d/%d evidence references include readable material", available, len(lookup))
}
