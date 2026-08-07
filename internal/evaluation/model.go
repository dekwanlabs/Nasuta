package evaluation

import (
	"errors"
	"time"
)

var (
	ErrInvalid     = errors.New("evaluation input invalid")
	ErrNotFound    = errors.New("evaluation resource not found")
	ErrForbidden   = errors.New("evaluation operation forbidden")
	ErrConflict    = errors.New("evaluation fact conflicts with existing data")
	ErrUnavailable = errors.New("evaluation capability unavailable")
)

type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

type Comparison[T any] struct {
	ID        string `json:"id"`
	Window    Window `json:"window"`
	Base      T      `json:"base"`
	Candidate T      `json:"candidate"`
}

type WorkflowTrace struct {
	Run       WorkflowRunTrace `json:"run"`
	Nodes     []NodeRunTrace   `json:"nodes"`
	Agents    []AgentRunTrace  `json:"agents"`
	Events    []WorkflowEvent  `json:"events"`
	Truncated TraceTruncation  `json:"truncated"`
}

type WorkflowRunTrace struct {
	ID              string     `json:"id"`
	WorkflowID      string     `json:"workflow_id"`
	WorkflowVersion int64      `json:"workflow_version"`
	WorkflowHash    string     `json:"workflow_hash"`
	InputHash       string     `json:"input_hash"`
	Scenario        string     `json:"scenario"`
	Status          string     `json:"status"`
	InputTokens     int64      `json:"input_tokens"`
	OutputTokens    int64      `json:"output_tokens"`
	ReasoningTokens int64      `json:"reasoning_tokens"`
	TotalTokens     int64      `json:"total_tokens"`
	ToolCalls       int64      `json:"tool_calls"`
	CostMicros      int64      `json:"cost_micros"`
	Retries         int64      `json:"retries"`
	ErrorCode       string     `json:"error_code,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
}

type NodeRunTrace struct {
	NodeID          string     `json:"node_id"`
	Attempt         int        `json:"attempt"`
	Kind            string     `json:"kind"`
	AgentRunID      string     `json:"agent_run_id,omitempty"`
	Status          string     `json:"status"`
	InputTokens     int64      `json:"input_tokens"`
	OutputTokens    int64      `json:"output_tokens"`
	ReasoningTokens int64      `json:"reasoning_tokens"`
	TotalTokens     int64      `json:"total_tokens"`
	ToolCalls       int64      `json:"tool_calls"`
	CostMicros      int64      `json:"cost_micros"`
	Retries         int64      `json:"retries"`
	ErrorCode       string     `json:"error_code,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
}

type AgentRunTrace struct {
	ID                string     `json:"id"`
	AgentID           string     `json:"agent_id"`
	DefinitionVersion int64      `json:"definition_version"`
	DefinitionHash    string     `json:"definition_hash"`
	ToolSnapshotID    string     `json:"tool_snapshot_id"`
	WorkflowNodeID    string     `json:"workflow_node_id"`
	Status            string     `json:"status"`
	EvidenceStatus    string     `json:"evidence_status"`
	InputTokens       int64      `json:"input_tokens"`
	OutputTokens      int64      `json:"output_tokens"`
	ReasoningTokens   int64      `json:"reasoning_tokens"`
	TotalTokens       int64      `json:"total_tokens"`
	ToolCalls         int64      `json:"tool_calls"`
	ToolFailures      int64      `json:"tool_failures"`
	PartialResults    int64      `json:"partial_results"`
	OmittedEvidence   int64      `json:"omitted_evidence"`
	LLMCalls          int64      `json:"llm_calls"`
	ErrorCode         string     `json:"error_code,omitempty"`
	StartedAt         time.Time  `json:"started_at"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
}

type WorkflowEvent struct {
	Seq       int64     `json:"seq"`
	Kind      string    `json:"kind"`
	NodeID    string    `json:"node_id,omitempty"`
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

type TraceTruncation struct {
	Nodes  bool `json:"nodes"`
	Agents bool `json:"agents"`
	Events bool `json:"events"`
}

type AgentVersionMetrics struct {
	Version                  int64   `json:"version"`
	DefinitionHash           string  `json:"definition_hash,omitempty"`
	RunCount                 int64   `json:"run_count"`
	SuccessCount             int64   `json:"success_count"`
	SuccessRate              float64 `json:"success_rate"`
	EvidenceRequiredRunCount int64   `json:"evidence_required_run_count"`
	EvidenceCompleteCount    int64   `json:"evidence_complete_count"`
	EvidenceCompletenessRate float64 `json:"evidence_completeness_rate"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	ReasoningTokens          int64   `json:"reasoning_tokens"`
	TotalTokens              int64   `json:"total_tokens"`
	AverageTotalTokens       float64 `json:"average_total_tokens"`
	CostMicros               int64   `json:"cost_micros"`
	P95LatencyMillis         int64   `json:"p95_latency_ms"`
	ToolCalls                int64   `json:"tool_calls"`
	ToolFailures             int64   `json:"tool_failures"`
	ToolFailureRate          float64 `json:"tool_failure_rate"`
}

