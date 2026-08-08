package qa

import (
	"context"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// InvestigationRunner executes one fixed read-only investigation workflow.
type InvestigationRunner interface {
	Available() bool
	Run(context.Context, InvestigationRequest) (InvestigationResult, error)
}

type InvestigationRequest struct {
	WorkflowRunID string
	ParentRunID   string
	Question      string
	Actor         agentapi.Actor
}

type InvestigationEvidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

type InvestigationCitation struct {
	Claim    string                  `json:"claim"`
	Evidence []InvestigationEvidence `json:"evidence"`
}

type InvestigationUsage struct {
	InputTokens     int64
	OutputTokens    int64
	ReasoningTokens int64
	TotalTokens     int64
	ToolCalls       int64
	CostMicros      int64
}

type InvestigationResult struct {
	WorkflowRunID string                  `json:"-"`
	Answer        string                  `json:"answer"`
	Citations     []InvestigationCitation `json:"citations"`
	Limitations   []string                `json:"limitations"`
	Usage         InvestigationUsage      `json:"-"`
}

func investigationOutcome(result InvestigationResult, runErr error) RunOutcome {
	if runErr != nil {
		return RunOutcome{
			Status: RunStatusFailed, ErrorCode: "investigation_failed", Err: runErr,
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}
	}
	answer := strings.TrimSpace(result.Answer)
	if answer == "" {
		return RunOutcome{
			Status: RunStatusFailed, ErrorCode: "empty_output", Err: ErrEmptyAnswer,
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}
	}
	references := investigationReferences(result.Citations)
	evidenceStatus := EvidenceComplete
	partialResults := 0
	if len(result.Limitations) > 0 {
		evidenceStatus = EvidencePartial
		partialResults = len(result.Limitations)
	}
	return RunOutcome{
		Status: RunStatusDone, Answer: answer, TokenUsed: int(result.Usage.TotalTokens),
		References: references, HitCount: len(references),
		Evidence: EvidenceMetrics{
			Status: evidenceStatus, ResultCount: len(references),
			ToolCallCount: int(result.Usage.ToolCalls), PartialResultCount: partialResults,
		},
	}
}

func investigationReferences(citations []InvestigationCitation) []agentapi.Reference {
	count := 0
	for _, citation := range citations {
		count += len(citation.Evidence)
	}
	references := make([]agentapi.Reference, 0, count)
	seen := make(map[string]struct{}, count)
	for _, citation := range citations {
		for _, evidence := range citation.Evidence {
			kind := strings.TrimSpace(evidence.Kind)
			reference := strings.TrimSpace(evidence.Reference)
			if kind == "" || reference == "" {
				continue
			}
			key := kind + "\x00" + reference
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			references = append(references, agentapi.Reference{
				Type: kind, Label: strings.TrimSpace(evidence.Summary), Target: reference,
			})
		}
	}
	return references
}
