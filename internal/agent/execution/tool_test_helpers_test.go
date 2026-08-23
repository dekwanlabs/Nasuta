package execution

import (
	"context"

	"github.com/dekwanlabs/nasuta/tool"
)

type Tool = tool.Tool

const (
	ToolKindRead  = tool.KindRead
	ToolKindWrite = tool.KindWrite
)

func stringHandler(run func(context.Context, tool.Arguments) (string, error)) tool.Handler {
	return tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
		content, err := run(ctx, args)
		return tool.Result{Content: content}, err
	})
}

func objectSchema(properties map[string]any, required []string) tool.JSONSchema {
	schema := tool.JSONSchema{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func propString(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func propInt(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}
