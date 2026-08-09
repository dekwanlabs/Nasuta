package run

import (
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

type EventType string

const (
	EventAnswerDelta       EventType = "answer.delta"
	EventToolStarted       EventType = "tool.started"
	EventToolFinished      EventType = "tool.finished"
	EventStatus            EventType = "status"
	EventReasoningDelta    EventType = "reasoning.delta"
	EventTrace             EventType = "trace"
	EventLLMCall           EventType = "llm.call"
	EventSessionStatus     EventType = "session.status"
	EventExecutionRouted   EventType = "execution.routed"
	EventExecutionDegraded EventType = "execution.degraded"
	EventWorkflowStarted   EventType = "workflow.started"
	EventAgentStarted      EventType = "agent.started"
	EventAgentCompleted    EventType = "agent.completed"
	EventEvidenceJoined    EventType = "evidence.joined"
	EventRunFinished       EventType = "run.finished"
)

type TextEvent struct {
	Text string `json:"text"`
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

// ExecutionEvent is the stable product projection for routed QA work.
type ExecutionEvent struct {
	RunID         string  `json:"run_id"`
	WorkflowRunID string  `json:"workflow_run_id,omitempty"`
	NodeID        string  `json:"node_id,omitempty"`
	Strategy      string  `json:"strategy,omitempty"`
	Status        string  `json:"status"`
	Reason        string  `json:"reason,omitempty"`
	Complexity    float64 `json:"complexity,omitempty"`
	Confidence    float64 `json:"confidence,omitempty"`
}

type ExecutionEventEmitter interface {
	EmitExecutionEvent(EventType, ExecutionEvent)
}

// SSEEvent is the tagged event forwarded unchanged by the HTTP transport.
type SSEEvent struct {
	Type EventType `json:"type"`
	Data any       `json:"data"`
}

// RunTerminal is the sole real-time projection of one persisted Run outcome.
type RunTerminal struct {
	RunID           string               `json:"run_id"`
	Status          RunStatus            `json:"status"`
	Answer          string               `json:"answer,omitempty"`
	StepCount       int                  `json:"step_count"`
	TokenUsed       int                  `json:"token_used"`
	References      []agentapi.Reference `json:"references,omitempty"`
	HitCount        int                  `json:"hit_count"`
	Error           string               `json:"error,omitempty"`
	Evidence        EvidenceMetrics      `json:"evidence"`
	SessionMessages []llm.Message        `json:"-"`
}

func TerminalFromEvent(event SSEEvent) *RunTerminal {
	if event.Type != EventRunFinished {
		return nil
	}
	terminal, _ := event.Data.(*RunTerminal)
	return terminal
}
