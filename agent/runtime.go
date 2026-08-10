package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dekwanlabs/nasuta/tool"
)

type Actor struct {
	UserID   int64  `json:"user_id"`
	TenantID string `json:"tenant_id,omitempty"`
}

type Correlation struct {
	SessionID     string `json:"session_id,omitempty"`
	ParentRunID   string `json:"parent_run_id,omitempty"`
	WorkflowRunID string `json:"workflow_run_id,omitempty"`
	NodeID        string `json:"node_id,omitempty"`
}

type ContextBlock struct {
	Source      string              `json:"source"`
	Title       string              `json:"title"`
	Content     string              `json:"content"`
	References  []Reference         `json:"references,omitempty"`
	Evidence    []tool.EvidenceUnit `json:"evidence,omitempty"`
	Complete    bool                `json:"complete"`
	ContentHash string              `json:"content_hash"`
}

type Reference struct {
	Type   string `json:"type"`
	Label  string `json:"label,omitempty"`
	Target string `json:"target"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolScope narrows one run below the immutable Definition capability ceiling.
type ToolScope struct {
	AllowWrite      bool     `json:"allow_write"`
	RestrictVisible bool     `json:"restrict_visible"`
	VisibleToolIDs  []string `json:"visible_tool_ids,omitempty"`
	OfferedToolIDs  []string `json:"offered_tool_ids,omitempty"`
	PruneApplied    bool     `json:"prune_applied"`
}

// RunPolicy carries execution semantics that are independent of a scenario's planner types.
type RunPolicy struct {
	EvidenceRequired bool  `json:"evidence_required"`
	EvidenceSeeded   bool  `json:"evidence_seeded"`
	WebResearch      bool  `json:"web_research"`
	MaxToolCalls     int64 `json:"max_tool_calls"`
	RedactSensitive  bool  `json:"redact_sensitive"`
}

// DefinitionSelection records the rollout decision that selected a definition.
type DefinitionSelection struct {
	RuleVersion           int64  `json:"rule_version,omitempty"`
	RuleHash              string `json:"rule_hash,omitempty"`
	CandidateVersion      int64  `json:"candidate_version,omitempty"`
	BucketBasisPoints     int    `json:"bucket_basis_points,omitempty"`
	PercentageBasisPoints int    `json:"percentage_basis_points,omitempty"`
	StableKeyHash         string `json:"stable_key_hash,omitempty"`
	Reason                string `json:"reason,omitempty"`
}

type RunRequest struct {
	RunID          string              `json:"run_id"`
	Agent          DefinitionRef       `json:"agent"`
	DefinitionHash string              `json:"definition_hash"`
	Selection      DefinitionSelection `json:"selection,omitempty"`
	Input          json.RawMessage     `json:"input"`
	Messages       []Message           `json:"messages,omitempty"`
	Context        []ContextBlock      `json:"context,omitempty"`
	Permissions    PermissionPolicy    `json:"permissions"`
	ToolScope      ToolScope           `json:"tool_scope"`
	Policy         RunPolicy           `json:"policy"`
	Actor          Actor               `json:"actor"`
	Correlation    Correlation         `json:"correlation"`
}

// RunStart fixes the identity and capability ceiling before scenario preparation.
type RunStart struct {
	RunID          string              `json:"run_id"`
	Agent          DefinitionRef       `json:"agent"`
	DefinitionHash string              `json:"definition_hash"`
	Selection      DefinitionSelection `json:"selection,omitempty"`
	Input          json.RawMessage     `json:"input"`
	Permissions    PermissionPolicy    `json:"permissions"`
	ToolScope      ToolScope           `json:"tool_scope"`
	Policy         RunPolicy           `json:"policy"`
	Actor          Actor               `json:"actor"`
	Correlation    Correlation         `json:"correlation"`
}

// RunSnapshot pins all mutable control-plane choices before execution starts.
type RunSnapshot struct {
	RunID               string              `json:"run_id"`
	AgentID             string              `json:"agent_id"`
	DefinitionVersion   int64               `json:"definition_version"`
	DefinitionHash      string              `json:"definition_hash"`
	Selection           DefinitionSelection `json:"selection,omitempty"`
	Provider            string              `json:"provider"`
	Model               string              `json:"model"`
	ModelParameters     map[string]any      `json:"model_parameters,omitempty"`
	ToolSnapshotID      string              `json:"tool_snapshot_id"`
	VisibleToolIDs      []string            `json:"visible_tool_ids"`
	InputSchemaVersion  int64               `json:"input_schema_version"`
	OutputSchemaVersion int64               `json:"output_schema_version"`
	PromptHash          string              `json:"prompt_hash"`
	ContextHash         string              `json:"context_hash"`
	Budget              BudgetPolicy        `json:"budget"`
	Permissions         PermissionPolicy    `json:"permissions"`
	Actor               Actor               `json:"actor"`
	Correlation         Correlation         `json:"correlation"`
	CreatedAt           time.Time           `json:"created_at"`
}

type RunStatus string

const (
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	CostMicros      int64 `json:"cost_micros"`
}

type RunError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type EvidenceSummary struct {
	Status             string `json:"status"`
	ForcedConclusion   bool   `json:"forced_conclusion"`
	ToolCallCount      int    `json:"tool_call_count"`
	ResultCount        int    `json:"result_count"`
	ToolFailureCount   int    `json:"tool_failure_count"`
	PartialResultCount int    `json:"partial_result_count"`
	OmittedItemCount   int    `json:"omitted_item_count"`
}

type RunResult struct {
	RunID      string          `json:"run_id"`
	Status     RunStatus       `json:"status"`
	Output     json.RawMessage `json:"output,omitempty"`
	Text       string          `json:"text,omitempty"`
	Evidence   EvidenceSummary `json:"evidence"`
	References []Reference     `json:"references,omitempty"`
	Messages   []Message       `json:"messages,omitempty"`
	Usage      Usage           `json:"usage"`
	Error      *RunError       `json:"error,omitempty"`
}

// Runtime executes one already-compiled request against a pinned definition.
type Runtime interface {
	Run(ctx context.Context, request RunRequest) (RunResult, error)
}

// ManagedRun keeps preparation accounting and the scenario commit boundary on one Run.
type ManagedRun interface {
	Context(context.Context) context.Context
	Execute(context.Context, RunRequest) (RunResult, error)
	Finish(*RunError) error
}

// ManagedRuntime starts Runs before a scenario performs model-backed preparation.
type ManagedRuntime interface {
	Runtime
	Begin(context.Context, RunStart) (ManagedRun, error)
}
