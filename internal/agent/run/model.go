package run

import (
	"errors"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusAborted Status = "aborted"
	StatusPaused  Status = "paused"
)

func (status Status) Terminal() bool {
	switch status {
	case StatusDone, StatusFailed, StatusAborted:
		return true
	default:
		return false
	}
}

func validControlTransition(from, to Status) bool {
	return from == StatusRunning && to == StatusPaused ||
		from == StatusPaused && to == StatusRunning
}

var ErrEmptyAnswer = errors.New("agent: completed without a visible answer")

type EvidenceStatus string

const (
	EvidenceNotRequired EvidenceStatus = "not_required"
	EvidenceComplete    EvidenceStatus = "complete"
	EvidencePartial     EvidenceStatus = "partial"
	EvidenceUnavailable EvidenceStatus = "unavailable"
)

// EvidenceMetrics summarizes delivery coverage without re-reading persisted steps.
type EvidenceMetrics struct {
	Status             EvidenceStatus `json:"status"`
	ForcedConclusion   bool           `json:"forced_conclusion"`
	ToolCallCount      int            `json:"tool_call_count"`
	ResultCount        int            `json:"result_count"`
	ToolFailureCount   int            `json:"tool_failure_count"`
	PartialResultCount int            `json:"partial_result_count"`
	OmittedItemCount   int            `json:"omitted_item_count"`
}

// Finalize derives the persisted evidence state after the loop completes.
func (metrics *EvidenceMetrics) Finalize(direct bool) {
	switch {
	case metrics.ResultCount == 0 && metrics.ToolCallCount == 0 && direct:
		metrics.Status = EvidenceNotRequired
	case metrics.ResultCount == 0:
		metrics.Status = EvidenceUnavailable
	case metrics.ForcedConclusion || metrics.ToolFailureCount > 0 || metrics.PartialResultCount > 0:
		metrics.Status = EvidencePartial
	default:
		metrics.Status = EvidenceComplete
	}
}

// Outcome is the single terminal fact consumed by persistence and streaming.
type Outcome struct {
	Status              Status
	ErrorCode           string
	StepCount           int
	TokenUsed           int
	Answer              string
	SessionMessages     []llm.Message
	Evidence            EvidenceMetrics
	References          []agentapi.Reference
	DelegationAdoptions []agentapi.DelegationAdoption
	HitCount            int
	Err                 error
}

type StepKind string

const (
	StepKindThink      StepKind = "think"
	StepKindToolCall   StepKind = "tool_call"
	StepKindToolResult StepKind = "tool_result"
	StepKindAnswer     StepKind = "answer"
	StepKindRetrieval  StepKind = "retrieval"
)

type StepRecord struct {
	StepNo              int                           `json:"step_no"`
	Kind                StepKind                      `json:"kind"`
	TraceID             string                        `json:"trace_id,omitempty"`
	ArtifactID          string                        `json:"artifact_id,omitempty"`
	ToolCallID          string                        `json:"tool_call_id,omitempty"`
	Tool                string                        `json:"tool,omitempty"`
	Args                string                        `json:"args,omitempty"`
	ResultPreview       string                        `json:"result_preview,omitempty"`
	Failed              bool                          `json:"failed,omitempty"`
	DeliveryError       string                        `json:"delivery_error,omitempty"`
	Content             string                        `json:"content,omitempty"`
	PromptContent       string                        `json:"prompt_content,omitempty"`
	AuthoritativeSHA256 string                        `json:"authoritative_sha256,omitempty"`
	PromptSHA256        string                        `json:"prompt_sha256,omitempty"`
	SizeBytes           int64                         `json:"size_bytes,omitempty"`
	Coverage            tool.EvidenceCoverage         `json:"coverage,omitempty"`
	AnswerContract      tool.AnswerContract           `json:"answer_contract,omitempty"`
	DelegationAdoptions []agentapi.DelegationAdoption `json:"delegation_adoptions,omitempty"`
	TokenDelta          int                           `json:"token_delta"`
	ReasoningTokens     int                           `json:"reasoning_tokens"`
	DurationMs          int                           `json:"duration_ms"`
	CreatedAt           time.Time                     `json:"created_at"`
}

type ControlKind int

const (
	CtrlNone ControlKind = iota
	CtrlPause
	CtrlAbort
	CtrlNudge
)

type ControlSignal struct {
	Kind    ControlKind
	Message string
}