type WorkflowVersionMetrics struct {
	Version                  int64   `json:"version"`
	DefinitionHash           string  `json:"definition_hash,omitempty"`
	Mode                     string  `json:"mode"`
	AgentNodeCount           int64   `json:"agent_node_count"`
	ObservedAgentNodeCount   int64   `json:"observed_agent_node_count"`
	RunCount                 int64   `json:"run_count"`
	SuccessCount             int64   `json:"success_count"`
	SuccessRate              float64 `json:"success_rate"`
	RecoveredRunCount        int64   `json:"recovered_run_count"`
	RecoveryRate             float64 `json:"recovery_rate"`
	InputTokens              int64   `json:"input_tokens"`
	OutputTokens             int64   `json:"output_tokens"`
	ReasoningTokens          int64   `json:"reasoning_tokens"`
	TotalTokens              int64   `json:"total_tokens"`
	AverageTotalTokens       float64 `json:"average_total_tokens"`
	ToolCalls                int64   `json:"tool_calls"`
	CostMicros               int64   `json:"cost_micros"`
	Retries                  int64   `json:"retries"`
	P95LatencyMillis         int64   `json:"p95_latency_ms"`
	LinkedAgentRuns          int64   `json:"linked_agent_runs"`
	LinkedAgentSuccessRate   float64 `json:"linked_agent_success_rate"`
	EvidenceCompletenessRate float64 `json:"evidence_completeness_rate"`
}

type ReviewPolicyVersionMetrics struct {
	Version                int64   `json:"version"`
	PolicyHash             string  `json:"policy_hash,omitempty"`
	RoundCount             int64   `json:"round_count"`
	CompletedRoundCount    int64   `json:"completed_round_count"`
	CompletionRate         float64 `json:"completion_rate"`
	PassedRoundCount       int64   `json:"passed_round_count"`
	PassRate               float64 `json:"pass_rate"`
	ReportCount            int64   `json:"report_count"`
	FindingCount           int64   `json:"finding_count"`
	UniqueFindingCount     int64   `json:"unique_finding_count"`
	UniqueYield            float64 `json:"unique_yield"`
	DuplicateRate          float64 `json:"duplicate_rate"`
	ConflictRoundCount     int64   `json:"conflict_round_count"`
	ConflictRate           float64 `json:"conflict_rate"`
	LabeledResolutionCount int64   `json:"labeled_resolution_count"`
	AdoptedFindingCount    int64   `json:"adopted_finding_count"`
	AdoptionRate           float64 `json:"adoption_rate"`
	TruePositiveCount      int64   `json:"true_positive_count"`
	FalsePositiveCount     int64   `json:"false_positive_count"`
	FalseNegativeCount     int64   `json:"false_negative_count"`
	PrecisionAvailable     bool    `json:"precision_available"`
	Precision              float64 `json:"precision"`
	RecallAvailable        bool    `json:"recall_available"`
	Recall                 float64 `json:"recall"`
	CostMicros             int64   `json:"cost_micros"`
	CostTrackedRoundCount  int64   `json:"cost_tracked_round_count"`
	P95LatencyMillis       int64   `json:"p95_latency_ms"`
}

type ReviewLabelKind string

const (
	LabelTruePositive  ReviewLabelKind = "true_positive"
	LabelFalsePositive ReviewLabelKind = "false_positive"
	LabelFalseNegative ReviewLabelKind = "false_negative"
)

type ReviewLabelInput struct {
	Label      ReviewLabelKind `json:"label"`
	FindingID  string          `json:"finding_id,omitempty"`
	TargetHash string          `json:"target_hash,omitempty"`
	Category   string          `json:"category,omitempty"`
}

type ReviewLabel struct {
	Seq           int64           `json:"seq"`
	ID            string          `json:"id"`
	RoundID       string          `json:"round_id"`
	PolicyID      string          `json:"policy_id"`
	PolicyVersion int64           `json:"policy_version"`
	SubjectHash   string          `json:"subject_hash"`
	FindingID     string          `json:"finding_id,omitempty"`
	TargetHash    string          `json:"target_hash"`
	Category      string          `json:"category"`
	Label         ReviewLabelKind `json:"label"`
	CreatedBy     int64           `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
}
