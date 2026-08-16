package run

import (
	"errors"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

type EventType string

const (
	EventAnswerDelta                    EventType = "answer.delta"
	EventToolStarted                    EventType = "tool.started"
	EventToolFinished                   EventType = "tool.finished"
	EventStatus                         EventType = "status"
	EventReasoningDelta                 EventType = "reasoning.delta"
	EventTrace                          EventType = "trace"
	EventLLMCall                        EventType = "llm.call"
	EventSessionStatus                  EventType = "session.status"
	EventExecutionRouted                EventType = "execution.routed"
	EventExecutionDegraded              EventType = "execution.degraded"
	EventWorkflowStarted                EventType = "workflow.started"
	EventAgentStarted                   EventType = "agent.started"
	EventAgentCompleted                 EventType = "agent.completed"
	EventEvidenceJoined                 EventType = "evidence.joined"
	EventContextUsage                   EventType = "context.usage"
	EventDelegationCreated              EventType = "delegation.created"
	EventDelegationStarted              EventType = "delegation.started"
	EventDelegationDone                 EventType = "delegation.completed"
	EventDelegationFailed               EventType = "delegation.failed"
	EventDelegationCancelled            EventType = "delegation.cancelled"
	EventDelegationRejected             EventType = "delegation.rejected"
	EventDelegationValidated            EventType = "delegation.validated"
	EventDelegationShadow               EventType = "delegation.shadow_evaluated"
	EventDelegationVerificationStarted  EventType = "delegation.verification_started"
	EventDelegationVerificationDone     EventType = "delegation.verification_completed"
	EventDelegationVerificationFailed   EventType = "delegation.verification_failed"
	EventDelegationVerificationRejected EventType = "delegation.verification_rejected"
	EventDelegationAdoptionEvaluated    EventType = "delegation.adoption_evaluated"
	EventRunFinished                    EventType = "run.finished"
)

type TextEvent struct {
	Text      string `json:"text"`
	Code      string `json:"code,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
}

type ToolStartedEvent struct {
	Step       int    `json:"step"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name"`
	Args       string `json:"args"`
}

type ToolFinishedEvent struct {
	Step          int    `json:"step"`
	ToolCallID    string `json:"tool_call_id,omitempty"`
	Tool          string `json:"tool"`
	Summary       string `json:"summary"`
	TraceID       string `json:"trace_id,omitempty"`
	ArtifactID    string `json:"artifact_id,omitempty"`
	Failed        bool   `json:"failed"`
	DeliveryError string `json:"delivery_error,omitempty"`
	DurationMs    int    `json:"duration_ms"`
	SizeBytes     int64  `json:"size_bytes"`
}

type SessionStatusEvent struct {
	Status      string `json:"status"`
	Text        string `json:"text"`
	FromTurn    int    `json:"from_turn,omitempty"`
	ToTurn      int    `json:"to_turn,omitempty"`
	UpdatedAtMs int64  `json:"updated_at_ms"`
}

// ContextUsageEvent is the unified context-budget projection for one run.
// ProjectedBeforeTokens is the value used by compaction and hard admission;
// actual provider usage is reported separately by the run usage aggregate.
type ContextUsageEvent struct {
	Phase                 string `json:"phase"`
	ProjectedBeforeTokens int    `json:"projected_before_tokens"`
	ProjectedAfterTokens  int    `json:"projected_after_tokens"`
	PeakProjectedTokens   int    `json:"peak_projected_tokens"`
	ContextWindow         int    `json:"context_window"`
	HighWaterTokens       int    `json:"high_water_tokens"`
	SafetyTokens          int    `json:"safety_tokens"`
	SafeLimitTokens       int    `json:"safe_limit_tokens"`
	OutputReserveTokens   int    `json:"output_reserve_tokens"`
	CompactionTriggered   bool   `json:"compaction_triggered"`
	CompactionApplied     bool   `json:"compaction_applied"`
}

// ExecutionEvent is the stable product projection for routed QA work.
type ExecutionEvent struct {
	RunID                   string         `json:"run_id"`
	ParentRunID             string         `json:"parent_run_id,omitempty"`
	ChildRunID              string         `json:"child_run_id,omitempty"`
	WorkflowRunID           string         `json:"workflow_run_id,omitempty"`
	NodeID                  string         `json:"node_id,omitempty"`
	DelegationID            string         `json:"delegation_id,omitempty"`
	Capability              string         `json:"capability,omitempty"`
	ObjectiveSummary        string         `json:"objective_summary,omitempty"`
	Strategy                string         `json:"strategy,omitempty"`
	Status                  string         `json:"status"`
	Reason                  string         `json:"reason,omitempty"`
	ErrorCode               string         `json:"error_code,omitempty"`
	ReportID                string         `json:"report_id,omitempty"`
	ReportIDs               []string       `json:"report_ids,omitempty"`
	AdoptionStatus          string         `json:"adoption_status,omitempty"`
	DurationMS              int64          `json:"duration_ms,omitempty"`
	Usage                   agentapi.Usage `json:"usage,omitempty"`
	ToolCalls               int64          `json:"tool_calls,omitempty"`
	ReportBytes             int            `json:"report_bytes,omitempty"`
	Completeness            string         `json:"completeness,omitempty"`
	CitationCoverage        float64        `json:"citation_coverage,omitempty"`
	StructuredClaimCoverage float64        `json:"structured_claim_coverage,omitempty"`
	ConflictCount           int            `json:"conflict_count,omitempty"`
	RequiresVerification    bool           `json:"requires_verification,omitempty"`
	VerificationReasons     []string       `json:"verification_reasons,omitempty"`
	VerificationID          string         `json:"verification_id,omitempty"`
	QueryKind               string         `json:"query_kind,omitempty"`
	Shadow                  bool           `json:"shadow,omitempty"`
	ReferenceCount          int            `json:"reference_count,omitempty"`
	Complexity              float64        `json:"complexity,omitempty"`
	Confidence              float64        `json:"confidence,omitempty"`
}

type ExecutionEventEmitter interface {
	EmitEvent(EventType, ExecutionEvent)
}

// SSEEvent is the tagged event forwarded unchanged by the HTTP transport.
type SSEEvent struct {
	Type EventType `json:"type"`
	Data any       `json:"data"`
}

// Terminal is the sole real-time projection of one persisted Run outcome.
type Terminal struct {
	RunID               string                        `json:"run_id"`
	Status              Status                        `json:"status"`
	Answer              string                        `json:"answer,omitempty"`
	ErrorCode           string                        `json:"error_code,omitempty"`
	StepCount           int                           `json:"step_count"`
	TokenUsed           int                           `json:"token_used"`
	References          []agentapi.Reference          `json:"references,omitempty"`
	DelegationAdoptions []agentapi.DelegationAdoption `json:"delegation_adoptions,omitempty"`
	HitCount            int                           `json:"hit_count"`
	Error               string                        `json:"error,omitempty"`
	Evidence            EvidenceMetrics               `json:"evidence"`
	SessionMessages     []llm.Message                 `json:"-"`
}

// QAParentEvent is the durable counterpart of a Parent Run projection.
type QAParentEvent struct {
	RunID     string   `json:"run_id"`
	Seq       int64    `json:"seq"`
	Kind      string   `json:"kind"`
	Summary   string   `json:"summary"`
	Detail    Terminal `json:"detail"`
	CreatedAt string   `json:"created_at"`
}

func TerminalFromEvent(event SSEEvent) *Terminal {
	if event.Type != EventRunFinished {
		return nil
	}
	terminal, _ := event.Data.(*Terminal)
	return terminal
}

func terminalFromOutcome(runID string, outcome Outcome) Terminal {
	terminal := Terminal{
		RunID: runID, Status: outcome.Status, Answer: outcome.Answer,
		ErrorCode: outcome.ErrorCode, StepCount: outcome.StepCount,
		TokenUsed:  outcome.TokenUsed,
		References: append([]agentapi.Reference(nil), outcome.References...),
		DelegationAdoptions: cloneDelegationAdoptions(
			outcome.DelegationAdoptions,
		),
		HitCount: outcome.HitCount,
		Evidence: outcome.Evidence,
		SessionMessages: append(
			[]llm.Message(nil),
			outcome.SessionMessages...,
		),
	}
	if outcome.Err != nil {
		terminal.Error = outcome.Err.Error()
	}
	return terminal
}

func outcomeFromTerminal(terminal Terminal) Outcome {
	outcome := Outcome{
		Status: terminal.Status, ErrorCode: terminal.ErrorCode,
		StepCount: terminal.StepCount, TokenUsed: terminal.TokenUsed,
		Answer:     terminal.Answer,
		Evidence:   terminal.Evidence,
		References: append([]agentapi.Reference(nil), terminal.References...),
		DelegationAdoptions: cloneDelegationAdoptions(
			terminal.DelegationAdoptions,
		),
		HitCount: terminal.HitCount,
	}
	if terminal.Error != "" {
		outcome.Err = errors.New(terminal.Error)
	}
	return outcome
}

func cloneDelegationAdoptions(
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
