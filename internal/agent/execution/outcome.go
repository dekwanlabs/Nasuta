package execution

import (
	"errors"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

// OutcomeFor maps one loop result to the shared persisted run outcome.
func OutcomeFor(
	result *RunResult,
	preRetrieved []agentapi.Reference,
	runErr error,
) agentrun.RunOutcome {
	if result == nil {
		if runErr == nil {
			runErr = errors.New("agent: run returned no result")
		}
		return agentrun.RunOutcome{
			Status: agentrun.RunStatusFailed,
			Err:    runErr,
			Evidence: agentrun.EvidenceMetrics{
				Status: agentrun.EvidenceUnavailable,
			},
		}
	}
	outcome := agentrun.RunOutcome{
		StepCount:       result.Steps,
		TokenUsed:       len(result.Answer),
		Answer:          result.Answer,
		SessionMessages: append([]llm.Message(nil), result.SessionMessages...),
		Evidence:        result.Evidence,
		References:      MergeOutcomeReferences(preRetrieved, result.References),
	}
	outcome.HitCount = len(outcome.References)
	if outcome.Evidence.Status == "" {
		outcome.Evidence.Status = agentrun.EvidenceUnavailable
	}
	switch {
	case result.Aborted:
		outcome.Status = agentrun.RunStatusAborted
		outcome.ErrorCode = "cancelled"
		outcome.Err = runErr
	case runErr != nil:
		outcome.Status = agentrun.RunStatusFailed
		outcome.ErrorCode = "runtime_failed"
		outcome.Err = runErr
	case result.Err != nil:
		outcome.Status = agentrun.RunStatusFailed
		outcome.ErrorCode = "agent_failed"
		outcome.Err = result.Err
	case strings.TrimSpace(result.Answer) == "":
		outcome.Status = agentrun.RunStatusFailed
		outcome.ErrorCode = "empty_output"
		outcome.Err = agentrun.ErrEmptyAnswer
	default:
		outcome.Status = agentrun.RunStatusDone
	}
	return outcome
}

// MergeOutcomeReferences keeps one canonical public reference set across sources.
func MergeOutcomeReferences(
	preRetrieved []agentapi.Reference,
	dynamic []tool.Reference,
) []agentapi.Reference {
	merged := make([]agentapi.Reference, 0, len(preRetrieved)+len(dynamic))
	seen := make(map[string]struct{}, len(preRetrieved)+len(dynamic))
	for _, reference := range preRetrieved {
		key := reference.Type + "\x00" + reference.Target
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, reference)
	}
	for _, reference := range dynamic {
		key := string(reference.Type) + "\x00" + reference.Target
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, agentapi.Reference{
			Type:   string(reference.Type),
			Label:  reference.Label,
			Target: reference.Target,
		})
	}
	return merged
}
