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
