package tools

import (
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	canonicalevidence "github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/tool"
)

func webEvidenceUnits(result WebSearchResponse) []tool.EvidenceUnit {
	if result.Fetched == nil {
		return nil
	}
	unit, ok := canonicalevidence.WebPageUnit(
		result.Fetched.URL,
		result.Fetched.Title,
		result.Fetched.Content,
		tool.EvidenceCoverage{Partial: true, Included: 1},
	)
	if !ok {
		return nil
	}
	return []tool.EvidenceUnit{unit}
}

func codeEvidenceUnits(result knowledge.CodeSearchResult) []tool.EvidenceUnit {
	units := make([]tool.EvidenceUnit, 0, len(result.Matches))
	for _, hit := range result.Matches {
		unit, ok := canonicalevidence.CodeUnit(
			"code", hit.Path, hit.StartLine, hit.EndLine, hit.Text, hit.Lang, hit.Repo,
			hit.EvidenceClass, hit.TrustTier, tool.EvidenceCoverage{Partial: true, Included: 1},
		)
		if ok {
			units = append(units, unit)
		}
	}
	return units
}

func serviceEvidenceUnits(records []domain.ServiceRecord) []tool.EvidenceUnit {
	units := make([]tool.EvidenceUnit, 0, len(records))
	for _, record := range records {
		unit, ok := canonicalevidence.ServiceMetadataUnit(
			record.ServiceName, record.Layer, record.Language, record.Summary,
		)
		if ok {
			units = append(units, unit)
		}
	}
	return units
}

func dependencyEvidenceUnits(result domain.DependencyTrace) []tool.EvidenceUnit {
	coverage := tool.EvidenceCoverage{
		Complete: !result.Truncated,
		Partial:  result.Truncated,
		Included: 1, OmittedItems: boolInt(result.Truncated),
	}
	units := make([]tool.EvidenceUnit, 0, len(result.Upstream)+len(result.Downstream))
	appendEdges := func(direction string, edges []domain.DependencyEdge) {
		for _, edge := range edges {
			unit, ok := canonicalevidence.DependencyUnit(result.Service, direction, edge, coverage)
			if ok {
				units = append(units, unit)
			}
		}
	}
	appendEdges("upstream", result.Upstream)
	appendEdges("downstream", result.Downstream)
	return units
}

func apiEvidenceUnits(endpoints []domain.EndpointRecord) []tool.EvidenceUnit {
	units := make([]tool.EvidenceUnit, 0, len(endpoints))
	for _, endpoint := range endpoints {
		unit, ok := canonicalevidence.APIUnit(endpoint)
		if ok {
			units = append(units, unit)
		}
	}
	return units
}

func symbolEvidenceUnits(result map[string]any) []tool.EvidenceUnit {
	if stringValue(result["resolution"]) != "unique" {
		return nil
	}
	matches := objectList(result["matches"])
	if len(matches) != 1 {
		return nil
	}
	match := matches[0]
	source := stringValue(match["source"])
	coverage := tool.EvidenceCoverage{Complete: true, Included: 1}
	if strings.Contains(source, "...(truncated)") {
		coverage.Complete = false
		coverage.Partial = true
		coverage.OmittedItems = 1
	}
	unit, ok := canonicalevidence.CodeUnit(
		"codegraph", stringValue(match["file"]), intValue(match["line"]),
		intValue(match["endLine"]), source, stringValue(match["language"]), "", "", 0, coverage,
	)
	if !ok {
		return nil
	}
	return []tool.EvidenceUnit{unit}
}

func callChainEvidenceUnits(result map[string]any) []tool.EvidenceUnit {
	var units []tool.EvidenceUnit
	seen := make(map[string]struct{})
	appendDirection := func(raw any) {
		direction, ok := raw.(map[string]any)
		if !ok {
			return
		}
		truncated, _ := direction["truncated"].(bool)
		unresolved := stringList(direction["unresolved"])
		coverage := tool.EvidenceCoverage{
			Complete: !truncated && len(unresolved) == 0,
			Partial:  truncated || len(unresolved) > 0,
			Included: 1, OmittedItems: len(unresolved) + boolInt(truncated),
		}
		for _, node := range objectList(direction["nodes"]) {
			source := stringValue(node["source"])
			itemCoverage := coverage
			if strings.Contains(source, "...(truncated)") {
				itemCoverage.Complete = false
				itemCoverage.Partial = true
				itemCoverage.OmittedItems++
			}
			unit, ok := canonicalevidence.CodeUnit(
				"codegraph", stringValue(node["file"]), intValue(node["line"]),
				intValue(node["endLine"]), source, stringValue(node["language"]), "", "", 0, itemCoverage,
			)
			if !ok {
				continue
			}
			key, ok := canonicalevidence.UnitKey(unit)
			if !ok {
				continue
			}
			if _, duplicate := seen[key.String()]; duplicate {
				continue
			}
			seen[key.String()] = struct{}{}
			units = append(units, unit)
		}
	}
	appendDirection(result["callers"])
	appendDirection(result["callees"])
	return units
}

