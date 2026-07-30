package tool

import (
	"context"
	"strings"
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

// SchemaType is the JSON Schema primitive vocabulary shared by tool owners.
type SchemaType string

// Supported JSON Schema primitive types.
const (
	TypeObject SchemaType = "object"
	TypeArray  SchemaType = "array"
	TypeString SchemaType = "string"
	TypeInt    SchemaType = "integer"
	TypeNumber SchemaType = "number"
	TypeBool   SchemaType = "boolean"
)

// Arguments contains one validated tool invocation.
type Arguments map[string]any

// String returns a trimmed string argument.
func (args Arguments) String(key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

// StringDefault returns fallback when the argument is empty or not a string.
func (args Arguments) StringDefault(key, fallback string) string {
	if value := args.String(key); value != "" {
		return value
	}
	return fallback
}

// Int returns an integer argument decoded from JSON or fallback.
func (args Arguments) Int(key string, fallback int) int {
	switch value := args[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return fallback
	}
}

// BoundedInt constrains an integer argument to the inclusive bounds.
func (args Arguments) BoundedInt(key string, fallback, low, high int) int {
	return min(max(args.Int(key, fallback), low), high)
}

// Bool returns a boolean argument or false.
func (args Arguments) Bool(key string) bool {
	value, _ := args[key].(bool)
	return value
}

// Object returns a nested argument object or nil.
func (args Arguments) Object(key string) Arguments {
	value, _ := args[key].(map[string]any)
	return Arguments(value)
}

// Time returns an optional RFC3339 timestamp argument.
func (args Arguments) Time(key string) (time.Time, error) {
	value := args.String(key)
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

// Strings returns trimmed, non-empty string elements.
func (args Arguments) Strings(key string) []string {
	switch raw := args[key].(type) {
	case []any:
		out := make([]string, 0, len(raw))
		for _, value := range raw {
			text, ok := value.(string)
			text = strings.TrimSpace(text)
			if ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(raw))
		for _, value := range raw {
			if value = strings.TrimSpace(value); value != "" {
				out = append(out, value)
			}
		}
		return out
	default:
		return nil
	}
}

// TimeRange is an authoritative server-resolved interval for time-aware tools.
type TimeRange struct {
	From        time.Time
	To          time.Time
	ToExclusive bool
	Raw         string
}

type timeRangeKey struct{}

// WithTimeRange pins one resolved interval to a tool execution context.
func WithTimeRange(ctx context.Context, value TimeRange) context.Context {
	return context.WithValue(ctx, timeRangeKey{}, value)
}

// TimeRangeFromContext returns the server-resolved interval, when present.
func TimeRangeFromContext(ctx context.Context) (TimeRange, bool) {
	value, ok := ctx.Value(timeRangeKey{}).(TimeRange)
	return value, ok
}

// Reference identifies evidence returned by a tool.
type Reference struct {
	Type   string `json:"type"`
	Label  string `json:"label"`
	Target string `json:"target"`
}

// EvidenceCoverage reports bounded omissions made by the tool owner.
type EvidenceCoverage struct {
	Partial      bool `json:"partial,omitempty"`
	OmittedItems int  `json:"omitted_items,omitempty"`
}

// Result is the bounded content passed back to the caller.
type Result struct {
	Content    string           `json:"content"`
	References []Reference      `json:"references,omitempty"`
	Coverage   EvidenceCoverage `json:"coverage,omitempty"`
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
	Intent   string
	Temporal bool
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
