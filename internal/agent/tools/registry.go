package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/callchain"
	canonicalevidence "github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

type Tool = tool.Tool
type Registry = tool.Registry
type ToolPolicy = tool.Policy
type SessionHistory = session.History

const (
	ToolKindRead  = tool.KindRead
	ToolKindWrite = tool.KindWrite
)

// NewRegistry registers every built-in tool through the public batch API.
func NewRegistry(svc *Service, cfg config.Config, sessions *memory.SessionStore, history SessionHistory) *Registry {
	registry := tool.NewRegistry()
	if err := registry.RegisterAll(builtinTools(svc, cfg, sessions, history)); err != nil {
		panic(fmt.Sprintf("register built-in tools: %v", err))
	}
	return registry
}

func builtinTools(svc *Service, cfg config.Config, sessions *memory.SessionStore, history SessionHistory) []Tool {
	listAPISchema := objectSchema(map[string]any{
		"service": propString("Optional exact service name filter."),
		"keyword": propString("Optional case-insensitive path, controller, or handler keyword."),
		"limit":   propInt("Max results (default 20)."),
	}, nil)
	listAPISchema["additionalProperties"] = false
	symbolProperties := map[string]any{
		"query":          propString("Function, method, class, or interface name; do not pass a service name, document title, or runbook ID."),
		"file":           propString("Optional canonical repos/... path scope."),
		"qualified_name": propString("Optional exact qualified name; may be used without query."),
		"limit":          propInt("Max nodes to return (default 5, min 2, max 10)."),
	}
	symbolSchema := objectSchema(symbolProperties, nil)
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
			Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
				result, err := svc.FindServices(ctx, args.String("query"), clampInt(args.Int("limit", 10), 1, 100))
				if err != nil {
					return tool.Result{}, err
				}
				payload := map[string]any{"matches": result.Matches, "semantic": result.Semantic}
				content, err := marshalResult(payload)
				if err != nil {
					return tool.Result{}, err
				}
				return tool.Result{
					Content: content, References: serviceRefs(result.Matches),
					EvidenceUnits: serviceEvidenceUnits(result.Matches),
				}, nil
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
			Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
				depth := args.BoundedInt("depth", 2, 1, 5)
				result, err := svc.TraceDeps(ctx, args.String("service"), args.StringDefault("direction", "both"), depth)
				if err != nil {
					return tool.Result{}, err
				}
				content, err := marshalResult(result)
				if err != nil {
					return tool.Result{}, err
				}
				return tool.Result{
					Content: content, References: dependencyRefs(result),
					EvidenceUnits: dependencyEvidenceUnits(result),
				}, nil
			}),
		},
		{
			ID: "list_apis",
			Description: "Authoritatively look up indexed complete API routes by service and/or a path, controller, or handler keyword. " +
				"Do not use this as the first lookup for an unresolved bare class, interface, method, controller, or handler with no service, file, or qualified-name scope; reuse an exact unique definition already present, otherwise get_symbol must resolve the target uniquely first. " +
				"Use this to locate a runtime endpoint before querying logs; omit service when ownership is unknown. Java routes combine class-level and method-level mappings. " +
				"This endpoint inventory does not establish caller or callee relationships. Trace callers to find an upstream controller, then use this lookup again to resolve that controller's complete route. Do not compose a route from partial code annotations.",
			Kind:        ToolKindRead,
			InputSchema: listAPISchema,
			Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
				matches, err := svc.FindAPIs(ctx, args.String("service"), args.String("keyword"), clampInt(args.Int("limit", 20), 1, 100))
				if err != nil {
					return tool.Result{}, err
				}
				content, err := marshalResult(map[string]any{"matches": matches})
				if err != nil {
					return tool.Result{}, err
				}
				return tool.Result{
					Content: content, References: apiRefs(matches),
					EvidenceUnits: apiEvidenceUnits(matches),
				}, nil
			}),
		},
		{
			ID: "search_code",
			Description: "Semantic search over code_chunk source code, config, SQL, and repository Markdown across all languages. It does not search knowledge runbooks or retrieve a known knowledge document. " +
				"Use it as a fallback to discover an unknown implementation or configuration, not as proof of an exact symbol, complete API route, or call chain. " +
				"Returns file path, line range and a snippet preview. Requires semantic search to be enabled.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"query": propString("Natural-language or code-intent query."),
				"lang":  propString("Optional language filter, e.g. java, python, go, sql, yaml."),
				"limit": propInt("Max results (default 10)."),
			}, []string{"query"}),
			Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
				result, err := svc.SearchCode(ctx, knowledge.CodeSearchQuery{
					Query: args.String("query"), Lang: args.String("lang"), Limit: args.Int("limit", 10),
				})
				if err != nil {
					return tool.Result{}, err
				}
				content, err := marshalResult(result)
				if err != nil {
					return tool.Result{}, err
				}
				return tool.Result{
					Content: content, References: codeRefs(result.Matches),
					EvidenceUnits: codeEvidenceUnits(result),
				}, nil
			}),
		},
		{
			ID: "get_symbol",
			Description: "Query the codegraph index for exact definitions and source bodies of functions, methods, classes, or interfaces. " +
				"For an unresolved bare class, interface, or method with no service, file, or qualified-name scope and no exact unique definition already present in current evidence, this must be the first and only tool call in the initial tool round, including when the user ultimately asks for APIs or call relationships. " +
				"Returns a unique definition, an explicit bounded ambiguity candidate list, or not_found. " +
				"A definition does not establish its callers or callees; use call tracing for those edges.",
			Kind:        ToolKindRead,
			InputSchema: symbolSchema,
			Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
				result, err := svc.GetSymbolResult(ctx, args.String("query"),
					args.String("file"), args.String("qualified_name"), args.Int("limit", 5))
				if err != nil {
					return tool.Result{}, err
				}
				content, err := marshalResult(result)
				if err != nil {
					return tool.Result{}, err
				}
				return tool.Result{
					Content: content, References: symbolRefs(result),
					EvidenceUnits: symbolEvidenceUnits(result),
				}, nil
			}),
		},
		{
			ID: "trace_calls",
			Description: "Trace method-level callers and callees from a symbol or exact code-hit location. " +
				"Follow callers from an internal implementation through client adapters to locate upstream controller candidates, then use the authoritative API lookup for their complete routes. " +
				"Verified service_route hops support cross-service calls; truncated or unresolved frontiers are incomplete, and this is not proof of complete service dependencies.",
			Kind: ToolKindRead,
			InputSchema: objectSchema(map[string]any{
				"query":          propString("Function or method name to trace when file+line is unavailable."),
				"file":           propString("Optional canonical repos/... source path used for exact location or disambiguation."),
				"line":           propInt("Optional source line; use with file for an exact semantic-hit start."),
				"qualified_name": propString("Exact qualified name; may be used alone or with file to disambiguate duplicate definitions."),
				"direction":      propString("callers | callees | both (default both)."),
				"max_depth":      propInt("Traversal depth 1-8 (default 3)."),
				"max_nodes":      propInt("Distinct node budget 1-200 (default 40)."),
				"max_fanout":     propInt("Per-node call-edge budget 1-100 (default 20)."),
			}, nil),
			Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
				result, err := svc.TraceCallsResult(ctx, callchain.Request{
					Query: args.String("query"), File: args.String("file"),
					Line: args.Int("line", 0), QualifiedName: args.String("qualified_name"),
					Direction: args.StringDefault("direction", "both"), MaxDepth: args.Int("max_depth", 3),
					MaxNodes: args.Int("max_nodes", 40), MaxFanout: args.Int("max_fanout", 20),
				})
				if err != nil {
					return tool.Result{}, err
				}
				content, err := marshalResult(result)
				if err != nil {
					return tool.Result{}, err
				}
				return tool.Result{
					Content: content, References: callChainRefs(result),
					EvidenceUnits: callChainEvidenceUnits(result),
				}, nil
			}),
		},
		{
			ID: "search_runbooks",
			Description: "Search knowledge documents and operational runbooks covering system architecture, business flows, modules, schemas, business guidance, and operations. " +
				"To scope the search, copy matches[].docId exactly from a previous result; never infer it from a title, path, filename, or document content. " +
				"Knowledge documents describe intended behavior and do not prove current runtime state.",
			Kind: ToolKindRead,
			ReferenceInputs: []tool.ReferenceInput{{
				Argument: "doc_id", Accepts: []tool.ReferenceType{tool.ReferenceRunbook},
			}},
			Admission: runbookAdmissionSpec(),
			InputSchema: objectSchema(map[string]any{
				"query":  propString("Fact or behavior to verify in the knowledge corpus."),
				"doc_id": propString("Optional canonical document ID copied exactly from matches[].docId. Never use a title, path, filename, or document content."),
				"limit":  propInt("Max documents or scoped chunks (default 3, max 10)."),
			}, []string{"query"}),
			Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
				query := args.String("query")
				if query == "" {
					return tool.Result{}, fmt.Errorf("search runbooks: query is required")
				}
				result, err := svc.SearchRunbooks(ctx, knowledge.RunbookQuery{
					Query: query, DocID: args.String("doc_id"),
					Limit: args.BoundedInt("limit", 3, 1, 10),
				})
				if err != nil {
					return tool.Result{}, err
				}
				content, err := marshalResult(result)
				if err != nil {
					return tool.Result{}, err
				}
				return tool.Result{
					Content: content, References: runbookRefs(result.Matches),
					EvidenceUnits: runbookEvidenceUnits(result),
				}, nil
			}),
		},
		{
			ID:          "check_docs",
			Description: "Check documentation coverage, entrypoints, APIs, dependencies, and source-of-truth coverage for one canonical service name. It does not read or complete a knowledge document and does not establish runtime behavior.",
			Kind:        ToolKindRead,
			ReferenceInputs: []tool.ReferenceInput{{
				Argument: "service", Accepts: []tool.ReferenceType{tool.ReferenceService},
			}},
			InputSchema: objectSchema(map[string]any{"service": propString("Service name to check.")}, []string{"service"}),
			Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
				result, err := svc.CheckDocsResult(ctx, args.String("service"))
				if err != nil {
					return "", err
				}
				return marshalResult(result)
			}),
		},
	}
	if svc.ontology != nil {
		tools = append(tools, relationTool(svc))
	}
	if sessions != nil {
		tools = append(tools, sessionTurnDetailsTool(sessions))
	}
	if history != nil {
		tools = append(tools, findTurnsTool(history))
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
			Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
				response, err := svc.WebSearchWithFetch(ctx, args.String("query"), args.Int("limit", 5))
				if err != nil {
					return tool.Result{}, err
				}
				return webSearchToolResult(response)
			}),
		},
	)
	return tools
}