func serviceRefs(records []domain.ServiceRecord) []tool.Reference {
	refs := make([]tool.Reference, 0, len(records))
	for _, record := range records {
		if record.ServiceName == "" {
			continue
		}
		refs = append(refs, tool.Reference{
			Type: tool.ReferenceService, Label: record.ServiceName, Target: record.ServiceName,
		})
	}
	return refs
}

func dependencyRefs(result domain.DependencyTrace) []tool.Reference {
	if len(dependencyEvidenceUnits(result)) == 0 {
		return nil
	}
	values := []string{result.Service}
	for _, edge := range append(append([]domain.DependencyEdge(nil), result.Upstream...), result.Downstream...) {
		if len(edge.Evidence) == 0 || edge.ExternalTarget != "" {
			continue
		}
		values = append(values, edge.From, edge.To)
	}
	return namedRefs(tool.ReferenceService, values)
}

func apiRefs(endpoints []domain.EndpointRecord) []tool.Reference {
	refs := make([]tool.Reference, 0, len(endpoints))
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		if _, ok := canonicalevidence.APIUnit(endpoint); !ok {
			continue
		}
		if _, duplicate := seen[endpoint.File]; duplicate {
			continue
		}
		seen[endpoint.File] = struct{}{}
		label := strings.TrimSpace(strings.ToUpper(endpoint.Method) + " " + endpoint.Path)
		refs = append(refs, tool.Reference{Type: tool.ReferenceCode, Label: label, Target: endpoint.File})
	}
	return refs
}

func symbolRefs(result map[string]any) []tool.Reference {
	if len(symbolEvidenceUnits(result)) == 0 {
		return nil
	}
	matches := objectList(result["matches"])
	if len(matches) != 1 {
		return nil
	}
	return symbolReferences(matches)
}

func callChainRefs(result map[string]any) []tool.Reference {
	if len(callChainEvidenceUnits(result)) == 0 {
		return nil
	}
	var nodes []map[string]any
	for _, key := range []string{"callers", "callees"} {
		if direction, ok := result[key].(map[string]any); ok {
			nodes = append(nodes, objectList(direction["nodes"])...)
		}
	}
	return symbolReferences(nodes)
}

func symbolReferences(nodes []map[string]any) []tool.Reference {
	refs := make([]tool.Reference, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		target := stringValue(node["qualifiedName"])
		if target == "" {
			target = stringValue(node["function"])
		}
		if target == "" {
			continue
		}
		if _, duplicate := seen[target]; duplicate {
			continue
		}
		seen[target] = struct{}{}
		label := target
		if file := stringValue(node["file"]); file != "" {
			label = fmt.Sprintf("%s (%s:L%d)", target, file, intValue(node["line"]))
		}
		refs = append(refs, tool.Reference{Type: tool.ReferenceSymbol, Label: label, Target: target})
	}
	return refs
}

func namedRefs(referenceType tool.ReferenceType, values []string) []tool.Reference {
	refs := make([]tool.Reference, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		refs = append(refs, tool.Reference{Type: referenceType, Label: value, Target: value})
	}
	return refs
}

func objectList(raw any) []map[string]any {
	switch values := raw.(type) {
	case []map[string]any:
		return values
	case []any:
		out := make([]map[string]any, 0, len(values))
		for _, value := range values {
			if item, ok := value.(map[string]any); ok {
				out = append(out, item)
			}
		}
		return out
	default:
		return nil
	}
}

func stringList(raw any) []string {
	switch values := raw.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text := stringValue(value); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func stringValue(raw any) string {
	value, _ := raw.(string)
	return strings.TrimSpace(value)
}

func intValue(raw any) int {
	switch value := raw.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}
