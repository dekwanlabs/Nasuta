package definition

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

func canonicalStructuredOutput(answer string) (json.RawMessage, error) {
	value := strings.TrimSpace(answer)
	if json.Valid([]byte(value)) {
		return json.RawMessage(value), nil
	}

	lines := strings.Split(value, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[len(lines)-1]) != "```" {
		return nil, errors.New("structured output must be JSON or one JSON fence")
	}
	opener := strings.ToLower(strings.TrimSpace(lines[0]))
	if opener != "```" && opener != "```json" {
		return nil, fmt.Errorf("unsupported structured output fence %q", opener)
	}
	for _, line := range lines[1 : len(lines)-1] {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			return nil, errors.New("structured output contains multiple fences")
		}
	}
	payload := strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	if !json.Valid([]byte(payload)) {
		return nil, errors.New("fenced structured output is not valid JSON")
	}
	return json.RawMessage(payload), nil
}

func validatedOutput(
	schemas *agentapi.SchemaRegistry,
	ref agentapi.SchemaRef,
	answer string,
) (json.RawMessage, error) {
	raw, rawErr := canonicalStructuredOutput(answer)
	if rawErr == nil {
		normalized := normalizeOutputForSchema(ref, raw)
		if err := schemas.Validate(ref, normalized); err == nil {
			return append(json.RawMessage(nil), normalized...), nil
		} else {
			rawErr = err
		}
	}
	// RepairJSON covers the common truncation/fence/trailing-comma defects
	// without altering valid string contents. Schema validation remains the
	// final arbiter, so a repair can never silently widen the contract.
	repaired := llm.RepairJSON(answer)
	if repaired != strings.TrimSpace(answer) && json.Valid([]byte(repaired)) {
		normalized := normalizeOutputForSchema(ref, json.RawMessage(repaired))
		if err := schemas.Validate(ref, normalized); err == nil {
			return append(json.RawMessage(nil), normalized...), nil
		} else {
			rawErr = err
		}
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return nil, fmt.Errorf("encode definition output: %w", err)
	}
	if err := schemas.Validate(ref, encoded); err == nil {
		return encoded, nil
	}
	return nil, fmt.Errorf(
		"definition output does not match schema %q version %d: %w",
		ref.ID, ref.Version, rawErr,
	)
}

const (
	investigationDiscoveredEntitiesLimit     = 50
	investigationDiscoveredDependenciesLimit = 20
)

func normalizeOutputForSchema(ref agentapi.SchemaRef, raw json.RawMessage) json.RawMessage {
	if ref != agentapi.InvestigationReportSchemaRef() {
		return raw
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		return raw
	}
	changed := false
	changed = stripUnknownReportFields(report) || changed
	changed = normalizeFindingEvidence(report) || changed
	changed = normalizeDiscoveredEntities(report) || changed
	changed = normalizeDiscoveredDependencies(report) || changed
	if !changed {
		return raw
	}
	normalized, err := json.Marshal(report)
	if err != nil {
		return raw
	}
	return normalized
}

// investigationReportFields mirrors the additionalProperties:false objects in
// the investigation.report schema. The model occasionally hallucinates an extra
// key at any nesting level (e.g. a "summary_scope" on an evidence item); such a
// key is rejected by the schema validator and would otherwise sink the whole
// report into the dependency-graph fallback. These keys are never read back —
// downstream consumers decode into typed structs that ignore unknown fields —
// so dropping them is lossless and keeps the model's own findings and flow.
//
// A zero fieldRule{} marks a leaf (no recursion); objectOf marks a nested
// object; arrayOf marks an array of objects.
var investigationReportFields = objectFields{
	"focus":                     fieldRule{},
	"summary":                   fieldRule{},
	"findings":                  arrayOf(findingFields),
	"gaps":                      fieldRule{},
	"covered_evidence_goals":    fieldRule{},
	"unresolved_evidence_goals": fieldRule{},
	"discovered_entities":       fieldRule{},
	"discovered_dependencies":   arrayOf(dependencyFields),
	"flow":                      objectOf(flowFields),
}

var findingFields = objectFields{
	"claim":             fieldRule{},
	"entity_ids":        fieldRule{},
	"evidence_goal_ids": fieldRule{},
	"evidence":          arrayOf(evidenceFields),
	"confidence":        fieldRule{},
}

var evidenceFields = objectFields{
	"kind":        fieldRule{},
	"reference":   fieldRule{},
	"summary":     fieldRule{},
	"evidence_id": fieldRule{},
	"identity":    objectOf(identityFields),
}

