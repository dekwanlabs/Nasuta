package evidence

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/tokenestimate"
	"github.com/dekwanlabs/nasuta/tool"
)

// CodeUnit builds the canonical identity shared by retrieval and source-owned
// code tool adapters. The file plus line range identifies the independently
// coverable source fragment; the hash is derived from the source body only.
func CodeUnit(
	sourceKind string,
	path string,
	startLine int,
	endLine int,
	content string,
	lang string,
	repo string,
	evidenceClass string,
	trustTier int,
	coverage tool.EvidenceCoverage,
) (tool.EvidenceUnit, bool) {
	path = strings.TrimSpace(path)
	content = strings.TrimSpace(content)
	if path == "" || content == "" {
		return tool.EvidenceUnit{}, false
	}
	if sourceKind != "code" && sourceKind != "codegraph" {
		return tool.EvidenceUnit{}, false
	}
	sections := []string(nil)
	if startLine > 0 {
		if endLine < startLine {
			endLine = startLine
		}
		sections = []string{fmt.Sprintf("L%d-L%d", startLine, endLine)}
	}
	if coverage.Included == 0 {
		coverage.Included = 1
	}
	if evidenceClass == "" || trustTier <= 0 {
		evidenceClass, trustTier = domain.EvidenceForCodeChunk(lang, repo)
	}
	return tool.EvidenceUnit{
		SourceKind:    sourceKind,
		Target:        path,
		Sections:      sections,
		ContentHash:   hashEvidenceContent(content),
		Coverage:      coverage,
		Facets:        facetValues(domain.ProvidedFacetsFor(sourceKind, "")),
		TrustTier:     trustTier,
		EvidenceClass: evidenceClass,
		TokenCost:     tokenestimate.Count(content),
	}, true
}

// RunbookChunkUnit builds one independently traceable runbook fragment. Chunk
// identity and source text are stable across query scores, result ordering, and
// different retrieval subsets of the same document.
func RunbookChunkUnit(
	docID string,
	chunkIndex int,
	content string,
	docKind string,
	evidenceClass string,
	trustTier int,
	coverage tool.EvidenceCoverage,
) (tool.EvidenceUnit, bool) {
	docID = strings.TrimSpace(docID)
	content = strings.TrimSpace(content)
	if docID == "" || content == "" || chunkIndex < 0 {
		return tool.EvidenceUnit{}, false
	}
	if coverage.Included == 0 {
		coverage.Included = 1
	}
	if !coverage.Complete {
		coverage.Partial = true
	}
	if evidenceClass == "" || trustTier <= 0 {
		evidenceClass, trustTier = domain.EvidenceForRunbookScope(docKind)
	}
	return tool.EvidenceUnit{
		SourceKind:    "runbook",
		Target:        docID,
		Sections:      []string{fmt.Sprintf("chunk:%d", chunkIndex)},
		ContentHash:   hashEvidenceContent(content),
		Coverage:      coverage,
		Facets:        facetValues(domain.ProvidedFacetsFor("runbook", docKind)),
		TrustTier:     trustTier,
		EvidenceClass: evidenceClass,
		TokenCost:     tokenestimate.Count(content),
	}, true
}

// WebPageUnit promotes one fetched page into canonical external evidence.
func WebPageUnit(
	rawURL string,
	title string,
	content string,
	coverage tool.EvidenceCoverage,
) (tool.EvidenceUnit, bool) {
	rawURL = strings.TrimSpace(rawURL)
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	if rawURL == "" || content == "" {
		return tool.EvidenceUnit{}, false
	}
	if coverage.Included == 0 {
		coverage.Included = 1
	}
	if !coverage.Complete {
		coverage.Partial = true
	}
	var sections []string
	if title != "" {
		sections = []string{title}
	}
	return tool.EvidenceUnit{
		SourceKind:    "web",
		Target:        rawURL,
		Sections:      sections,
		ContentHash:   hashEvidenceContent(content),
		Coverage:      coverage,
		TrustTier:     domain.TrustUnknown,
		EvidenceClass: domain.EvidenceClassUnknown,
		TokenCost:     tokenestimate.Count(content),
	}, true
}

// ServiceMetadataUnit builds the canonical service-card identity used by both
// retrieval and the get_service adapter. Only fields available in both paths
// participate in the hash, preventing false conflicts from presentation-only
// metadata.
func ServiceMetadataUnit(name, layer, language, summary string) (tool.EvidenceUnit, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return tool.EvidenceUnit{}, false
	}
	content := marshalCanonical(struct {
		Name     string `json:"name"`
		Layer    string `json:"layer,omitempty"`
		Language string `json:"language,omitempty"`
		Summary  string `json:"summary,omitempty"`
	}{Name: name, Layer: layer, Language: language, Summary: summary})
	return tool.EvidenceUnit{
		SourceKind:    "service",
		Target:        name,
		ContentHash:   hashEvidenceContent(content),
		Coverage:      tool.EvidenceCoverage{Complete: true, Included: 1},
		Facets:        facetValues(domain.ProvidedFacetsFor("service", "")),
		TrustTier:     domain.TrustServiceMeta,
		EvidenceClass: domain.EvidenceClassServiceMeta,
		TokenCost:     tokenestimate.Count(content),
	}, true
}

