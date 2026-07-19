package mcp

import (
	"encoding/json"
	"testing"

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