var identityFields = objectFields{
	"source_kind": fieldRule{},
	"target":      fieldRule{},
	"section":     fieldRule{},
	"version":     fieldRule{},
	"time_range":  fieldRule{},
}

var flowFields = objectFields{
	"subject":    fieldRule{},
	"status":     fieldRule{},
	"nodes":      arrayOf(flowNodeFields),
	"edges":      arrayOf(flowEdgeFields),
	"open_hops":  fieldRule{},
	"confidence": fieldRule{},
}

var flowNodeFields = objectFields{
	"id":            fieldRule{},
	"label":         fieldRule{},
	"kind":          fieldRule{},
	"evidence_refs": fieldRule{},
}

var flowEdgeFields = objectFields{
	"from":           fieldRule{},
	"to":             fieldRule{},
	"protocol":       fieldRule{},
	"sync_mode":      fieldRule{},
	"evidence_refs":  fieldRule{},
	"evidence_state": fieldRule{},
}

var dependencyFields = objectFields{
	"from": fieldRule{},
	"to":   fieldRule{},
	"kind": fieldRule{},
}

// fieldRule is the recursion target for one whitelisted key: exactly one of
// object or items is set for a composite value; neither for a leaf.
type fieldRule struct {
	object objectFields
	items  objectFields
}

// objectFields maps an allowed key to its recursion rule.
type objectFields map[string]fieldRule

func objectOf(fields objectFields) fieldRule { return fieldRule{object: fields} }

func arrayOf(fields objectFields) fieldRule { return fieldRule{items: fields} }

// stripUnknownReportFields removes keys not present in the schema whitelist at
// every nesting level of the report. Only whitelisted keys are kept; required
// fields are never invented here, so a report missing them still fails
// validation and falls through to recovery as before.
func stripUnknownReportFields(report map[string]any) bool {
	return stripObjectFields(report, investigationReportFields)
}

func stripObjectFields(obj map[string]any, fields objectFields) bool {
	changed := false
	for key, value := range obj {
		rule, allowed := fields[key]
		if !allowed {
			delete(obj, key)
			changed = true
			continue
		}
		if stripValueFields(value, rule) {
			changed = true
		}
	}
	return changed
}

func stripValueFields(value any, rule fieldRule) bool {
	switch {
	case rule.object != nil:
		nested, ok := value.(map[string]any)
		return ok && stripObjectFields(nested, rule.object)
	case rule.items != nil:
		array, ok := value.([]any)
		if !ok {
			return false
		}
		changed := false
		for _, item := range array {
			element, ok := item.(map[string]any)
			if ok && stripObjectFields(element, rule.items) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func normalizeFindingEvidence(report map[string]any) bool {
	findings, ok := report["findings"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, findingValue := range findings {
		finding, ok := findingValue.(map[string]any)
		if !ok {
			continue
		}
		evidenceItems, ok := finding["evidence"].([]any)
		if !ok {
			continue
		}
		for _, evidenceValue := range evidenceItems {
			item, ok := evidenceValue.(map[string]any)
			if !ok {
				continue
			}
			if normalizeEvidenceItem(item) {
				changed = true
			}
		}
	}
	return changed
}

func normalizeEvidenceItem(item map[string]any) bool {
	changed := false
	if identity, exists := item["identity"]; exists {
		if _, isString := identity.(string); isString {
			delete(item, "identity")
			changed = true
		}
	}
	if evidenceID, exists := item["evidence_id"]; exists {
		if value, isString := evidenceID.(string); !isString || !isValidEvidenceID(value) {
			delete(item, "evidence_id")
			changed = true
		}
	}
	return changed
}

func normalizeDiscoveredEntities(report map[string]any) bool {
	entities, exists := report["discovered_entities"]
	if !exists {
		return false
	}
	bounded, clipped := boundUniqueStringList(entities, investigationDiscoveredEntitiesLimit)
	if !clipped {
		return false
	}
	report["discovered_entities"] = bounded
	return true
}

func normalizeDiscoveredDependencies(report map[string]any) bool {
	deps, ok := report["discovered_dependencies"].([]any)
	if !ok || len(deps) <= investigationDiscoveredDependenciesLimit {
		return false
	}
	report["discovered_dependencies"] = append([]any(nil), deps[:investigationDiscoveredDependenciesLimit]...)
	return true
}

func boundUniqueStringList(value any, maxItems int) ([]any, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]any, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return items, false
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
		if maxItems > 0 && len(out) >= maxItems {
			break
		}
	}
	if len(out) != len(items) {
		return out, true
	}
	for index := range items {
		if items[index] != out[index] {
			return out, true
		}
	}
	return items, false
}

func isValidEvidenceID(value string) bool {
	return evidence.ValidHandle(value)
}
