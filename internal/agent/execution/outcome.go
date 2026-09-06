package execution

import (
	"context"
	"errors"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

// OutcomeFor maps one loop result to the shared persisted run outcome.
func OutcomeFor(
	result *RunResult,
	preRetrieved []agentapi.Reference,
	runErr error,
) run.Outcome {
	if result == nil {
		if runErr == nil {
			runErr = errors.New("agent: run returned no result")
		}
		return run.Outcome{
			Status: run.StatusFailed,
			Err:    runErr,
			Evidence: run.EvidenceMetrics{
				Status: run.EvidenceUnavailable,
			},
		}
	}
	outcome := run.Outcome{
		StepCount:       result.Steps,
		TokenUsed:       len(result.Answer),
		Answer:          result.Answer,
		SessionMessages: append([]llm.Message(nil), result.SessionMessages...),
		Evidence:        result.Evidence,
		References:      MergeOutcomeReferences(preRetrieved, result.References),
		DelegationAdoptions: cloneDelegationAdoptions(
			result.DelegationAdoptions,
		),
	}
	outcome.HitCount = len(outcome.References)
	if outcome.Evidence.Status == "" {
		outcome.Evidence.Status = run.EvidenceUnavailable
	}
	// Preserve the execution-layer terminal flags so the public projection can
	// carry answer completeness, fallback usage, completeness, and the original
	// termination cause without inventing a second state machine.
	outcome.AnswerComplete = result.AnswerComplete
	outcome.FallbackUsed = result.FallbackUsed
	outcome.Completeness = result.Completeness
	outcome.TerminationReason = result.TerminationReason

	deadlineCause := context.DeadlineExceeded
	switch {
	case result.Aborted:
		outcome.Status = run.StatusAborted
		outcome.ErrorCode = "cancelled"
		outcome.Err = runErr
		if outcome.TerminationReason == "" {
			outcome.TerminationReason = "cancelled"
		}
	case runErr != nil:
		outcome.Status = run.StatusFailed
		outcome.ErrorCode = errorCodeFor(runErr, "runtime_failed")
		outcome.Err = runErr
		if outcome.TerminationReason == "" {
			outcome.TerminationReason = terminationReasonFor(runErr, deadlineCause)
		}
	case result.Err != nil:
		outcome.Status = run.StatusFailed
		outcome.ErrorCode = errorCodeFor(result.Err, "agent_failed")
		outcome.Err = result.Err
		if outcome.TerminationReason == "" {
			outcome.TerminationReason = terminationReasonFor(result.Err, deadlineCause)
		}
	case result.OutputMode == agentapi.RunOutputEvidenceWorker:
		outcome.Status = run.StatusDone
	case strings.TrimSpace(result.Answer) == "":
		outcome.Status = run.StatusFailed
		outcome.ErrorCode = "empty_output"
		outcome.Err = run.ErrEmptyAnswer
	default:
		outcome.Status = run.StatusDone
	}

	// Status is intentionally left as the original three-state result here.
	// Public partial classification happens in the definition layer after
	// output recovery and contract validation, using the explicit
	// AnswerComplete/FallbackUsed/Completeness signals rather than inferring
	// partial from a default-false bool. This keeps recovery paths intact for
	// callers that construct RunResult values directly.
	return outcome
}

// terminationReasonFor classifies the original stop cause without losing the
// underlying deadline error. It returns the empty string only when there is no
// error to classify.
func terminationReasonFor(err error, deadlineCause error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, deadlineCause):
		return "deadline_exceeded"
	case errors.Is(err, agentapi.ErrBudgetExceeded), errors.Is(err, ErrModelCallBudgetExhausted):
		return "budget_exhausted"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "provider_error"
	}
}

func errorCodeFor(err error, fallback string) string {
	if errors.Is(err, agentapi.ErrBudgetExceeded) || errors.Is(err, ErrModelCallBudgetExhausted) {
		return "budget_exhausted"
	}
	return fallback
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
