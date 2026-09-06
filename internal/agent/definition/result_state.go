package definition

import (
	"encoding/json"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

func internalMessage(message agentapi.Message) llm.Message {
	compiled := llm.Message{
		Role: message.Role, Content: message.Content,
		ToolCallID: message.ToolCallID, Name: message.Name,
	}
	if len(message.ToolCalls) == 0 {
		return compiled
	}
	compiled.ToolCalls = make([]llm.ToolCall, 0, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		compiled.ToolCalls = append(compiled.ToolCalls, llm.ToolCall{
			ID: call.ID, Type: call.Type,
			Function: llm.ToolFunction{
				Name: call.Function.Name, Arguments: call.Function.Arguments,
			},
		})
	}
	return compiled
}

func publicEvidence(evidence run.EvidenceMetrics) agentapi.EvidenceSummary {
	return agentapi.EvidenceSummary{
		Status: string(evidence.Status), ForcedConclusion: evidence.ForcedConclusion,
		ToolCallCount: evidence.ToolCallCount, ResultCount: evidence.ResultCount,
		ToolFailureCount:   evidence.ToolFailureCount,
		PartialResultCount: evidence.PartialResultCount,
		OmittedItemCount:   evidence.OmittedItemCount,
	}
}

func publicEvidenceConflicts(conflicts []evidence.Conflict) []agentapi.EvidenceConflict {
	return evidence.PublicConflicts(conflicts)
}
func referencesFromRequest(blocks []agentapi.ContextBlock) []agentapi.Reference {
	count := 0
	for _, block := range blocks {
		count += len(block.References)
	}
	if count == 0 {
		return nil
	}
	references := make([]agentapi.Reference, 0, count)
	for _, block := range blocks {
		references = append(references, block.References...)
	}
	return references
}

func contextReferenceTypes(blocks []agentapi.ContextBlock) map[string]tool.ReferenceType {
	var index map[string]tool.ReferenceType
	for _, block := range blocks {
		for _, reference := range block.References {
			referenceType := tool.ReferenceType(reference.Type)
			switch referenceType {
			case tool.ReferenceRunbook, tool.ReferenceService, tool.ReferenceSymbol:
				if reference.Target == "" {
					continue
				}
				if index == nil {
					index = make(map[string]tool.ReferenceType)
				}
				index[reference.Target] = referenceType
			}
		}
	}
	return index
}

func joinedContextContent(blocks []agentapi.ContextBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	var content strings.Builder
	for _, block := range blocks {
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		content.WriteString("## ")
		content.WriteString(block.Title)
		content.WriteString("\n")
		content.WriteString(block.Content)
	}
	return content.String()
}

func contextEvidenceUnits(blocks []agentapi.ContextBlock) []tool.EvidenceUnit {
	count := 0
	for _, block := range blocks {
		count += len(block.Evidence)
	}
	if count == 0 {
		return nil
	}
	units := make([]tool.EvidenceUnit, 0, count)
	for _, block := range blocks {
		for _, unit := range block.Evidence {
			unit.Sections = append([]string(nil), unit.Sections...)
			unit.Facets = append([]string(nil), unit.Facets...)
			units = append(units, unit)
		}
	}
	return units
}

func evidenceConflicts(
	blocks []agentapi.ContextBlock,
) []evidence.Conflict {
	count := 0
	for _, block := range blocks {
		count += len(block.EvidenceConflicts)
	}
	if count == 0 {
		return nil
	}
	conflicts := make([]evidence.Conflict, 0, count)
	for _, block := range blocks {
		for _, conflict := range block.EvidenceConflicts {
			conflicts = append(conflicts, evidence.Conflict{
				Key: evidence.Key{
					SourceKind: conflict.Identity.SourceKind,
					Target:     conflict.Identity.Target,
					Section:    conflict.Identity.Section,
					Version:    conflict.Identity.Version,
					TimeRange:  conflict.Identity.TimeRange,
				},
				Current:        evidence.CloneUnit(conflict.Current),
				Incoming:       evidence.CloneUnit(conflict.Incoming),
				CurrentOrigin:  conflict.CurrentOrigin,
				IncomingOrigin: conflict.IncomingOrigin,
			})
		}
	}
	return conflicts
}

func hashMessages(messages []llm.Message) string {
	raw, _ := json.Marshal(messages)
	return hashBytes(raw)
}
