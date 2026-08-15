package definition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
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

func publicMessages(messages []llm.Message) []agentapi.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]agentapi.Message, 0, len(messages))
	for _, message := range messages {
		compiled := agentapi.Message{
			Role: message.Role, Content: message.Content,
			ToolCallID: message.ToolCallID, Name: message.Name,
		}
		if len(message.ToolCalls) > 0 {
			compiled.ToolCalls = make([]agentapi.ToolCall, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				compiled.ToolCalls = append(compiled.ToolCalls, agentapi.ToolCall{
					ID: call.ID, Type: call.Type,
					Function: agentapi.ToolFunction{
						Name: call.Function.Name, Arguments: call.Function.Arguments,
					},
				})
			}
		}
		out = append(out, compiled)
	}
	return out
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
	if len(conflicts) == 0 {
		return nil
	}
	out := make([]agentapi.EvidenceConflict, len(conflicts))
	for index, conflict := range conflicts {
		out[index] = agentapi.EvidenceConflict{
			Identity: agentapi.EvidenceIdentity{
				SourceKind: conflict.Key.SourceKind,
				Target:     conflict.Key.Target,
				Section:    conflict.Key.Section,
				Version:    conflict.Key.Version,
				TimeRange:  conflict.Key.TimeRange,
			},
			Current:        evidence.CloneUnit(conflict.Current),
			Incoming:       evidence.CloneUnit(conflict.Incoming),
			CurrentOrigin:  conflict.CurrentOrigin,
			IncomingOrigin: conflict.IncomingOrigin,
		}
	}
	return out
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

func mapResult(
	runID string,
	result *execution.RunResult,
	runErr error,
	cancelCause error,
	usage agentapi.Usage,
	preRetrieved []agentapi.Reference,
	schemas *agentapi.SchemaRegistry,
	outputSchema agentapi.SchemaRef,
) (agentapi.RunResult, run.Outcome) {
	if cancelCause != nil {
		outcome := execution.OutcomeFor(result, preRetrieved, cancelCause)
		outcome.Status = run.StatusAborted
		outcome.ErrorCode = "cancelled"
		publicResult := publicTerminalEvidence(runID, result, outcome, usage)
		publicResult.Status = agentapi.RunCancelled
		publicResult.Error = &agentapi.RunError{
			Code: "cancelled", Message: cancelCause.Error(),
		}
		return publicResult, outcome
	}
	outcome := execution.OutcomeFor(result, preRetrieved, runErr)
	if errors.Is(outcome.Err, execution.ErrToolCallBudgetExhausted) {
		outcome.ErrorCode = "tool_call_budget_exhausted"
	}
	if outcome.Status != run.StatusDone {
		runError := outcome.Err
		if runError == nil {
			runError = errors.New("definition run failed")
		}
		publicResult := publicTerminalEvidence(runID, result, outcome, usage)
		publicResult.Status = agentapi.RunFailed
		publicResult.Error = &agentapi.RunError{
			Code: outcome.ErrorCode, Message: runError.Error(),
			Retryable: retryableError(runError),
		}
		return publicResult, outcome
	}
	publicResult := publicTerminalEvidence(runID, result, outcome, usage)
	publicResult.Status = agentapi.RunSucceeded
	publicResult.Text = outcome.Answer
	publicResult.References = append([]agentapi.Reference(nil), outcome.References...)
	publicResult.Messages = publicMessages(outcome.SessionMessages)
	output, err := validatedOutput(schemas, outputSchema, outcome.Answer)
	if err != nil {
		outcome.Status = run.StatusFailed
		outcome.ErrorCode = "invalid_output"
		outcome.Err = err
		publicResult.Status = agentapi.RunFailed
		publicResult.Text = ""
		publicResult.References = nil
		publicResult.Messages = nil
		publicResult.Error = &agentapi.RunError{Code: "invalid_output", Message: err.Error()}
		return publicResult, outcome
	}
	publicResult.Output = output
	return publicResult, outcome
}

func publicTerminalEvidence(
	runID string,
	result *execution.RunResult,
	outcome run.Outcome,
	usage agentapi.Usage,
) agentapi.RunResult {
	publicResult := agentapi.RunResult{
		RunID: runID, Usage: usage,
		Evidence: publicEvidence(outcome.Evidence),
	}
	if result == nil {
		return publicResult
	}
	publicResult.EvidenceUnits = evidence.CloneUnits(result.EvidenceUnits)
	publicResult.EvidenceConflicts = publicEvidenceConflicts(result.EvidenceConflicts)
	return publicResult
}

func retryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var classified interface{ Retryable() bool }
	return errors.As(err, &classified) && classified.Retryable()
}

func validatedOutput(
	schemas *agentapi.SchemaRegistry,
	ref agentapi.SchemaRef,
	answer string,
) (json.RawMessage, error) {
	raw := json.RawMessage(strings.TrimSpace(answer))
	var rawErr error
	if json.Valid(raw) {
		rawErr = schemas.Validate(ref, raw)
		if rawErr == nil {
			return append(json.RawMessage(nil), raw...), nil
		}
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return nil, fmt.Errorf("encode definition output: %w", err)
	}
	if err := schemas.Validate(ref, encoded); err == nil {
		return encoded, nil
	} else if rawErr == nil {
		rawErr = err
	}
	return nil, fmt.Errorf("definition output does not match schema %q version %d: %w", ref.ID, ref.Version, rawErr)
}

func failedRun(runID, code string, err error) agentapi.RunResult {
	return agentapi.RunResult{
		RunID: runID, Status: agentapi.RunFailed,
		Error: &agentapi.RunError{Code: code, Message: err.Error()},
	}
}
