package agent

import (
	"database/sql"

	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
)

type RunStore = agentrun.RunStore
type RunHub = agentrun.RunHub
type RunStatus = agentrun.RunStatus
type RunOutcome = agentrun.RunOutcome
type EvidenceStatus = agentrun.EvidenceStatus
type EvidenceMetrics = agentrun.EvidenceMetrics
type StepKind = agentrun.StepKind
type StepRecord = agentrun.StepRecord
type ControlKind = agentrun.ControlKind
type ControlSignal = agentrun.ControlSignal
type Observer = agentrun.Observer
type Controller = agentrun.Controller
type RunRecord = agentrun.RunRecord
type RunKind = agentrun.RunKind
type RunUsageSummary = agentrun.RunUsageSummary
type ContextUsageSnapshot = agentrun.ContextUsageSnapshot
type LLMCallRow = agentrun.LLMCallRow
type StepRow = agentrun.StepRow
type RunDetail = agentrun.RunDetail
type RunPage = agentrun.RunPage
type ToolResultArtifactChunk = agentrun.ToolResultArtifactChunk
type EventType = agentrun.EventType
type TextEvent = agentrun.TextEvent
type ToolStartedEvent = agentrun.ToolStartedEvent
type ToolFinishedEvent = agentrun.ToolFinishedEvent
type ExecutionEvent = agentrun.ExecutionEvent
type ExecutionEventEmitter = agentrun.ExecutionEventEmitter
type SSEEvent = agentrun.SSEEvent
type RunTerminal = agentrun.RunTerminal

const (
	RunStatusRunning = agentrun.RunStatusRunning
	RunStatusDone    = agentrun.RunStatusDone
	RunStatusFailed  = agentrun.RunStatusFailed
	RunStatusAborted = agentrun.RunStatusAborted
	RunStatusPaused  = agentrun.RunStatusPaused

	EvidenceNotRequired = agentrun.EvidenceNotRequired
	EvidenceComplete    = agentrun.EvidenceComplete
	EvidencePartial     = agentrun.EvidencePartial
	EvidenceUnavailable = agentrun.EvidenceUnavailable

	RunKindAgent    = agentrun.RunKindAgent
	RunKindQAParent = agentrun.RunKindQAParent

	StepKindThink      = agentrun.StepKindThink
	StepKindToolCall   = agentrun.StepKindToolCall
	StepKindToolResult = agentrun.StepKindToolResult
	StepKindAnswer     = agentrun.StepKindAnswer
	StepKindRetrieval  = agentrun.StepKindRetrieval

	CtrlNone  = agentrun.CtrlNone
	CtrlPause = agentrun.CtrlPause
	CtrlAbort = agentrun.CtrlAbort
	CtrlNudge = agentrun.CtrlNudge

	EventAnswerDelta       = agentrun.EventAnswerDelta
	EventToolStarted       = agentrun.EventToolStarted
	EventToolFinished      = agentrun.EventToolFinished
	EventStatus            = agentrun.EventStatus
	EventReasoningDelta    = agentrun.EventReasoningDelta
	EventTrace             = agentrun.EventTrace
	EventLLMCall           = agentrun.EventLLMCall
	EventExecutionRouted   = agentrun.EventExecutionRouted
	EventExecutionDegraded = agentrun.EventExecutionDegraded
	EventWorkflowStarted   = agentrun.EventWorkflowStarted
	EventAgentStarted      = agentrun.EventAgentStarted
	EventAgentCompleted    = agentrun.EventAgentCompleted
	EventEvidenceJoined    = agentrun.EventEvidenceJoined
	EventRunFinished       = agentrun.EventRunFinished
)

var (
	ErrRunNotActive = agentrun.ErrRunNotActive
	ErrEmptyAnswer  = agentrun.ErrEmptyAnswer
)

func NewRunStore(db *sql.DB) (*RunStore, error) {
	return agentrun.NewRunStore(db)
}

func bindRunStore(db *sql.DB) *RunStore {
	return agentrun.BindStore(db)
}

func NewRunHub(store *RunStore) *RunHub {
	return agentrun.NewRunHub(store)
}

func NoopObserver() Observer {
	return agentrun.NoopObserver()
}

func TerminalFromEvent(event SSEEvent) *RunTerminal {
	return agentrun.TerminalFromEvent(event)
}

type runCreator interface {
	Create(RunRecord) error
}