func sessionTurnDetailsTool(sessions *memory.SessionStore) Tool {
	return Tool{
		ID: "get_turn",
		Description: "Read one archived turn referenced by current-session state or recalled history. " +
			"Call only when exact prior wording, identifiers, tool arguments, or evidence are necessary.",
		Kind: ToolKindRead, MCPHidden: true,
		InputSchema: objectSchema(map[string]any{
			"ref": propString("One ref shown in session state or recalled history, for example cmp_xxx."),
		}, []string{"ref"}),
		Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
			scope, ok := session.ScopeFromContext(ctx)
			if !ok {
				return "", fmt.Errorf("session turn details are unavailable without a current compressed conversation")
			}
			record, err := sessions.GetTurnDetail(scope.SessionID, scope.UserID, args.String("ref"))
			if err != nil {
				return "", err
			}
			return marshalResult(record)
		}),
	}
}

func findTurnsTool(history SessionHistory) Tool {
	return Tool{
		ID: "find_turns",
		Description: "Search archived turns in the current QA session when automatically recalled history leaves a material gap. " +
			"An empty result means only that the bounded search found no match, not that the history contains no relevant fact.",
		Kind: ToolKindRead, MCPHidden: true,
		InputSchema: objectSchema(map[string]any{
			"query": propString("What to find in the current session history."),
			"limit": propInt("Maximum summaries to return (default 8, max 24)."),
		}, []string{"query"}),
		Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
			scope, ok := session.ScopeFromContext(ctx)
			if !ok {
				return "", fmt.Errorf("session history is unavailable without a current compressed conversation")
			}
			return history.Find(ctx, scope.UserID, scope.SessionID, args.String("query"), args.BoundedInt("limit", 8, 1, 24), 8192)
		}),
	}
}

