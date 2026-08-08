package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
)

// GetSymbolFiltered applies explicit file and qualified-name disambiguation.
func (srv *Service) GetSymbolFiltered(ctx context.Context, query, file, qualifiedName string, limit int) map[string]any {
	result, err := srv.GetSymbolResult(ctx, query, file, qualifiedName, limit)
	if err != nil {
		return map[string]any{"matches": nil, "error": err.Error()}
	}
	return result
}

// GetSymbolResult queries codegraph without hiding availability or query failures.
func (srv *Service) GetSymbolResult(ctx context.Context, query, file, qualifiedName string, limit int) (map[string]any, error) {
	query = strings.TrimSpace(query)
	qualifiedName = strings.TrimSpace(qualifiedName)
	if query == "" {
		query = qualifiedName
	}
	if query == "" {
		return nil, fmt.Errorf("get symbol: query or qualified_name is required")
	}
	root := srv.workspaceRoot
	if root == "" {
		return nil, fmt.Errorf("codegraph: no workspace root configured")
	}
	if limit <= 0 {
		limit = 5
	}
	if limit < 2 {
		limit = 2
	}
	if limit > 10 {
		limit = 10
	}

	db, err := codegraph.Open(root)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, fmt.Errorf("codegraph not indexed")
	}
	defer db.Close()

	nodes, err := db.SearchSymbols(ctx, codegraph.SymbolQuery{
		Terms:        symbolQueryTokens(query),
		PathPrefixes: nonEmptyStrings(file),
		Limit:        40,
	})
	if err != nil {
		return nil, err
	}
	nodes = exactSymbolCandidates(nodes, query, qualifiedName)
	if len(nodes) == 0 {
		return map[string]any{"resolution": "not_found", "matches": []any{}}, nil
	}
	if len(nodes) > 1 {
		candidateLimit := min(limit, len(nodes))
		candidates := make([]any, 0, candidateLimit)
		for _, n := range nodes[:candidateLimit] {
			candidates = append(candidates, map[string]any{
				"kind": n.Kind, "qualifiedName": n.QualifiedName,
				"file": n.FilePath, "line": n.StartLine,
			})
		}
		return map[string]any{
			"resolution": "ambiguous",
			"candidates": candidates,
		}, nil
	}

	n := nodes[0]
	source, err := readNodeSource(root, n)
	if err != nil {
		return nil, err
	}
	match := map[string]any{
		"id": n.ID, "function": n.Name, "qualifiedName": n.QualifiedName,
		"kind": n.Kind, "file": n.FilePath, "line": n.StartLine, "source": source,
	}
	return map[string]any{"resolution": "unique", "matches": []any{match}}, nil
}

func exactSymbolCandidates(nodes []codegraph.Node, query, qualifiedName string) []codegraph.Node {
	out := make([]codegraph.Node, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		switch n.Kind {
		case "field", "import", "namespace", "file", "constant":
			continue
		}
		if qualifiedName != "" {
			if !strings.EqualFold(n.QualifiedName, qualifiedName) {
				continue
			}
		} else if !strings.EqualFold(n.Name, query) && !strings.EqualFold(n.QualifiedName, query) {
			continue
		}
		key := strings.ToLower(n.FilePath) + "\x00" +
			strings.ToLower(n.QualifiedName) + "\x00" + strings.ToLower(n.Kind)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, n)
	}
	return out
}

