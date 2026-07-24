package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// DynamicHandler atomically rebuilds the MCP surface when the registry changes.
type DynamicHandler struct {
	tools    *agent.Service
	registry *agent.Registry
	mu       sync.RWMutex
	revision uint64
	handler  http.Handler
}

func NewDynamicHandler(tools *agent.Service, registry *agent.Registry) *DynamicHandler {
	if registry == nil {
		registry = agent.NewRegistry(tools, config.Config{}, nil)
	}
	handler := &DynamicHandler{tools: tools, registry: registry}
	if err := handler.rebuild(); err != nil {
		log.Errorf("[mcp] initial tool surface build failed: %v", err)
		handler.handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "MCP tool surface unavailable", http.StatusServiceUnavailable)
		})
	}
	return handler
}

func (handler *DynamicHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	handler.refresh()
	handler.mu.RLock()
	current := handler.handler
	handler.mu.RUnlock()
	current.ServeHTTP(w, r)
}

func (handler *DynamicHandler) refresh() {
	if handler.registry.Revision() != handler.currentRevision() {
		if err := handler.rebuild(); err != nil {
			log.Errorf("[mcp] tool surface rebuild failed; keeping revision %d: %v", handler.currentRevision(), err)
		}
	}
}

func (handler *DynamicHandler) currentRevision() uint64 {
	handler.mu.RLock()
	defer handler.mu.RUnlock()
	return handler.revision
}

func (handler *DynamicHandler) rebuild() error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	revision := handler.registry.Revision()
	if handler.handler != nil && handler.revision == revision {
		return nil
	}
	mcpServer, err := BuildMCP(handler.tools, handler.registry)
	if err != nil {
		return err
	}
	handler.handler = server.NewStreamableHTTPServer(mcpServer)
	handler.revision = revision
	return nil
}

// BuildMCP registers the knowledge tools on a new MCP server. Tool descriptions
// and parameter schemas come from the shared tools.Registry (ADR: single source
// of truth), so the MCP surface and the internal Agent loop never drift apart.
func BuildMCP(tools *agent.Service, registry *agent.Registry) (*server.MCPServer, error) {
	mcpServer := server.NewMCPServer("codeloom", "1.0.0",
		server.WithToolCapabilities(true),
	)

	if registry == nil {
		registry = agent.NewRegistry(tools, config.Config{}, nil)
	}
	snapshot := registry.Snapshot(tool.ReadPolicy())
	executor := tool.NewExecutor(30 * time.Second)
	for _, candidate := range snapshot.MCPTools() {
		toolID := candidate.ID
		schema, err := schemaWithTrace(candidate.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %q schema: %w", toolID, err)
		}
		mcpServer.AddTool(mcp.NewToolWithRawSchema(string(toolID), candidate.Description, schema), func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := argsMap(r)
			traceEnabled, _ := args["_trace"].(bool)
			delete(args, "_trace")
			var recorder *mcpTraceRecorder
			if traceEnabled {
				recorder = &mcpTraceRecorder{started: time.Now()}
				ctx = domain.WithTraceRecorder(ctx, recorder)
			}
			result, err := executor.Execute(ctx, snapshot, toolID, tool.Arguments(args))
			if err != nil {
				return mcp.NewToolResultError(err.Error()), nil
			}
			if recorder != nil {
				result.Content = attachTrace(result.Content, recorder.snapshot())
			}
			return mcp.NewToolResultText(result.Content), nil
		})
	}

	return mcpServer, nil
}

func schemaWithTrace(schema tool.JSONSchema) (json.RawMessage, error) {
	cloned := cloneSchemaMap(map[string]any(schema))
	properties, ok := cloned["properties"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("properties must be an object")
	}
	properties["_trace"] = map[string]any{
		"type":        "boolean",
		"description": "Include an opt-in read-only evaluation trace in object results.",
	}
	data, err := json.Marshal(cloned)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	return data, nil
}

func cloneSchemaMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		switch nested := value.(type) {
		case map[string]any:
			out[key] = cloneSchemaMap(nested)
		case []any:
			items := make([]any, len(nested))
			for i, item := range nested {
				if object, ok := item.(map[string]any); ok {
					items[i] = cloneSchemaMap(object)
				} else {
					items[i] = item
				}
			}
			out[key] = items
		case []string:
			out[key] = append([]string(nil), nested...)
		default:
			out[key] = value
		}
	}
	return out
}

type mcpTraceRecorder struct {
	mu       sync.Mutex
	started  time.Time
	sequence int
	events   []domain.EvaluationTrace
}

func (recorder *mcpTraceRecorder) RecordTrace(event domain.EvaluationTrace) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.sequence++
	event.Sequence = recorder.sequence
	if event.ElapsedMS == 0 {
		event.ElapsedMS = time.Since(recorder.started).Milliseconds()
	}
	recorder.events = append(recorder.events, event)
}

func (recorder *mcpTraceRecorder) snapshot() []domain.EvaluationTrace {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]domain.EvaluationTrace(nil), recorder.events...)
}

func attachTrace(result string, events []domain.EvaluationTrace) string {
	if len(events) == 0 {
		return result
	}
	var object map[string]any
	if json.Unmarshal([]byte(result), &object) != nil || object == nil {
		return result
	}
	object["_trace"] = events
	encoded, err := json.Marshal(object)
	if err != nil {
		return result
	}
	return string(encoded)
}

// argsMap extracts the arguments map from a CallToolRequest, tolerating nil.
func argsMap(r mcp.CallToolRequest) map[string]any {
	a := r.GetArguments()
	if a == nil {
		return map[string]any{}
	}
	return a
}
