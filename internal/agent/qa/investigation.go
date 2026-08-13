package qa

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

// InvestigationRunner owns the durable workflow boundary used by QA.
type InvestigationRunner interface {
	Available() bool
	Start(context.Context, InvestigationRequest) error
	AwaitTerminal(context.Context, string) (InvestigationTerminal, error)
	LoadTerminal(context.Context, string) (InvestigationTerminal, error)
	Cancel(context.Context, string, int64) error
}

type InvestigationRequest struct {
	WorkflowRunID string
	Contract      TaskContract
	Actor         agentapi.Actor
}

// TaskContract is the canonical input shared by every delegated investigator.
type TaskContract struct {
	TaskID        string         `json:"task_id"`
	Question      string         `json:"question"`
	Objective     string         `json:"objective"`
	Entities      []EntityRef    `json:"entities"`
	EvidenceGoals []EvidenceGoal `json:"evidence_goals"`
	Context       TaskContext    `json:"context"`
}

type EntityRef struct {
	ID string `json:"id"`
}

type EvidenceGoal struct {
	ID       string `json:"id"`
	Facet    string `json:"facet"`
	Required bool   `json:"required"`
}

type TaskContext struct {
	ConversationRefs []ConversationRef   `json:"conversation_refs,omitempty"`
	TimeRange        *TaskTimeRange      `json:"time_range,omitempty"`
	SeedEvidence     []tool.EvidenceUnit `json:"seed_evidence,omitempty"`
}

type ConversationRef struct {
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Turn      int    `json:"turn,omitempty"`
}

type TaskTimeRange struct {
	From        time.Time `json:"from"`
	To          time.Time `json:"to"`
	ToExclusive bool      `json:"to_exclusive"`
	Raw         string    `json:"raw,omitempty"`
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

type InvestigationStatus string

const (
	InvestigationSucceeded InvestigationStatus = "succeeded"
	InvestigationFailed    InvestigationStatus = "failed"
	InvestigationCancelled InvestigationStatus = "cancelled"
	InvestigationTimedOut  InvestigationStatus = "timed_out"
)

// InvestigationTerminal carries only durable workflow terminal facts.
type InvestigationTerminal struct {
	WorkflowRunID string
	Status        InvestigationStatus
	ErrorCode     string
	Output        *InvestigationResult
	Usage         InvestigationUsage
}

type InvestigationResult struct {
	Answer      string                  `json:"answer"`
	Citations   []InvestigationCitation `json:"citations"`
	Limitations []string                `json:"limitations"`
}

func taskContractFromPreparation(
	prepared *qaPreparation,
	seedBlocks []ContextBlock,
) TaskContract {
	intent := prepared.analysis.RetrievalIntent
	entities := make([]EntityRef, 0, len(intent.TargetEntities))
	for _, entity := range intent.TargetEntities {
		entities = append(entities, EntityRef{ID: entity})
	}
	goals := make([]EvidenceGoal, 0, len(intent.RequiredFacets))
	for _, facet := range intent.RequiredFacets {
		value := string(facet)
		goals = append(goals, EvidenceGoal{ID: value, Facet: value, Required: true})
	}
	taskContext := TaskContext{
		ConversationRefs: append([]ConversationRef(nil), prepared.conversationRefs...),
		SeedEvidence:     preloadedEvidence(seedBlocks),
	}
	if prepared.analysis.HasTimeRange {
		resolved := prepared.analysis.TimeRange
		taskContext.TimeRange = &TaskTimeRange{
			From: resolved.From, To: resolved.To,
			ToExclusive: resolved.ToExclusive, Raw: resolved.Raw,
		}
	}
	return TaskContract{
		TaskID: prepared.request.RunID, Question: prepared.request.Question,
		Objective: prepared.planning.CleanQuestion, Entities: entities,
		EvidenceGoals: goals, Context: taskContext,
	}
}

func preloadedEvidence(blocks []ContextBlock) []tool.EvidenceUnit {
	ledger := evidence.New(nil, "")
	for _, block := range blocks {
		ledger.Add(block.Evidence, "preloaded")
	}
	return ledger.Units()
}

func investigationOutcome(terminal InvestigationTerminal) (RunOutcome, error) {
	switch terminal.Status {
	case InvestigationSucceeded:
		if terminal.Output == nil {
			return RunOutcome{}, fmt.Errorf(
				"investigation workflow %q succeeded without output",
				terminal.WorkflowRunID,
			)
		}
		return successfulInvestigationOutcome(*terminal.Output, terminal.Usage), nil
	case InvestigationFailed:
		return RunOutcome{
			Status: RunStatusFailed, ErrorCode: terminal.ErrorCode,
			Err: fmt.Errorf(
				"investigation workflow %q failed with code %q",
				terminal.WorkflowRunID,
				terminal.ErrorCode,
			),
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}, nil
	case InvestigationCancelled:
		return RunOutcome{
			Status: RunStatusAborted, ErrorCode: terminal.ErrorCode,
			Err:      fmt.Errorf("investigation workflow %q was cancelled", terminal.WorkflowRunID),
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}, nil
	case InvestigationTimedOut:
		return RunOutcome{
			Status: RunStatusFailed, ErrorCode: terminal.ErrorCode,
			Err:      fmt.Errorf("investigation workflow %q timed out", terminal.WorkflowRunID),
			Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
		}, nil
	default:
		return RunOutcome{}, fmt.Errorf(
			"investigation workflow %q has unknown terminal status %q",
			terminal.WorkflowRunID,
			terminal.Status,
		)
	}
}

func successfulInvestigationOutcome(
	result InvestigationResult,
	usage InvestigationUsage,
) RunOutcome {
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
		Status: RunStatusDone, Answer: answer, TokenUsed: int(usage.TotalTokens),
		References: references, HitCount: len(references),
		Evidence: EvidenceMetrics{
			Status: evidenceStatus, ResultCount: len(references),
			ToolCallCount: int(usage.ToolCalls), PartialResultCount: partialResults,
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
