package run

import (
	"database/sql"
	"errors"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

type RunStore struct {
	db *sql.DB
}

type runStepStore interface {
	AddStep(StepRow) error
}

type runCompleter interface {
	Complete(string, RunOutcome) error
}

type runControlStore interface {
	TransitionControl(string, RunStatus, RunStatus) error
}

// NewRunStore binds agent run queries to the platform-owned MySQL pool.
func NewRunStore(db *sql.DB) (*RunStore, error) {
	if db == nil {
		return nil, fmt.Errorf("agent/runstore: database is required")
	}
	runStore := &RunStore{db: db}
	recovered, err := runStore.RecoverInterrupted()
	if err != nil {
		return nil, fmt.Errorf("agent/runstore: recover interrupted runs: %w", err)
	}
	if recovered > 0 {
		log.Warnf("[qa] recovered %d interrupted agent runs as aborted", recovered)
	}
	return runStore, nil
}

// BindStore binds a database without running startup recovery.
func BindStore(db *sql.DB) *RunStore {
	return &RunStore{db: db}
}

var ErrRunNotActive = errors.New("agent: run is missing or already terminal")

const maxToolResultArtifactChunkBytes = 256 << 10

// ToolResultArtifactChunk is one bounded, tenant-scoped artifact read.
type ToolResultArtifactChunk struct {
	ID          string                `json:"id"`
	SessionID   string                `json:"session_id"`
	RunID       string                `json:"run_id"`
	ToolCallID  string                `json:"tool_call_id"`
	Content     string                `json:"content"`
	ContentType string                `json:"content_type"`
	SHA256      string                `json:"sha256"`
	SizeBytes   int64                 `json:"size_bytes"`
	Coverage    tool.EvidenceCoverage `json:"coverage"`
	Offset      int64                 `json:"offset"`
	NextOffset  int64                 `json:"next_offset"`
	HasMore     bool                  `json:"has_more"`
	CreatedAt   string                `json:"created_at"`
}

// ContextUsageSnapshot is the latest round's observed context footprint.
type ContextUsageSnapshot struct {
	PeakInputTokens    int
	PeakReservedTokens int
}

type rowScanner interface {
	Scan(...any) error
}

type RunRecord struct {
	ID                   string                       `json:"id"`
	RunKind              RunKind                      `json:"run_kind"`
	UserID               int64                        `json:"user_id"`
	SessionID            string                       `json:"session_id"`
	AgentID              string                       `json:"agent_id"`
	DefinitionVersion    int64                        `json:"definition_version"`
	DefinitionHash       string                       `json:"definition_hash"`
	Selection            agentapi.DefinitionSelection `json:"selection"`
	ToolSnapshotID       string                       `json:"tool_snapshot_id"`
	InputSchemaVersion   int64                        `json:"input_schema_version"`
	OutputSchemaVersion  int64                        `json:"output_schema_version"`
	ParentRunID          string                       `json:"parent_run_id"`
	WorkflowRunID        string                       `json:"workflow_run_id"`
	WorkflowNodeID       string                       `json:"workflow_node_id"`
	Question             string                       `json:"question"`
	Status               RunStatus                    `json:"status"`
	ErrorCode            string                       `json:"error_code"`
	Mode                 string                       `json:"mode"`
	MaxSteps             int                          `json:"max_steps"`
	StepCount            int                          `json:"step_count"`
	TokenUsed            int                          `json:"token_used"`
	InputTokens          int64                        `json:"input_tokens"`
	CachedInputTokens    int64                        `json:"cached_input_tokens"`
	OutputTokens         int64                        `json:"output_tokens"`
	ReasoningTokens      int64                        `json:"reasoning_tokens"`
	TotalTokens          int64                        `json:"total_tokens"`
	LLMCallCount         int                          `json:"llm_call_count"`
	PeakInputTokens      int                          `json:"peak_input_tokens"`
	PeakReservedTokens   int                          `json:"peak_reserved_tokens"`
	EvidenceStatus       EvidenceStatus               `json:"evidence_status"`
	ForcedConclusion     bool                         `json:"forced_conclusion"`
	EvidenceResultCount  int                          `json:"evidence_result_count"`
	ToolCallCount        int                          `json:"tool_call_count"`
	ToolFailureCount     int                          `json:"tool_failure_count"`
	PartialResultCount   int                          `json:"partial_result_count"`
	OmittedEvidenceCount int                          `json:"omitted_evidence_count"`
	StartedAt            string                       `json:"started_at"`
	EndedAt              string                       `json:"ended_at"`
}

type RunKind string

const (
	RunKindAgent    RunKind = "agent"
	RunKindQAParent RunKind = "qa_parent"
)

// RunUsageSummary is the token snapshot needed by the live QA composer.
type RunUsageSummary struct {
	RunID                   string `json:"run_id"`
	SessionTotalTokens      int64  `json:"session_total_tokens"`
	RoundInputTokens        int64  `json:"round_input_tokens"`
	RoundCachedInputTokens  int64  `json:"round_cached_input_tokens"`
	RoundTotalTokens        int64  `json:"round_total_tokens"`
	RoundPeakInputTokens    int64  `json:"round_peak_input_tokens"`
	RoundPeakReservedTokens int64  `json:"round_peak_reserved_tokens"`
}

type LLMCallRow struct {
	ID                int64  `json:"id"`
	RunID             string `json:"run_id"`
	CallSeq           int    `json:"call_seq"`
	Phase             string `json:"phase"`
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	InputTokens       int    `json:"input_tokens"`
	CachedInputTokens int    `json:"cached_input_tokens"`
	OutputTokens      int    `json:"output_tokens"`
	ReasoningTokens   int    `json:"reasoning_tokens"`
	TotalTokens       int    `json:"total_tokens"`
	MaxOutputTokens   int    `json:"max_output_tokens"`
	DurationMs        int64  `json:"duration_ms"`
	Status            string `json:"status"`
	CreatedAt         string `json:"created_at"`
}

type StepRow struct {
	ID                  int64                 `json:"id"`
	RunID               string                `json:"run_id"`
	StepNo              int                   `json:"step_no"`
	Kind                StepKind              `json:"kind"`
	TraceID             string                `json:"trace_id,omitempty"`
	ArtifactID          string                `json:"artifact_id,omitempty"`
	ToolCallID          string                `json:"tool_call_id,omitempty"`
	Tool                string                `json:"tool,omitempty"`
	Args                string                `json:"args,omitempty"`
	ResultPreview       string                `json:"result_preview,omitempty"`
	Failed              bool                  `json:"failed,omitempty"`
	DeliveryError       string                `json:"delivery_error,omitempty"`
	Content             string                `json:"content,omitempty"`
	PromptContent       string                `json:"prompt_content,omitempty"`
	AuthoritativeSHA256 string                `json:"authoritative_sha256,omitempty"`
	PromptSHA256        string                `json:"prompt_sha256,omitempty"`
	SizeBytes           int64                 `json:"size_bytes,omitempty"`
	Coverage            tool.EvidenceCoverage `json:"coverage,omitempty"`
	AnswerContract      tool.AnswerContract   `json:"answer_contract,omitempty"`
	TokenDelta          int                   `json:"token_delta"`
	ReasoningTokens     int                   `json:"reasoning_tokens"`
	DurationMs          int                   `json:"duration_ms"`
	CreatedAt           string                `json:"created_at"`
}

type RunDetail struct {
	RunRecord
	Steps    []StepRow    `json:"steps"`
	LLMCalls []LLMCallRow `json:"llm_calls"`
	Terminal *RunTerminal `json:"terminal,omitempty"`
}

type RunPage = domain.Page[RunRecord]

// RunControlRecord carries only the persisted facts needed to dispatch control.
type RunControlRecord struct {
	ID            string
	RunKind       RunKind
	Status        RunStatus
	WorkflowRunID string
	UserID        int64
}

// QAParentRecord is the durable identity and state needed for parent reconciliation.
type QAParentRecord struct {
	ID            string
	WorkflowRunID string
	UserID        int64
	SessionID     string
	Question      string
	Status        RunStatus
	StartedAt     string
	EndedAt       string
}

// QAParentCursor is a stable keyset cursor over parent creation order.
type QAParentCursor struct {
	StartedAt string
	ID        string
}