// DependencyUnit builds one evidence-backed service edge. Edges without source
// provenance are deliberately not promoted into authoritative evidence.
func DependencyUnit(
	service string,
	direction string,
	edge domain.DependencyEdge,
	coverage tool.EvidenceCoverage,
) (tool.EvidenceUnit, bool) {
	service = strings.TrimSpace(service)
	direction = strings.TrimSpace(direction)
	if service == "" || direction == "" || edge.From == "" || edge.To == "" || len(edge.Evidence) == 0 {
		return tool.EvidenceUnit{}, false
	}
	if coverage.Included == 0 {
		coverage.Included = 1
	}
	content := marshalCanonical(struct {
		From       string            `json:"from"`
		To         string            `json:"to"`
		Type       domain.EdgeType   `json:"type,omitempty"`
		Evidence   []domain.Evidence `json:"evidence"`
		Confidence float64           `json:"confidence,omitempty"`
	}{
		From: edge.From, To: edge.To, Type: edge.Type,
		Evidence: edge.Evidence, Confidence: edge.Confidence,
	})
	evidenceClass, trustTier := dependencyTrust(edge.Evidence)
	return tool.EvidenceUnit{
		SourceKind: "dependency",
		Target:     service,
		Sections: []string{fmt.Sprintf(
			"%s:%s->%s:%s", direction, edge.From, edge.To, edge.Type,
		)},
		ContentHash:   hashEvidenceContent(content),
		Coverage:      coverage,
		Facets:        facetValues(domain.ProvidedFacetsFor("dependency", "")),
		TrustTier:     trustTier,
		EvidenceClass: evidenceClass,
		TokenCost:     tokenestimate.Count(content),
	}, true
}

// APIUnit builds one source-backed route identity. A route without a canonical
// file location remains display data rather than fabricated code evidence.
func APIUnit(endpoint domain.EndpointRecord) (tool.EvidenceUnit, bool) {
	if strings.TrimSpace(endpoint.File) == "" || strings.TrimSpace(endpoint.Path) == "" {
		return tool.EvidenceUnit{}, false
	}
	content := marshalCanonical(endpoint)
	section := strings.TrimSpace(strings.ToUpper(endpoint.Method) + " " + endpoint.Path)
	if endpoint.HandlerMethod != "" {
		section += "@" + endpoint.HandlerMethod
	} else if endpoint.Handler != "" {
		section += "@" + endpoint.Handler
	}
	evidenceClass, trustTier := sourceTrust(endpoint.Source, endpoint.File, endpoint.Repo)
	return tool.EvidenceUnit{
		SourceKind:    "code",
		Target:        endpoint.File,
		Sections:      []string{"route:" + section},
		ContentHash:   hashEvidenceContent(content),
		Coverage:      tool.EvidenceCoverage{Complete: true, Included: 1},
		Facets:        facetValues(domain.ProvidedFacetsFor("code", "")),
		TrustTier:     trustTier,
		EvidenceClass: evidenceClass,
		TokenCost:     tokenestimate.Count(content),
	}, true
}

func hashEvidenceContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum)
}

func marshalCanonical(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

func facetValues(facets []domain.EvidenceFacet) []string {
	values := make([]string, len(facets))
	for index, facet := range facets {
		values[index] = string(facet)
	}
	return values
}

func dependencyTrust(items []domain.Evidence) (string, int) {
	class, trust := domain.EvidenceClassUnknown, domain.TrustUnknown
	for _, item := range items {
		candidateClass, candidateTrust := sourceTrust(item.Kind, item.Path, "")
		if candidateTrust > trust {
			class, trust = candidateClass, candidateTrust
		}
	}
	return class, trust
}

func sourceTrust(source domain.SourceKind, path, repo string) (string, int) {
	switch source {
	case domain.SourceCodeScan:
		return domain.EvidenceForCodeChunk(strings.TrimPrefix(filepath.Ext(path), "."), repo)
	case domain.SourceConfig:
		return domain.EvidenceClassConfig, domain.TrustConfig
	case domain.SourceDoc:
		return domain.EvidenceClassRepoDoc, domain.TrustRepoDoc
	default:
		return domain.EvidenceClassUnknown, domain.TrustUnknown
	}
}
