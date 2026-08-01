package mcp

import (
	"context"
	"testing"

	"github.com/dekwanlabs/nasuta/tool"
)

func TestDynamicHandlerRebuildsAfterRegistryRevision(t *testing.T) {
	registry := tool.NewRegistry()
	handler := NewDynamicHandler(registry)
	initial := handler.currentRevision()
	if err := registry.Register(tool.Tool{
		ID:          "dynamic",
		Description: "dynamic test tool",
		Kind:        tool.KindRead,
		InputSchema: tool.JSONSchema{"type": "object", "properties": map[string]any{}},
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{Content: "ok"}, nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	handler.refresh()
	if handler.currentRevision() == initial || handler.currentRevision() != registry.Revision() {
		t.Fatalf("handler revision=%d registry=%d initial=%d", handler.currentRevision(), registry.Revision(), initial)
	}
}
