package qa

import (
	"errors"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/tool"
)

func outcomeFromPublicResult(result agentapi.RunResult) RunOutcome {
	outcome := RunOutcome{
		Answer: result.Text, TokenUsed: len(result.Text),
		SessionMessages: publicResultMessages(result.Messages),
		Evidence: EvidenceMetrics{
			Status: EvidenceStatus(result.Evidence.Status), ForcedConclusion: result.Evidence.ForcedConclusion,
			ToolCallCount: result.Evidence.ToolCallCount, ResultCount: result.Evidence.ResultCount,
			ToolFailureCount:   result.Evidence.ToolFailureCount,
			PartialResultCount: result.Evidence.PartialResultCount,
			OmittedItemCount:   result.Evidence.OmittedItemCount,
		},
		References: append([]agentapi.Reference(nil), result.References...),
		DelegationAdoptions: clonePublicDelegationAdoptions(
			result.DelegationAdoptions,
		),
	}
	outcome.HitCount = len(outcome.References)
	switch result.Status {
	case agentapi.RunSucceeded:
		outcome.Status = RunStatusDone
	case agentapi.RunCancelled:
		outcome.Status = RunStatusAborted
	default:
		outcome.Status = RunStatusFailed
	}
	if result.Error != nil {
		outcome.ErrorCode = result.Error.Code
		outcome.Err = errors.New(result.Error.Message)
	}
	return outcome
}

func clonePublicDelegationAdoptions(
	adoptions []agentapi.DelegationAdoption,
) []agentapi.DelegationAdoption {
	if len(adoptions) == 0 {
		return nil
	}
	cloned := make([]agentapi.DelegationAdoption, len(adoptions))
	for index, adoption := range adoptions {
		adoption.AdoptedReportIDs = append(
			[]string(nil),
			adoption.AdoptedReportIDs...,
		)
		cloned[index] = adoption
	}
	return cloned
}

func outcomeFor(result *RunResult, preRetrieved []agentapi.Reference, runErr error) RunOutcome {
	return execution.OutcomeFor(result, preRetrieved, runErr)
}

// mergeOutcomeReferences keeps one canonical public reference set across sources.
func mergeOutcomeReferences(preRetrieved []agentapi.Reference, dynamic []tool.Reference) []agentapi.Reference {
	return execution.MergeOutcomeReferences(preRetrieved, dynamic)
}
