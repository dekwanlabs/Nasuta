// Package tracecontract defines versioned execution-trace wire contracts.
package tracecontract

// V1 identifies the first stable execution-trace wire contract.
const V1 = "v1"

// EventV1 is a read-only execution event shared by dashboard and MCP traces.
type EventV1 struct {
	TraceID        string         `json:"trace_id,omitempty"`
	RunID          string         `json:"run_id,omitempty"`
	ParentRunID    string         `json:"parent_run_id,omitempty"`
	WorkflowRunID  string         `json:"workflow_run_id,omitempty"`
	AgentRunID     string         `json:"agent_run_id,omitempty"`
	WorkflowNodeID string         `json:"workflow_node_id,omitempty"`
	Sequence       int            `json:"sequence,omitempty"`
	Node           string         `json:"node"`
	Status         string         `json:"status"`
	ElapsedMS      int64          `json:"elapsed_ms,omitempty"`
	DurationMS     int64          `json:"duration_ms,omitempty"`
	Input          map[string]any `json:"input,omitempty"`
	Output         map[string]any `json:"output,omitempty"`
}