func nonEmptyStrings(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

// TraceCalls resolves a symbol and walks its callers or callees.
func (srv *Service) TraceCalls(ctx context.Context, request callchain.Request) map[string]any {
	result, err := srv.TraceCallsResult(ctx, request)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return result
}

// TraceCallsResult walks codegraph call edges without hiding broken prerequisites.
func (srv *Service) TraceCallsResult(ctx context.Context, request callchain.Request) (map[string]any, error) {
	if srv.callChain == nil || !srv.callChain.Available() {
		return nil, fmt.Errorf("call chain unavailable: codegraph or structure index is not ready")
	}
	requested := request
	resolved, err := srv.resolveAPICallTarget(ctx, request)
	if err != nil {
		return nil, err
	}
	result, err := srv.callChain.Trace(ctx, resolved)
	if err != nil {
		return nil, err
	}
	return callChainResult(srv.workspaceRoot, requested, resolved, result)
}

func (srv *Service) resolveAPICallTarget(ctx context.Context, request callchain.Request) (callchain.Request, error) {
	if srv.ontology == nil || request.Query == "" || request.File != "" || request.Line > 0 || request.QualifiedName != "" {
		return request, nil
	}
	result, err := srv.ontology.QueryRelations(ctx, ontology.RelationQuery{
		Entity: request.Query, EntityClass: ontology.ClassAPIEndpoint,
		Predicates: []ontology.Predicate{ontology.PredicateImplementedBy}, Direction: ontology.DirectionOutgoing,
		MaxDepth: 1, MaxNodes: 20, MaxFanout: 20,
	})
	if err != nil {
		return request, fmt.Errorf("resolve API call target: %w", err)
	}
	if result.Root == nil || len(result.Facts) != 1 {
		return request, nil
	}
	fact := result.Facts[0]
	request.Query = fact.Object.Name
	request.QualifiedName = fact.Object.Name
	if len(fact.Evidence) > 0 {
		request.File = fact.Evidence[0].Path
		request.Line = fact.Evidence[0].Line
	}
	return request, nil
}

func callChainResult(root string, requested, resolved callchain.Request, result callchain.Result) (map[string]any, error) {
	decorate := func(direction callchain.DirectionResult, callers bool) (map[string]any, error) {
		nodes := make([]map[string]any, 0, len(direction.Hops))
		for _, hop := range direction.Hops {
			node := hop.Target
			if callers {
				node = hop.Source
			}
			source, err := readNodeSource(root, node.Node)
			if err != nil {
				return nil, err
			}
			nodes = append(nodes, map[string]any{
				"id": node.ID, "function": node.Name, "qualifiedName": node.QualifiedName,
				"kind": node.Kind, "file": node.FilePath, "line": node.StartLine,
				"service": node.ServiceName, "depth": hop.Depth, "source": source,
				"callSite":   map[string]any{"line": hop.Edge.Line, "col": hop.Edge.Col},
				"confidence": hop.Edge.Confidence, "provenance": hop.Edge.Provenance, "bridge": hop.Bridge,
			})
		}
		return map[string]any{
			"nodes": nodes, "truncated": direction.Truncated,
			"nextFrontier": direction.NextFrontier, "unresolved": direction.Unresolved,
		}, nil
	}
	callers, err := decorate(result.Callers, true)
	if err != nil {
		return nil, err
	}
	callees, err := decorate(result.Callees, false)
	if err != nil {
		return nil, err
	}
	response := map[string]any{
		"direction": result.Direction, "maxDepth": result.MaxDepth,
		"maxNodes": result.MaxNodes, "maxFanout": result.MaxFanout,
		"requestedTarget": requested, "resolvedTarget": resolved,
		"callers": callers, "callees": callees,
	}
	if result.Target != nil {
		response["target"] = result.Target
	}
	if len(result.Candidates) > 0 {
		response["candidates"] = result.Candidates
		response["ambiguous"] = true
	}
	return response, nil
}

func symbolQueryTokens(query string) []string {
	fields := strings.FieldsFunc(query, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '.', '#', '/', '\\', ':':
			return true
		default:
			return false
		}
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if utf8.RuneCountInString(f) >= 3 {
			out = append(out, f)
		}
	}
	return out
}

// readNodeSource loads the indexed range and reports index/filesystem drift.
func readNodeSource(root string, n codegraph.Node) (string, error) {
	if n.FilePath == "" || n.StartLine <= 0 {
		return "", fmt.Errorf("read source: node %q has no file location", n.QualifiedName)
	}
	absPath := filepath.Join(root, n.FilePath)
	if !strings.HasPrefix(n.FilePath, "repos/") {
		absPath = filepath.Join(root, "repos", n.FilePath)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("read source %q: %w", n.FilePath, err)
	}
	lines := strings.Split(string(data), "\n")
	if n.EndLine > len(lines) {
		n.EndLine = len(lines)
	}
	if n.EndLine <= n.StartLine {
		n.EndLine = n.StartLine + 40
	}
	start := n.StartLine - 3
	if start < 0 {
		start = 0
	}
	end := n.EndLine
	if end > len(lines) {
		end = len(lines)
	}
	body := strings.Join(lines[start:end], "\n")
	runes := []rune(body)
	if len(runes) > 4000 {
		body = string(runes[:4000]) + "\n...(truncated)"
	}
	return body, nil
}
