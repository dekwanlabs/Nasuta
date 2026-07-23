package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/ontology"
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
			Description: "Look up service metadata and location by name, module path, owner, tag, or keyword. " +
				"It does not establish dependencies, endpoints, or implementation behavior.",
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
			Description: "Return indexed upstream/downstream service-level dependency edges for blast-radius analysis. " +
				"It does not establish method-level callers, callees, or execution order.",
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
			Description: "Authoritatively look up indexed complete API routes and handlers by service and/or path keyword. " +
				"Use this before claiming an endpoint; Java routes combine class-level and method-level mappings. Do not compose a route from partial code annotations.",
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
				"Use it as a fallback to discover an unknown implementation or configuration, not as proof of an exact symbol, complete API route, or call chain. " +
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
			Description: "Query the codegraph index for exact definitions and source bodies of functions, methods, classes, or interfaces. " +
				"A definition does not establish its callers or callees; use call tracing for those edges.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"query":          propString("Function name, class name, or service keyword to look up."),
				"file":           propString("Optional canonical repos/... path scope."),
				"qualified_name": propString("Optional exact qualified name."),
				"limit":          propInt("Max nodes to return (default 5, max 10)."),
			}, []string{"query"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.GetSymbolFiltered(ctx, argStr(args, "query", ""),
					argStr(args, "file", ""), argStr(args, "qualified_name", ""), argInt(args, "limit", 5)))
			}),
		},
		{
			ID: "trace_calls",
			Description: "Trace method-level callers and callees from a symbol or exact code-hit location. " +
				"Verified service_route hops support cross-service calls; truncated or unresolved frontiers are incomplete, and this is not proof of complete service dependencies.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"query":          propString("Function or method name to trace when file+line is unavailable."),
				"file":           propString("Optional canonical repos/... source path used for exact location or disambiguation."),
				"line":           propInt("Optional source line; use with file for an exact semantic-hit start."),
				"qualified_name": propString("Optional exact qualified name used to disambiguate overloaded names."),
				"direction":      propString("callers | callees | both (default both)."),
				"max_depth":      propInt("Traversal depth 1-8 (default 3)."),
				"max_nodes":      propInt("Distinct node budget 1-200 (default 40)."),
				"max_fanout":     propInt("Per-node call-edge budget 1-100 (default 20)."),
			}, nil),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.TraceCalls(ctx, callchain.Request{
					Query: argStr(args, "query", ""), File: argStr(args, "file", ""),
					Line: argInt(args, "line", 0), QualifiedName: argStr(args, "qualified_name", ""),
					Direction: argStr(args, "direction", "both"), MaxDepth: argInt(args, "max_depth", 3),
					MaxNodes: argInt(args, "max_nodes", 40), MaxFanout: argInt(args, "max_fanout", 20),
				}))
			}),
		},
		{
			ID: "search_runbooks",
			Description: "Search operational runbooks by symptom, task type, or tag for procedures and recovery guidance. " +
				"Runbooks describe intended operations and do not prove current runtime state or executed behavior.",
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
			Description: "Check documentation coverage and evidence gaps for a service. It does not establish runtime or business facts.",
			Kind:        ToolKindRead,
			InputSchema: objectSchema(map[string]any{"service": propString("Service name to check.")}, []string{"service"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				return marshalResult(svc.DocGapCheck(ctx, argStr(args, "service", "")))
			}),
		},
		{
			ID:          "index_stats",
			Description: "Return health and summary counts for the current knowledge index. It does not establish business behavior or runtime state.",
			Kind:        ToolKindRead,
			InputSchema: objectSchema(map[string]any{}, nil),
			Handler: stringHandler(func(ctx context.Context, _ tool.Arguments) (string, error) {
				return marshalResult(svc.IndexSummary(ctx))
			}),
		},
	}
	if svc.ontology != nil {
		tools = append(tools, relationTool(svc))
	}

	if !cfg.WebSearchEnabled {
		return tools
	}
	tools = append(tools,
		Tool{
			ID: "web_search",
			Description: "Search the configured web backend for external documentation or current facts. " +
				"This call automatically fetches the highest-ranked result and returns bounded page evidence together with the candidates.",
			Kind:      ToolKindRead,
			MCPHidden: !cfg.WebSearchMCPEnabled,
			InputSchema: objectSchema(map[string]any{
				"query": propString("Search query string."),
				"limit": propInt("Max results (default 5, max 10)."),
			}, []string{"query"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				results, err := svc.WebSearchWithFetch(ctx, argStr(args, "query", ""), argInt(args, "limit", 5))
				if err != nil {
					return "", err
				}
				return marshalResult(results)
			}),
		},
	)
	return tools
}

func relationTool(svc *Service) Tool {
	return Tool{
		ID: "query_relations",
		Description: "Query evidence-backed relationships in the current indexed ontology across services, APIs, symbols, dependencies, and runbooks. " +
			"Multi-hop paths show reachability rather than a new direct fact; an empty result only means the current snapshot has no matching evidence, and truncated results are incomplete.",
		Kind: ToolKindRead,
		InputSchema: objectSchema(map[string]any{
			"entity":       propString("Entity name, canonical key, alias, or ID."),
			"entity_class": propString("Optional class: repository | service | api_endpoint | code_symbol | external_system | runbook."),
			"relations":    propStringArray("Optional predicates: contains, exposes, implemented_by, depends_on, documented_by."),
			"direction":    propString("outgoing | incoming | both (default outgoing)."),
			"max_depth":    propInt("Traversal depth 1-5 (default 2)."),
			"max_nodes":    propInt("Distinct node budget 1-500 (default 50)."),
			"max_fanout":   propInt("Per-node fact budget 1-100 (default 20)."),
		}, []string{"entity"}),
		Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
			predicates := make([]ontology.Predicate, 0)
			for _, value := range argStrings(args, "relations") {
				predicates = append(predicates, ontology.Predicate(value))
			}
			result, err := svc.ontology.QueryRelations(ctx, ontology.RelationQuery{
				Entity: argStr(args, "entity", ""), EntityClass: ontology.Class(argStr(args, "entity_class", "")),
				Predicates: predicates, Direction: ontology.Direction(argStr(args, "direction", "outgoing")),
				MaxDepth: argInt(args, "max_depth", 2), MaxNodes: argInt(args, "max_nodes", 50),
				MaxFanout: argInt(args, "max_fanout", 20),
			})
			if err != nil {
				return "", err
			}
			return marshalResult(result)
		}),
	}
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

func propStringArray(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
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

func argStrings(args tool.Arguments, key string) []string {
	value, _ := args[key].([]any)
	out := make([]string, 0, len(value))
	for _, item := range value {
		if text, ok := item.(string); ok && text != "" {
			out = append(out, text)
		}
	}
	return out
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
