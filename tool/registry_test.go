package tool

import (
	"context"
	"errors"
	"testing"
)

func TestRegisterAllIsAtomic(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(testTool("existing", "v1")); err != nil {
		t.Fatal(err)
	}
	err := registry.RegisterAll([]Tool{
		testTool("new", "v1"),
		testTool("existing", "v2"),
	})
	if err == nil {
		t.Fatal("RegisterAll accepted a conflicting batch")
	}
	snapshot := registry.Snapshot(AllPolicy())
	if _, ok := snapshot.Get("new"); ok {
		t.Fatal("RegisterAll partially published the batch")
	}
}

func TestSnapshotPinsReplacedHandler(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(testTool("versioned", "v1")); err != nil {
		t.Fatal(err)
	}
	old := registry.Snapshot(ReadPolicy())
	if err := registry.Replace(testTool("versioned", "v2")); err != nil {
		t.Fatal(err)
	}
	current := registry.Snapshot(ReadPolicy())
	executor := NewExecutor(0)

	oldResult, err := executor.Execute(context.Background(), old, "versioned", Arguments{})
	if err != nil {
		t.Fatal(err)
	}
	currentResult, err := executor.Execute(context.Background(), current, "versioned", Arguments{})
	if err != nil {
		t.Fatal(err)
	}
	if oldResult.Content != "v1" || currentResult.Content != "v2" {
		t.Fatalf("snapshot results = %q, %q", oldResult.Content, currentResult.Content)
	}
}

func TestInvalidSchemaRejectsWholeBatch(t *testing.T) {
	registry := NewRegistry()
	valid := testTool("valid", "ok")
	invalid := testTool("invalid", "bad")
	invalid.InputSchema = JSONSchema{"type": "object"}
	if err := registry.RegisterAll([]Tool{valid, invalid}); err == nil {
		t.Fatal("RegisterAll accepted invalid schema")
	}
	if got := registry.Snapshot(AllPolicy()).Tools(); len(got) != 0 {
		t.Fatalf("registry contains %d tools after rejected batch", len(got))
	}
}

func TestExecutorValidatesRequiredArguments(t *testing.T) {
	registry := NewRegistry()
	candidate := testTool("required", "ok")
	candidate.InputSchema = JSONSchema{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{"type": "string"},
		},
		"required": []string{"query"},
	}
	if err := registry.Register(candidate); err != nil {
		t.Fatal(err)
	}
	_, err := NewExecutor(0).Execute(context.Background(), registry.Snapshot(ReadPolicy()), "required", Arguments{})
	if err == nil {
		t.Fatal("executor accepted missing required argument")
	}
}

func TestEmptyBatchDoesNotAdvanceRevision(t *testing.T) {
	registry := NewRegistry()
	revision := registry.Revision()
	if err := registry.RegisterAll(nil); err != nil {
		t.Fatal(err)
	}
	if registry.Revision() != revision {
		t.Fatalf("empty batch advanced revision from %d to %d", revision, registry.Revision())
	}
}

func TestSnapshotSchemaMutationDoesNotChangeRegistry(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(testTool("immutable", "ok")); err != nil {
		t.Fatal(err)
	}
	first := registry.Snapshot(ReadPolicy())
	candidate, _ := first.Get("immutable")
	candidate.InputSchema["type"] = "string"
	current, _ := registry.Snapshot(ReadPolicy()).Get("immutable")
	if current.InputSchema["type"] != "object" {
		t.Fatalf("snapshot mutation changed registry schema: %#v", current.InputSchema)
	}
}

func TestReadRegistryCannotReplaceOrRemoveWriteTools(t *testing.T) {
	registry := NewRegistry()
	write := testTool("write", "pending")
	write.Kind = KindWrite
	if err := registry.Register(write); err != nil {
		t.Fatal(err)
	}
	publisher := NewReadRegistry(registry)
	read := ReadTool{
		ID: "write", Description: "read replacement",
		InputSchema: JSONSchema{"type": "object", "properties": map[string]any{}},
		Handler: HandlerFunc(func(context.Context, Arguments) (Result, error) {
			return Result{Content: "read"}, nil
		}),
	}
	if err := publisher.Replace(read); err == nil {
		t.Fatal("read publisher replaced a write tool")
	}
	if err := publisher.Unregister("write"); err == nil {
		t.Fatal("read publisher removed a write tool")
	}
	if _, ok := registry.Snapshot(AllPolicy()).Get("write"); !ok {
		t.Fatal("write tool disappeared after rejected read operations")
	}
}

func TestReadRegistryPublishesReadTools(t *testing.T) {
	registry := NewRegistry()
	publisher := NewReadRegistry(registry)
	if err := publisher.Register(ReadTool{
		ID: "read", Description: "read tool",
		InputSchema: JSONSchema{"type": "object", "properties": map[string]any{}},
		Handler: HandlerFunc(func(context.Context, Arguments) (Result, error) {
			return Result{Content: "ok"}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	candidate, ok := registry.Snapshot(ReadPolicy()).Get("read")
	if !ok || candidate.Kind != KindRead {
		t.Fatalf("published tool = %#v, ok=%v", candidate, ok)
	}
}

func testTool(id ToolID, content string) Tool {
	return Tool{
		ID:          id,
		Description: "test tool",
		Kind:        KindRead,
		InputSchema: JSONSchema{"type": "object", "properties": map[string]any{}},
		Handler: HandlerFunc(func(context.Context, Arguments) (Result, error) {
			if content == "error" {
				return Result{}, errors.New("failed")
			}
			return Result{Content: content}, nil
		}),
	}
}
