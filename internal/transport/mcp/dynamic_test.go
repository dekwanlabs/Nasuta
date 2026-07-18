package mcptransport

import (
	"context"
	"testing"

	toolruntime "github.com/dekwanlabs/astris/tool"
)

func TestDynamicHandlerRebuildsAfterRegistryRevision(t *testing.T) {
	registry := toolruntime.NewRegistry()
	handler := NewDynamicHandler(nil, registry)
	initial := handler.currentRevision()
	if err := registry.Register(toolruntime.Tool{
		ID:          "dynamic",
		Description: "dynamic test tool",
		Kind:        toolruntime.KindRead,
		InputSchema: toolruntime.JSONSchema{"type": "object", "properties": map[string]any{}},
		Handler: toolruntime.HandlerFunc(func(context.Context, toolruntime.Arguments) (toolruntime.Result, error) {
			return toolruntime.Result{Content: "ok"}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	handler.refresh()
	if handler.currentRevision() == initial || handler.currentRevision() != registry.Revision() {
		t.Fatalf("handler revision=%d registry=%d initial=%d", handler.currentRevision(), registry.Revision(), initial)
	}
}
