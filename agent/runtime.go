package agent

import (
	"context"
	"encoding/json"
	"time"
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
	Source      string      `json:"source"`
	Title       string      `json:"title"`
	Content     string      `json:"content"`
	References  []Reference `json:"references,omitempty"`
	Complete    bool        `json:"complete"`
	ContentHash string      `json:"content_hash"`
}

type Reference struct {
	Type   string `json:"type"`
	Label  string `json:"label,omitempty"`
	Target string `json:"target"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type RunRequest struct {
	RunID       string          `json:"run_id"`
	Agent       DefinitionRef   `json:"agent"`
	Input       json.RawMessage `json:"input"`
	Messages    []Message       `json:"messages,omitempty"`
	Context     []ContextBlock  `json:"context,omitempty"`
	Actor       Actor           `json:"actor"`
	Correlation Correlation     `json:"correlation"`
}

// RunSnapshot pins all mutable control-plane choices before execution starts.
type RunSnapshot struct {
	RunID               string           `json:"run_id"`
	AgentID             string           `json:"agent_id"`
	DefinitionVersion   int64            `json:"definition_version"`
	DefinitionHash      string           `json:"definition_hash"`
	Provider            string           `json:"provider"`
	Model               string           `json:"model"`
	ModelParameters     map[string]any   `json:"model_parameters,omitempty"`
	ToolSnapshotID      string           `json:"tool_snapshot_id"`
	VisibleToolIDs      []string         `json:"visible_tool_ids"`
	InputSchemaVersion  int64            `json:"input_schema_version"`
	OutputSchemaVersion int64            `json:"output_schema_version"`
	PromptHash          string           `json:"prompt_hash"`
	ContextHash         string           `json:"context_hash"`
	Budget              BudgetPolicy     `json:"budget"`
	Permissions         PermissionPolicy `json:"permissions"`
	Actor               Actor            `json:"actor"`
	Correlation         Correlation      `json:"correlation"`
	CreatedAt           time.Time        `json:"created_at"`
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
}

type RunError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type RunResult struct {
	RunID      string          `json:"run_id"`
	Status     RunStatus       `json:"status"`
	Output     json.RawMessage `json:"output,omitempty"`
	Text       string          `json:"text,omitempty"`
	References []Reference     `json:"references,omitempty"`
	Usage      Usage           `json:"usage"`
	Error      *RunError       `json:"error,omitempty"`
}

// Runtime executes one already-compiled request against a pinned definition.
type Runtime interface {
	Run(ctx context.Context, request RunRequest) (RunResult, error)
}
