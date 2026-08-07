package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestAttachTracePreservesObjectResult(t *testing.T) {
	result := attachTrace(`{"matches":[{"path":"a.go"}]}`, []domain.EvaluationTrace{{Node: "vector_search", Status: "completed"}})
	var object map[string]any
	if err := json.Unmarshal([]byte(result), &object); err != nil {
		t.Fatal(err)
	}
	if _, ok := object["matches"]; !ok {
		t.Fatalf("result = %s", result)
	}
	trace, ok := object["_trace"].([]any)
	if !ok || len(trace) != 1 {
		t.Fatalf("trace = %#v", object["_trace"])
	}
}

func TestSchemaWithTracePreservesNestedContract(t *testing.T) {
	raw, err := schemaWithTrace(tool.JSONSchema{
		"type": "object",
		"properties": map[string]any{
			"filters": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"field": map[string]any{"type": "string", "enum": []any{"service", "trace_id"}},
					},
					"required": []string{"field"},
				},
			},
		},
		"required": []string{"filters"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]any)
	if _, ok := properties["_trace"]; !ok {
		t.Fatal("_trace property missing")
	}
	filters := properties["filters"].(map[string]any)
	items := filters["items"].(map[string]any)
	field := items["properties"].(map[string]any)["field"].(map[string]any)
	if len(field["enum"].([]any)) != 2 {
		t.Fatalf("nested enum was lost: %#v", field)
	}
}

func TestAttachTraceLeavesArrayContractUnchanged(t *testing.T) {
	result := `[{"path":"a.go"}]`
	if got := attachTrace(result, []domain.EvaluationTrace{{Node: "search"}}); got != result {
		t.Fatalf("array result changed: %s", got)
	}
}

func TestBuildMCPReturnsUnifiedToolTrace(t *testing.T) {
	registry := tool.NewRegistry()
	if err := registry.Register(tool.Tool{
		ID: "lookup", Description: "Looks up a service.", Kind: tool.KindRead,
		InputSchema: tool.JSONSchema{
			"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
		},
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{Content: `{"matches":["orders"]}`}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	server, err := BuildMCP(registry)
	if err != nil {
		t.Fatal(err)
	}
	client, err := client.NewInProcessClient(server)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	initialize := mcp.InitializeRequest{}
	initialize.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initialize.Params.ClientInfo = mcp.Implementation{Name: "trace-test", Version: "1"}
	if _, err := client.Initialize(t.Context(), initialize); err != nil {
		t.Fatal(err)
	}
	request := mcp.CallToolRequest{}
	request.Params.Name = "lookup"
	request.Params.Arguments = map[string]any{"query": "orders", "_trace": true}
	result, err := client.CallTool(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("content = %#v", result.Content)
	}
	content, ok := result.Content[0].(mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T", result.Content[0])
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(content.Text), &object); err != nil {
		t.Fatal(err)
	}
	traces, ok := object["_trace"].([]any)
	if !ok || len(traces) != 1 {
		t.Fatalf("trace = %#v", object["_trace"])
	}
	trace := traces[0].(map[string]any)
	if trace["node"] != "tool_execution" || trace["status"] != "completed" || trace["sequence"] != float64(1) {
		t.Fatalf("trace = %#v", trace)
	}
}