func relationTool(svc *Service) Tool {
	return Tool{
		ID: "trace_relations",
		Description: "Authoritatively resolve how indexed entities relate. Use this before free-text search for: which repository contains a service; what endpoints a service exposes; which handler symbol implements an endpoint; what a service depends on, including external systems and the protocol qualifier; what depends on a service (set direction to incoming); which runbook documents a service. " +
			"Each returned fact carries file:line evidence and a confidence value. It covers only those five relations — method-level call edges require call tracing, non-handler symbols require symbol resolution, and file contents require semantic search. " +
			"Multi-hop paths show reachability rather than a new direct fact; an empty result only means the current snapshot has no matching evidence, and truncated results are incomplete.",
		Kind: ToolKindRead,
		InputSchema: objectSchema(map[string]any{
			"entity":       propString("Entity name, canonical key, alias, or ID."),
			"entity_class": propString("Optional class: repository | service | api_endpoint | code_symbol | external_system | runbook."),
			"relations":    propStringArray("Optional predicates: contains, exposes, implemented_by, depends_on, documented_by."),
			"direction":    propString("outgoing (what this entity points to) | incoming (what points to this entity) | both. Default outgoing; use incoming to find upstream consumers of a service."),
			"max_depth":    propInt("Traversal depth 1-5 (default 2)."),
			"max_nodes":    propInt("Distinct node budget 1-500 (default 50)."),
			"max_fanout":   propInt("Per-node fact budget 1-100 (default 20)."),
		}, []string{"entity"}),
		Handler: stringHandler(func(ctx context.Context, args tool.Arguments) (string, error) {
			result, err := svc.TraceRelationsResult(ctx, ontology.RelationQuery{
				Entity: args.String("entity"), EntityClass: ontology.Class(args.String("entity_class")),
				Predicates: relationPredicates(args.Strings("relations")),
				Direction:  ontology.Direction(args.StringDefault("direction", "outgoing")),
				MaxDepth:   args.Int("max_depth", 2), MaxNodes: args.Int("max_nodes", 50),
				MaxFanout: args.Int("max_fanout", 20),
			})
			if err != nil {
				return "", err
			}
			return marshalResult(result)
		}),
	}
}

