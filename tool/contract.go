package tool

import (
	"context"
	"time"
)

// ToolID is the stable identity shared by Agent and MCP.
type ToolID string

// Kind separates read operations from approval-gated writes.
type Kind string

const (
	KindRead  Kind = "read"
	KindWrite Kind = "write"
)

// JSONSchema is the input contract published to models and MCP clients.
type JSONSchema map[string]any

// Arguments contains one validated tool invocation.
type Arguments map[string]any

func (args Arguments) String(key string) string {
	value, _ := args[key].(string)
	return value
}

func (args Arguments) Int(key string, fallback int) int {
	switch value := args[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	default:
		return fallback
	}
}

// Reference identifies evidence returned by a tool.
type Reference struct {
	Type   string `json:"type"`
	Label  string `json:"label"`
	Target string `json:"target"`
}

// Result is the bounded content passed back to the caller.
type Result struct {
	Content    string      `json:"content"`
	References []Reference `json:"references,omitempty"`
}

// Handler executes a tool under the run context.
type Handler interface {
	Execute(context.Context, Arguments) (Result, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(context.Context, Arguments) (Result, error)

func (fn HandlerFunc) Execute(ctx context.Context, args Arguments) (Result, error) {
	return fn(ctx, args)
}

// PrefetchSpec marks a read tool as eligible for trusted prefetch planning.
type PrefetchSpec struct {
	Description string
	Timeout     time.Duration
}

// RoutingSpec makes a read tool visible only when its declared intent matches.
type RoutingSpec struct {
	Intent string
}

// Tool is the single contract consumed by Agent and MCP.
type Tool struct {
	ID          ToolID
	Description string
	Kind        Kind
	InputSchema JSONSchema
	Routing     *RoutingSpec
	Prefetch    *PrefetchSpec
	Handler     Handler
	MCPHidden   bool
}

// ReadTool is the only tool shape exposed to upper-layer compositions.
type ReadTool struct {
	ID          ToolID
	Description string
	InputSchema JSONSchema
	Routing     *RoutingSpec
	Prefetch    *PrefetchSpec
	Handler     Handler
	MCPHidden   bool
}

// ReadToolSet is one owner's complete desired read-tool catalog.
type ReadToolSet struct {
	Owner string
	Tools []ReadTool
}

func (candidate ReadTool) tool() Tool {
	return Tool{
		ID: candidate.ID, Description: candidate.Description, Kind: KindRead,
		InputSchema: candidate.InputSchema, Routing: candidate.Routing, Prefetch: candidate.Prefetch,
		Handler: candidate.Handler, MCPHidden: candidate.MCPHidden,
	}
}

// Policy is fixed when a run starts.
type Policy struct {
	AllowRead  bool
	AllowWrite bool
	AllowedIDs map[ToolID]struct{}
}

// Allows applies both kind and optional ID restrictions.
func (policy Policy) Allows(candidate Tool) bool {
	switch candidate.Kind {
	case KindRead:
		if !policy.AllowRead {
			return false
		}
	case KindWrite:
		if !policy.AllowWrite {
			return false
		}
	default:
		return false
	}
	if len(policy.AllowedIDs) == 0 {
		return true
	}
	_, ok := policy.AllowedIDs[candidate.ID]
	return ok
}

// ReadPolicy exposes every registered read tool.
func ReadPolicy() Policy {
	return Policy{AllowRead: true}
}

// AllPolicy exposes every registered tool.
func AllPolicy() Policy {
	return Policy{AllowRead: true, AllowWrite: true}
}
