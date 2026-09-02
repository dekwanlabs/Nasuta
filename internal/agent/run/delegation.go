package run

import (
	"errors"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

var (
	ErrDelegationBudgetInsufficient = errors.New("agent: delegation budget is insufficient")
	ErrDelegationChildLimit         = errors.New("agent: delegation child limit exceeded")
	ErrDelegationTaskConflict       = errors.New("agent: delegation task identity conflicts with persisted admission")
	ErrDelegationNotAdmitted        = errors.New("agent: delegation task was not admitted")
	ErrDelegationAccounting         = errors.New("agent: delegation usage exceeds its reservation")
	ErrEvidenceLedgerConflict       = errors.New("agent: evidence ledger conflicts with persisted artifact")
	ErrWorkItemConflict             = errors.New("agent: work item identity conflicts with persisted queue item")
)

// DelegationReservation is the immutable grant persisted before a child starts.
type DelegationReservation struct {
	ParentRunID        string                 `json:"parent_run_id"`
	DelegationID       string                 `json:"delegation_id"`
	TaskIndex          int                    `json:"task_index"`
	ChildRunID         string                 `json:"child_run_id"`
	Capability         agentapi.CapabilityRef `json:"capability"`
	CapabilityHash     string                 `json:"capability_content_hash"`
	ObjectiveHash      string                 `json:"objective_hash"`
	Limits             agentapi.RunLimits     `json:"limits"`
	ReservedTokens     int64                  `json:"reserved_tokens"`
	ReservedCostMicros int64                  `json:"reserved_cost_micros"`
}

// DelegationAdmission owns the aggregate parent budget check for one batch.
type DelegationAdmission struct {
	ParentRunID         string
	DelegationID        string
	MaxChildren         int
	MaxTotalTokens      int64
	MaxTotalCostMicros  int64
	ParentAnswerReserve int64
	Reservations        []DelegationReservation
}

// DelegationRejection persists a deterministic admission failure without a child Run.
type DelegationRejection struct {
	ParentRunID    string
	DelegationID   string
	TaskIndex      int
	Capability     agentapi.CapabilityRef
	CapabilityHash string
	ObjectiveHash  string
	Code           string
}

// RunArtifact is one content-addressed artifact owned by a Run.
type RunArtifact struct {
	ID          string
	RunID       string
	Kind        string
	Schema      agentapi.SchemaRef
	ContentHash string
	Content     []byte
}

type DelegationArtifact = RunArtifact

// DelegationSettlement releases one reservation after authoritative usage is known.
type DelegationSettlement struct {
	ParentRunID      string
	DelegationID     string
	TaskIndex        int
	ChildRunID       string
	Usage            agentapi.Usage
	Artifact         *DelegationArtifact
	EvidenceArtifact *RunArtifact
}

// DelegationTaskRecord stores association and budget facts, not a second lifecycle.
type DelegationTaskRecord struct {
	ParentRunID      string
	DelegationID     string
	TaskIndex        int
	ChildRunID       string
	Capability       agentapi.CapabilityRef
	CapabilityHash   string
	ObjectiveHash    string
	Admitted         bool
	RejectionCode    string
	Reservation      DelegationReservation
	SettledUsage     *agentapi.Usage
	ReportArtifactID string
	Existing         bool `json:"-"`
}

// DelegationAttemptStatus is the durable lifecycle state of one child attempt.
type DelegationAttemptStatus string

const (
	DelegationAttemptRunning     DelegationAttemptStatus = "running"
	DelegationAttemptSucceeded   DelegationAttemptStatus = "succeeded"
	DelegationAttemptFailed      DelegationAttemptStatus = "failed"
	DelegationAttemptCancelled   DelegationAttemptStatus = "cancelled"
	DelegationAttemptTimedOut    DelegationAttemptStatus = "timed_out"
	DelegationAttemptInterrupted DelegationAttemptStatus = "interrupted"
)

// DelegationAttemptRecord makes retries observable without changing the
// logical delegation task identity. attempt_no starts at one.
type DelegationAttemptRecord struct {
	ParentRunID      string
	DelegationID     string
	TaskIndex        int
	AttemptNo        int
	AttemptID        string
	ChildRunID       string
	Status           DelegationAttemptStatus
	Retryable        bool
	ErrorCode        string
	ErrorMessage     string
	StartedAt        string
	EndedAt          string
	NextAttemptAt    string
	Usage            *agentapi.Usage
	ReportArtifactID string
	Existing         bool
}

// DelegationAttemptStart is the idempotent admission input for one attempt.
type DelegationAttemptStart struct {
	ParentRunID  string
	DelegationID string
	TaskIndex    int
	AttemptNo    int
	AttemptID    string
	ChildRunID   string
	StartedAt    string
}

// DelegationAttemptFinish closes one attempt. A retryable failed attempt does
// not settle the logical task; only the final attempt produces a report.
type DelegationAttemptFinish struct {
	ParentRunID      string
	DelegationID     string
	TaskIndex        int
	AttemptNo        int
	AttemptID        string
	Status           DelegationAttemptStatus
	Retryable        bool
	ErrorCode        string
	ErrorMessage     string
	EndedAt          string
	NextAttemptAt    string
	Usage            *agentapi.Usage
	ReportArtifactID string
}

// DelegationCheckpointStatus is the parent-side durable handoff state.
type DelegationCheckpointStatus string

const (
	DelegationCheckpointPending     DelegationCheckpointStatus = "pending"
	DelegationCheckpointCompleted   DelegationCheckpointStatus = "completed"
	DelegationCheckpointUnavailable DelegationCheckpointStatus = "unavailable"
	DelegationCheckpointInterrupted DelegationCheckpointStatus = "interrupted"
)

// DelegationCheckpoint records the point at which the parent delegated work.
// It is deliberately independent from Agent StepNo so older callers can use
// the recovery boundary even when a step number is not available at the tool
// boundary.
type DelegationCheckpoint struct {
	ParentRunID      string
	DelegationID     string
	TaskIndex        int
	InvocationID     string
	RequestHash      string
	Status           DelegationCheckpointStatus
	ChildRunID       string
	ReportArtifactID string
	ErrorCode        string
	ErrorMessage     string
	CreatedAt        string
	UpdatedAt        string
}
