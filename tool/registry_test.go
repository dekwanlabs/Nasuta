package tool

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type panicExecutionObserver struct{}

func (panicExecutionObserver) OnToolExecution(context.Context, Execution) {
	panic("observer failed")
}

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

func TestExecutorObserverPanicDoesNotReplaceToolResult(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(testTool("observed", "ok")); err != nil {
		t.Fatal(err)
	}
	ctx := WithExecutionObserver(t.Context(), panicExecutionObserver{})
	result, err := NewExecutor(0).Execute(ctx, registry.Snapshot(ReadPolicy()), "observed", Arguments{})
	if err != nil || result.Content != "ok" {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}

func TestExecutorRejectsUnknownArgumentsWhenSchemaIsClosed(t *testing.T) {
	registry := NewRegistry()
	candidate := testTool("closed", "ok")
	candidate.InputSchema = JSONSchema{
		"type": TypeObject,
		"properties": map[string]any{
			"query": map[string]any{"type": TypeString},
		},
		"additionalProperties": false,
	}
	if err := registry.Register(candidate); err != nil {
		t.Fatal(err)
	}
	_, err := NewExecutor(0).Execute(context.Background(), registry.Snapshot(ReadPolicy()), "closed", Arguments{
		"query": "device", "url": "/ignored",
	})
	if err == nil || !strings.Contains(err.Error(), "arguments.url is not allowed") {
		t.Fatalf("unknown argument error = %v", err)
	}
}

func TestExecutorEnforcesSchemaConstraints(t *testing.T) {
	registry := NewRegistry()
	candidate := testTool("constraints", "ok")
	candidate.InputSchema = JSONSchema{
		"type": TypeObject,
		"properties": map[string]any{
			"limit": map[string]any{"type": TypeInt, "minimum": 1, "maximum": 20},
			"items": map[string]any{
				"type": TypeArray, "minItems": 1,
				"items": map[string]any{
					"oneOf": []any{
						map[string]any{"type": TypeString, "const": "logs"},
						map[string]any{"type": TypeInt, "minimum": 10},
					},
				},
			},
		},
	}
	if err := registry.Register(candidate); err != nil {
		t.Fatal(err)
	}
	executor := NewExecutor(0)
	snapshot := registry.Snapshot(ReadPolicy())
	for _, args := range []Arguments{
		{"limit": 0},
		{"limit": 21},
		{"items": []any{}},
		{"items": []any{"traces"}},
		{"items": []any{9}},
	} {
		if _, err := executor.Execute(context.Background(), snapshot, "constraints", args); err == nil {
			t.Fatalf("executor accepted %#v", args)
		}
	}
	if _, err := executor.Execute(context.Background(), snapshot, "constraints", Arguments{
		"limit": 20, "items": []any{"logs", 10},
	}); err != nil {
		t.Fatalf("valid constrained arguments: %v", err)
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

func TestSnapshotIDPinsRevisionAndVisibleTools(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(testTool("first", "v1")); err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot(ReadPolicy())
	if snapshot.ID() == "" || snapshot.ID() != registry.Snapshot(ReadPolicy()).ID() {
		t.Fatal("snapshot id is empty or unstable")
	}
	if err := registry.Register(testTool("second", "v1")); err != nil {
		t.Fatal(err)
	}
	if snapshot.ID() == registry.Snapshot(ReadPolicy()).ID() {
		t.Fatal("snapshot id did not change after publication")
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

func TestReadRegistryReconcilesOwnedToolSet(t *testing.T) {
	registry := NewRegistry()
	publisher := NewReadRegistry(registry)
	if err := publisher.Reconcile(ReadToolSet{
		Owner: "scenario.observe",
		Tools: []ReadTool{
			testReadTool("logs", "v1"),
			testReadTool("traces", "v1"),
		},
	}); err != nil {
		t.Fatal(err)
	}
	old := registry.Snapshot(ReadPolicy())

	if err := publisher.Reconcile(ReadToolSet{
		Owner: "scenario.observe",
		Tools: []ReadTool{testReadTool("logs", "v2")},
	}); err != nil {
		t.Fatal(err)
	}
	current := registry.Snapshot(ReadPolicy())
	if _, ok := current.Get("traces"); ok {
		t.Fatal("reconcile retained an omitted owned tool")
	}
	result, err := NewExecutor(0).Execute(context.Background(), current, "logs", Arguments{})
	if err != nil {
		t.Fatal(err)
	}
	oldResult, err := NewExecutor(0).Execute(context.Background(), old, "logs", Arguments{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "v2" || oldResult.Content != "v1" {
		t.Fatalf("snapshot results = current %q old %q", result.Content, oldResult.Content)
	}

	if err := publisher.Reconcile(ReadToolSet{Owner: "scenario.observe"}); err != nil {
		t.Fatal(err)
	}
	if got := registry.Snapshot(ReadPolicy()).Tools(); len(got) != 0 {
		t.Fatalf("empty desired set retained %d tools", len(got))
	}
}

func TestReadRegistryReconcileRejectsForeignIDsAtomically(t *testing.T) {
	registry := NewRegistry()
	publisher := NewReadRegistry(registry)
	if err := registry.Register(testTool("builtin", "platform")); err != nil {
		t.Fatal(err)
	}
	err := publisher.Reconcile(ReadToolSet{
		Owner: "scenario.observe",
		Tools: []ReadTool{
			testReadTool("new", "scenario"),
			testReadTool("builtin", "replacement"),
		},
	})
	if err == nil {
		t.Fatal("reconcile replaced an unowned tool")
	}
	snapshot := registry.Snapshot(ReadPolicy())
	if _, ok := snapshot.Get("new"); ok {
		t.Fatal("reconcile partially published before the ownership conflict")
	}
	result, execErr := NewExecutor(0).Execute(context.Background(), snapshot, "builtin", Arguments{})
	if execErr != nil {
		t.Fatal(execErr)
	}
	if result.Content != "platform" {
		t.Fatalf("builtin result = %q", result.Content)
	}
}

func TestReadRegistryReconcileCannotReplaceWriteTool(t *testing.T) {
	registry := NewRegistry()
	write := testTool("write", "pending")
	write.Kind = KindWrite
	if err := registry.Register(write); err != nil {
		t.Fatal(err)
	}
	err := NewReadRegistry(registry).Reconcile(ReadToolSet{
		Owner: "scenario.observe",
		Tools: []ReadTool{testReadTool("write", "read")},
	})
	if err == nil {
		t.Fatal("reconcile replaced a write tool")
	}
	candidate, ok := registry.Snapshot(AllPolicy()).Get("write")
	if !ok || candidate.Kind != KindWrite {
		t.Fatalf("write tool = %#v, ok=%v", candidate, ok)
	}
}

func TestCandidateToolsAreDerivedFromCurrentSnapshot(t *testing.T) {
	registry := NewRegistry()
	handler := HandlerFunc(func(context.Context, Arguments) (Result, error) {
		return Result{Content: "ok"}, nil
	})
	runbook := Tool{
		ID: "runbook_reader", Description: "reads runbooks", Kind: KindRead,
		InputSchema: JSONSchema{"type": "object", "properties": map[string]any{
			"doc_id": map[string]any{"type": "string"},
		}},
		ReferenceInputs: []ReferenceInput{{Argument: "doc_id", Accepts: []ReferenceType{ReferenceRunbook}}},
		Handler:         handler,
	}
	if err := registry.Register(runbook); err != nil {
		t.Fatal(err)
	}
	if got := registry.Snapshot(ReadPolicy()).CandidateTools(ReferenceRunbook); len(got) != 1 || got[0] != "runbook_reader" {
		t.Fatalf("candidate tools = %v", got)
	}
}

func TestReadToolReferenceDeclarationsMatchNativeTools(t *testing.T) {
	registry := NewRegistry()
	publisher := NewReadRegistry(registry)
	handler := HandlerFunc(func(context.Context, Arguments) (Result, error) {
		return Result{Content: "ok"}, nil
	})
	if err := publisher.Reconcile(ReadToolSet{
		Owner: "extension",
		Tools: []ReadTool{{
			ID: "extension_runbook", Description: "extension runbook reader",
			InputSchema: JSONSchema{"type": "object", "properties": map[string]any{
				"doc_id": map[string]any{"type": "string"},
			}},
			ReferenceInputs: []ReferenceInput{{Argument: "doc_id", Accepts: []ReferenceType{ReferenceRunbook}}},
			Handler:         handler,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot(ReadPolicy())
	candidates := snapshot.CandidateTools(ReferenceRunbook)
	if len(candidates) != 1 || candidates[0] != "extension_runbook" {
		t.Fatalf("extension candidates = %v", candidates)
	}
	registered, ok := snapshot.Get("extension_runbook")
	if !ok || len(registered.ReferenceInputs) != 1 || registered.ReferenceInputs[0].Argument != "doc_id" {
		t.Fatalf("extension declaration = %#v", registered.ReferenceInputs)
	}
}

func testReadTool(id ToolID, content string) ReadTool {
	candidate := testTool(id, content)
	return ReadTool{
		ID: candidate.ID, Description: candidate.Description, InputSchema: candidate.InputSchema,
		Handler: candidate.Handler,
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
