package workflow

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

var (
	ErrInvalid          = errors.New("workflow input invalid")
	ErrNotFound         = errors.New("workflow resource not found")
	ErrForbidden        = errors.New("workflow operation forbidden")
	ErrConflict         = errors.New("workflow conflict")
	ErrUnavailable      = errors.New("workflow capability unavailable")
	ErrInvariant        = errors.New("workflow invariant violated")
	ErrNodePersistence  = errors.New("workflow node persistence failed")
	ErrRunPersistence   = errors.New("workflow run persistence failed")
	ErrApprovalConflict = fmt.Errorf(
		"workflow approval decision conflicts with existing fact: %w",
		ErrConflict,
	)
)

type RunStatus string

const (
	RunRunning      RunStatus = "running"
	RunSucceeded    RunStatus = "succeeded"
	RunFailed       RunStatus = "failed"
	RunCancelled    RunStatus = "cancelled"
	RunTimedOut     RunStatus = "timed_out"
	RunWaitingHuman RunStatus = "waiting_human"
)

type RunRecord struct {
	ID                  string                    `json:"id"`
	ParentRunID         string                    `json:"parent_run_id,omitempty"`
	Round               int                       `json:"round"`
	BaseDepth           int                       `json:"base_depth"`
	WorkflowID          string                    `json:"workflow_id"`
	WorkflowVersion     int64                     `json:"workflow_version"`
	WorkflowHash        string                    `json:"workflow_hash"`
	Selection           DefinitionSelection       `json:"selection,omitempty"`
	InputHash           string                    `json:"input_hash"`
	ActorUserID         int64                     `json:"actor_user_id"`
	ActorTenantID       string                    `json:"actor_tenant_id"`
	ActorPermissions    agentapi.PermissionPolicy `json:"actor_permissions"`
	Scenario            string                    `json:"scenario"`
	ScenarioPermissions agentapi.PermissionPolicy `json:"scenario_permissions"`
	Status              RunStatus                 `json:"status"`
	Budget              Budget                    `json:"budget"`
	Usage               Usage                     `json:"usage"`
	ErrorCode           string                    `json:"error_code,omitempty"`
	StopReason          StopReason                `json:"stop_reason,omitempty"`
	StartedAt           time.Time                 `json:"started_at"`
	EndedAt             *time.Time                `json:"ended_at,omitempty"`
}

type NodeRunRecord struct {
	WorkflowRunID   string     `json:"workflow_run_id"`
	NodeID          string     `json:"node_id"`
	Attempt         int        `json:"attempt"`
	Kind            NodeKind   `json:"kind"`
	AgentRunID      string     `json:"agent_run_id,omitempty"`
	InputHandoffIDs []string   `json:"input_handoff_ids"`
	OutputHandoffID string     `json:"output_handoff_id,omitempty"`
	Status          RunStatus  `json:"status"`
	Usage           Usage      `json:"usage"`
	ErrorCode       string     `json:"error_code,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	FirstStartedAt  time.Time  `json:"first_started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
}

type Event struct {
	WorkflowRunID string          `json:"workflow_run_id"`
	Seq           int64           `json:"seq"`
	Kind          string          `json:"kind"`
	NodeID        string          `json:"node_id,omitempty"`
	Summary       string          `json:"summary"`
	Detail        json.RawMessage `json:"detail,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// TerminalEventDetail projects the two independent terminal facts without
// introducing another persisted completion state.
type TerminalEventDetail struct {
	RunStatus    RunStatus    `json:"run_status"`
	Completeness Completeness `json:"completeness,omitempty"`
	StopReason   StopReason   `json:"stop_reason,omitempty"`
	ErrorCode    string       `json:"error_code,omitempty"`
}

type RunState struct {
	Run         RunRecord
	Input       Handoff
	Nodes       map[string]NodeRunRecord
	Handoffs    map[string]Handoff
	NodeOutputs map[string]Handoff
	Gates       map[string]GateDecision
	Approvals   map[string]Approval
}

// TerminalResult contains the durable facts used by owning domains to converge.
type TerminalResult struct {
	Run    RunRecord
	Output *Handoff
}

type ApprovalTransition struct {
	Approval  Approval
	Applied   bool
	RunStatus RunStatus
}

type HandoffCursor struct {
	CreatedAt time.Time
	ID        string
}

type NodeRunCursor struct {
	NodeID  string
	Attempt int
}

type ActiveRunCursor struct {
	StartedAt time.Time
	ID        string
}

type ActiveRunRef struct {
	ID        string
	StartedAt time.Time
}

type CancelTransition struct {
	Applied bool
	Status  RunStatus
}

// Store persists immutable workflow facts and exposes only bounded list reads.
type Store struct {
	db  *sql.DB
	hub *EventHub
}

func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("workflow store database is required")
	}
	return &Store{db: db, hub: NewEventHub()}, nil
}

func boundedLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 100 {
		return 100
	}
	return limit
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

type rowScanner interface {
	Scan(...any) error
}

func requireSingleTransition(result sql.Result, target string) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read workflow transition %q result: %w", target, err)
	}
	if affected != 1 {
		return fmt.Errorf(
			"workflow transition %q changed %d rows: %w",
			target, affected, ErrConflict,
		)
	}
	return nil
}
