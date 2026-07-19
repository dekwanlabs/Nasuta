package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/graph"
	"github.com/dekwanlabs/nasuta/tool"
)

type Tool = tool.Tool
type Registry = tool.Registry
type ToolPolicy = tool.Policy

const (
	ToolKindRead  = tool.KindRead
	ToolKindWrite = tool.KindWrite
)

// ToolPolicyForPlan fixes the tool permission set for one run.
func ToolPolicyForPlan(_ domain.EvidencePlan, allowWrite bool) ToolPolicy {
	return ToolPolicy{
		AllowRead:  true,
		AllowWrite: allowWrite,
	}
}

// NewRegistry registers every built-in tool through the public batch API.
func NewRegistry(svc *Service, cfg config.Config) *Registry {
	registry := tool.NewRegistry()
	if err := registry.RegisterAll(builtinTools(svc, cfg)); err != nil {
		panic(fmt.Sprintf("register built-in tools: %v", err))
	}
	return registry
}

func builtinTools(svc *Service, cfg config.Config) []Tool {
	tools := []Tool{
		{
			ID: "get_service",
			Description: "Look up backend services by name, module path, owner, tag, or keyword. " +
				"Use this first when you need to locate a service or understand what a service is before changing it.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"query": propString("Service name, module path, owner, or keyword."),
				"limit": propInt("Max results (default 10)."),
			}, []string{"query"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.ServiceLookup(ctx, argStr(args, "query", ""), argInt(args, "limit", 10)))
			}),
		},
		{
			ID: "trace_deps",
			Description: "Return upstream/downstream dependency edges for a service. " +
				"Use this to assess blast radius before changing an API, deprecating a service, or diagnosing cascading failures.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"service":   propString("Service name to inspect."),
				"direction": propString("upstream | downstream | both (default both)."),
				"depth":     propInt("Traversal depth 1-5 (default 2)."),
			}, []string{"service"}),
			Handler: stringHandler(func(_ context.Context, args tool.Arguments) (string, error) {
				depth := clampInt(argInt(args, "depth", 2), 1, 5)
				result := svc.TraceDeps(argStr(args, "service", ""), argStr(args, "direction", "both"), depth)
				return marshalResult(graphResultToMap(result))
			}),
		},
		{
			ID: "list_apis",
			Description: "Find Java Controller or Python FastAPI endpoints by service and/or path keyword. " +
				"Use this to locate which handler serves a given route before editing it.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"service":     propString("Optional service name filter."),
				"pathKeyword": propString("Optional endpoint path keyword."),
				"limit":       propInt("Max results (default 20)."),
			}, nil),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.ListApis(ctx, argStr(args, "service", ""), argStr(args, "pathKeyword", ""), argInt(args, "limit", 20)))
			}),
		},
		{
			ID: "search_code",
			Description: "Semantic search over indexed source code, config, SQL and docs across all languages. " +
				"Use this to find where something is implemented or configured when you do not know the exact file or symbol. " +
				"Returns file path, line range and a snippet preview. Requires semantic search to be enabled.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"query": propString("Natural-language or code-intent query."),
				"lang":  propString("Optional language filter, e.g. java, python, go, sql, yaml."),
				"limit": propInt("Max results (default 10)."),
			}, []string{"query"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.CodeSearch(ctx, argStr(args, "query", ""), argStr(args, "lang", ""), argInt(args, "limit", 10)))
			}),
		},
		{
			ID: "get_symbol",
			Description: "Query the codegraph index for function-level definitions and source bodies. " +
				"Use this when you need the exact implementation of a function, method, class, or interface.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"query": propString("Function name, class name, or service keyword to look up."),
				"limit": propInt("Max nodes to return (default 5, max 10)."),
			}, []string{"query"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.GetSymbol(ctx, argStr(args, "query", ""), argInt(args, "limit", 5)))
			}),
		},
		{
			ID: "trace_calls",
			Description: "Trace callers or callees of a function using the codegraph index. " +
				"Use this to follow a method-level call chain after locating a concrete symbol.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"query":     propString("Function or method name to trace."),
				"direction": propString("callers | callees (default callers)."),
				"limit":     propInt("Max related nodes to return (default 5, max 10)."),
			}, []string{"query", "direction"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.TraceCalls(ctx,
					argStr(args, "query", ""),
					argStr(args, "direction", "callers"),
					argInt(args, "limit", 5)))
			}),
		},
		{
			ID: "search_runbooks",
			Description: "Search runbooks by symptom, task type, or tag. " +
				"Use this when investigating a failure or before a risky change to find the relevant playbook.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"query": propString("Symptom, task, or keyword to search."),
				"limit": propInt("Max results (default 10)."),
			}, []string{"query"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.RunbookSearch(ctx, argStr(args, "query", ""), argInt(args, "limit", 10), false, ""))
			}),
		},
		{
			ID:          "check_docs",
			Description: "Check whether a service has enough documentation and code evidence.",
			Kind:        ToolKindRead,
			InputSchema: objectSchema(map[string]any{"service": propString("Service name to check.")}, []string{"service"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.DocGapCheck(ctx, argStr(args, "service", "")))
			}),
		},
		{
			ID:          "index_stats",
			Description: "Return summary counts for the current backend knowledge index.",
			Kind:        ToolKindRead,
			InputSchema: objectSchema(map[string]any{}, nil),
			Handler: stringHandler(func(ctx context.Context, _ tool.Arguments) (string, error) {
				return marshalResult(svc.IndexSummary(ctx))
			}),
		},
	}

	if !cfg.WebSearchEnabled {
		return tools
	}
	tools = append(tools,
		Tool{
			ID: "web_search",
			Description: "Search the configured web backend for external documentation or current facts. " +
				"After searching, fetch the most relevant primary source pages.",
			Kind:      ToolKindRead,
			MCPHidden: !cfg.WebSearchMCPEnabled,
			InputSchema: objectSchema(map[string]any{
				"query": propString("Search query string."),
				"limit": propInt("Max results (default 5, max 10)."),
			}, []string{"query"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				results, err := svc.WebSearch(ctx, argStr(args, "query", ""), argInt(args, "limit", 5))
				if err != nil {
					return "", err
				}
				return marshalResult(results)
			}),
		},
		Tool{
			ID: "web_fetch",
			Description: "Fetch and extract relevant readable content from a web page. " +
				"Pass the current research question so local ranking can select the most useful passages.",
			Kind:      ToolKindRead,
			MCPHidden: !cfg.WebSearchMCPEnabled,
			InputSchema: objectSchema(map[string]any{
				"url":   propString("Full HTTPS URL to fetch."),
				"query": propString("Current question or evidence need used to rank passages locally."),
			}, []string{"url"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return svc.WebFetchRelevant(ctx, argStr(args, "url", ""), argStr(args, "query", ""))
			}),
		},
	)
	return tools
}

func stringHandler(run func(context.Context, tool.Arguments) (string, error)) tool.Handler {
	return tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
		content, err := run(ctx, args)
		return tool.Result{Content: content}, err
	})
}

func objectSchema(props map[string]any, required []string) tool.JSONSchema {
	schema := tool.JSONSchema{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func propString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func propInt(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func argStr(args tool.Arguments, key, fallback string) string {
	if value := args.String(key); value != "" {
		return value
	}
	return fallback
}

func argInt(args tool.Arguments, key string, fallback int) int {
	return args.Int(key, fallback)
}

func clampInt(value, low, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func marshalResult(value any) (string, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func graphResultToMap(result graph.Result) map[string]any {
	upstream := make([]map[string]any, len(result.Upstream))
	for i, edge := range result.Upstream {
		upstream[i] = map[string]any{"from": edge.From, "to": edge.To}
	}
	downstream := make([]map[string]any, len(result.Downstream))
	for i, edge := range result.Downstream {
		downstream[i] = map[string]any{"from": edge.From, "to": edge.To}
	}
	return map[string]any{"upstream": upstream, "downstream": downstream}
}