func relationPredicates(values []string) []ontology.Predicate {
	predicates := make([]ontology.Predicate, 0, len(values))
	for _, value := range values {
		predicates = append(predicates, ontology.Predicate(value))
	}
	return predicates
}

func webSearchToolResult(response WebSearchResponse) (tool.Result, error) {
	content, err := marshalResult(response)
	if err != nil {
		return tool.Result{}, err
	}
	units := webEvidenceUnits(response)
	coverage := tool.EvidenceCoverage{Partial: true}
	if len(units) > 0 {
		coverage = units[0].Coverage
	}
	return tool.Result{Content: content, EvidenceUnits: units, Coverage: coverage}, nil
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

// codeRefs derives canonical code references from search_result code hits.
// Each reference targets the normalized repos/... path so the evaluator can
// match it against a declaration verbatim.
func codeRefs(hits []knowledge.CodeSearchHit) []tool.Reference {
	refs := make([]tool.Reference, 0, len(hits))
	for _, hit := range hits {
		if hit.Path == "" {
			continue
		}
		label := hit.Path
		if hit.StartLine > 0 {
			label = fmt.Sprintf("%s:L%d", hit.Path, hit.StartLine)
		}
		refs = append(refs, tool.Reference{Type: tool.ReferenceCode, Label: label, Target: hit.Path})
	}
	return refs
}

// runbookRefs derives canonical runbook references from search_runbooks hits.
// Each reference targets the document id (docID) which is the authoritative
// identity the evaluator matches against.
func runbookRefs(hits []knowledge.RunbookSearchHit) []tool.Reference {
	refs := make([]tool.Reference, 0, len(hits))
	for _, hit := range hits {
		if hit.DocID == "" {
			continue
		}
		refs = append(refs, tool.Reference{Type: tool.ReferenceRunbook, Label: hit.Title, Target: hit.DocID})
	}
	return refs
}

func runbookAdmissionSpec() *tool.AdmissionSpec {
	const (
		baseTokens    = 256
		tokensPerItem = 1800
	)
	return &tool.AdmissionSpec{
		ResolveScope: func(args tool.Arguments) (tool.EvidenceScope, error) {
			if docID := args.String("doc_id"); docID != "" {
				return tool.EvidenceScope{SourceKind: "runbook", Target: docID}, nil
			}
			query := args.String("query")
			if query == "" {
				return tool.EvidenceScope{}, fmt.Errorf("query is required")
			}
			return tool.EvidenceScope{
				SourceKind: "runbook_search",
				Target:     platform.UUIDFromString("runbook_query\x00" + query),
			}, nil
		},
		MaxResultTokens: func(args tool.Arguments) int {
			return baseTokens + args.BoundedInt("limit", 3, 1, 10)*tokensPerItem
		},
		Narrow: func(args tool.Arguments, availableTokens int) (tool.Arguments, bool) {
			limit := args.BoundedInt("limit", 3, 1, 10)
			affordable := (availableTokens - baseTokens) / tokensPerItem
			if affordable < 1 || affordable >= limit {
				return args, false
			}
			narrowed := make(tool.Arguments, len(args)+1)
			for key, value := range args {
				narrowed[key] = value
			}
			narrowed["limit"] = affordable
			return narrowed, true
		},
	}
}

func runbookEvidenceUnits(result knowledge.RunbookSearchResult) []tool.EvidenceUnit {
	var units []tool.EvidenceUnit
	for _, hit := range result.Matches {
		for _, chunk := range hit.Chunks {
			unit, ok := canonicalevidence.RunbookChunkUnit(
				hit.DocID, chunk.ChunkIndex, chunk.ChunkText, hit.DocKind,
				hit.EvidenceClass, hit.TrustTier,
				tool.EvidenceCoverage{Partial: true, Included: 1, OmittedItems: boolInt(result.Truncated)},
			)
			if ok {
				units = append(units, unit)
			}
		}
	}
	return units
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
