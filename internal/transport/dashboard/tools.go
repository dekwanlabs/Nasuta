package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/callchain"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform/httputil"
)

type enrichedNode struct {
	codegraph.Node
	Service        string         `json:"service"`
	TargetService  string         `json:"targetService,omitempty"`
	DownstreamFile string         `json:"downstreamFile,omitempty"`
	DownstreamLine int            `json:"downstreamLine,omitempty"`
	Children       []enrichedNode `json:"children,omitempty"`
	Depth          int            `json:"depth,omitempty"`
	CallLine       int            `json:"callLine,omitempty"`
	CallCol        int            `json:"callCol,omitempty"`
	Provenance     string         `json:"provenance,omitempty"`
	Bridge         bool           `json:"bridge,omitempty"`
}

type endpointChain struct {
	Method       codegraph.Node      `json:"method"`
	Service      string              `json:"service"`
	Callers      []enrichedNode      `json:"callers"`
	Callees      []enrichedNode      `json:"callees"`
	Truncated    map[string]bool     `json:"truncated"`
	NextFrontier map[string][]string `json:"nextFrontier,omitempty"`
	Unresolved   map[string][]string `json:"unresolved,omitempty"`
}

func (handler *Handler) APISummary(w http.ResponseWriter, r *http.Request) {
	sm, err := handler.db.Summary(r.Context())
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	result := map[string]any{
		"services":        sm.Services,
		"endpoints":       sm.Endpoints,
		"dependencies":    sm.Dependencies,
		"runbooks":        handler.runbookCount(),
		"moduleDocs":      handler.moduleDocsCount(),
		"codeChunks":      handler.codeChunkCount(r.Context()),
		"repos":           sm.Repos,
		"semanticEnabled": handler.semantic != nil && handler.semantic.Capabilities().Dense,
	}
	if handler.semantic != nil && handler.semantic.Capabilities().Count {
		if count, err := handler.semantic.Count(r.Context(), semantic.Filter{}); err == nil {
			result["vectorCount"] = count
		}
	}
	httputil.WriteJSON(w, result)
}

// moduleDocsCount returns the number of generated module docs (kind=module) in
// the DocStore — the status source for the "Regen Docs" op. 0 when no DocStore.
func (handler *Handler) moduleDocsCount() int {
	if handler.docDB == nil {
		return 0
	}
	n, err := handler.docDB.CountByKind(domain.DocKindModule)
	if err != nil {
		return 0
	}
	return n
}

// codeChunkCount returns the number of embedded code chunks.
// 0 when semantic search is disabled or the count fails.
func (handler *Handler) codeChunkCount(ctx context.Context) int {
	if handler.semantic == nil || !handler.semantic.Capabilities().Count {
		return 0
	}
	n, err := handler.semantic.Count(ctx, semantic.Filter{Keywords: map[string]string{"kind": "code_chunk"}})
	if err != nil {
		log.Errorf("[dashboard] code chunk count error: %v", err)
		return 0
	}
	return n
}

// runbookCount returns the number of runbooks in the platform DocStore, or 0
// when MySQL is unconfigured or the count fails. Runbooks no longer live in
// SQLite, so this is the sole source for the summary "runbooks" count.
func (handler *Handler) runbookCount() int {
	if handler.docDB == nil {
		return 0
	}
	n, err := handler.docDB.CountRunbooks()
	if err != nil {
		return 0
	}
	return n
}

func (handler *Handler) APIServices(w http.ResponseWriter, r *http.Request) {
	services, err := handler.db.AllServices(r.Context())
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	query := strings.ToLower(httputil.Query(r).Str("q"))
	if query != "" {
		var filtered []any
		for _, s := range services {
			if strings.Contains(strings.ToLower(s.ServiceName), query) ||
				strings.Contains(strings.ToLower(s.Layer), query) ||
				strings.Contains(strings.ToLower(s.Owner), query) ||
				strings.Contains(strings.ToLower(s.Summary), query) {
				filtered = append(filtered, s)
			}
		}
		httputil.WriteJSON(w, filtered)
		return
	}
	httputil.WriteJSON(w, services)
}

func (handler *Handler) APIEndpoints(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	service := q.Str("service")
	keyword := q.Str("keyword")
	page, pageSize := q.Page(20, 100000)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	result, err := handler.db.ListApis(r.Context(), service, keyword, page, pageSize)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, result)
}

func (handler *Handler) APIEdges(w http.ResponseWriter, r *http.Request) {
	edges, err := handler.db.Edges(r.Context())
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, edges)
}

func (handler *Handler) APISemanticStatus(w http.ResponseWriter, r *http.Request) {
	if handler.semantic == nil {
		httputil.WriteJSON(w, map[string]any{"enabled": false})
		return
	}
	count, err := handler.semantic.Count(r.Context(), semantic.Filter{})
	if err != nil {
		httputil.WriteJSON(w, map[string]any{
			"enabled": true, "provider": handler.cfg.Semantic.Provider,
			"collection": handler.cfg.Semantic.Collection, "capabilities": handler.semantic.Capabilities(),
			"error": err.Error(),
		})
		return
	}
	httputil.WriteJSON(w, map[string]any{
		"enabled": true, "provider": handler.cfg.Semantic.Provider,
		"collection": handler.cfg.Semantic.Collection, "capabilities": handler.semantic.Capabilities(),
		"vectorCount": count,
	})
}

func (handler *Handler) ServiceLookup(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	query := q.Str("query")
	limit := q.Int("limit", 10)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	httputil.WriteJSON(w, handler.tools.ServiceLookup(r.Context(), query, limit))
}

func (handler *Handler) CodeSearch(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	query := q.Str("query")
	lang := q.Str("lang")
	limit := q.Int("limit", 10)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	httputil.WriteJSON(w, handler.tools.CodeSearch(r.Context(), query, lang, limit))
}

func (handler *Handler) RunbookSearch(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	query := q.Str("query")
	limit := q.Int("limit", 10)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	httputil.WriteJSON(w, handler.tools.RunbookSearch(r.Context(), query, limit, false, ""))
}

func (handler *Handler) TraceDeps(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	service := q.Str("service")
	direction := q.StrDefault("direction", "both")
	depth := q.Int("depth", 2)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	if depth < 1 {
		depth = 1
	}
	if depth > 5 {
		depth = 5
	}
	httputil.WriteJSON(w, handler.tools.TraceDeps(service, direction, depth))
}

func (handler *Handler) ListApis(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	service := q.Str("service")
	keyword := q.Str("keyword")
	limit := q.Int("limit", 200)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	httputil.WriteJSON(w, handler.tools.ListApis(r.Context(), service, keyword, limit))
}

func (handler *Handler) DocGapCheck(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, handler.tools.DocGapCheck(r.Context(), httputil.Query(r).Str("service")))
}

func (handler *Handler) IndexSummary(w http.ResponseWriter, r *http.Request) {
	httputil.WriteJSON(w, handler.tools.IndexSummary(r.Context()))
}

func (handler *Handler) GetSymbol(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	query := q.Str("query")
	file := q.Str("file")
	qualifiedName := q.Str("qualified_name")
	limit := q.Int("limit", 5)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	httputil.WriteJSON(w, handler.tools.GetSymbolFiltered(r.Context(), query, file, qualifiedName, limit))
}

func (handler *Handler) TraceCalls(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	request := callchain.Request{
		Query: q.Str("query"), File: q.Str("file"), Line: q.Int("line", 0),
		QualifiedName: q.Str("qualified_name"), Direction: q.StrDefault("direction", "both"),
		MaxDepth: q.Int("max_depth", 3), MaxNodes: q.Int("max_nodes", 40), MaxFanout: q.Int("max_fanout", 20),
	}
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	httputil.WriteJSON(w, handler.tools.TraceCalls(r.Context(), request))
}

func (handler *Handler) APICodeGraphEndpoint(w http.ResponseWriter, r *http.Request) {
	if handler.callChain == nil || !handler.callChain.Available() {
		httputil.WriteServiceUnavailable(w, "codegraph not available")
		return
	}
	q := httputil.Query(r)
	file := q.Required("file")
	line := q.Int("line", 0)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	if line <= 0 {
		httputil.WriteBadRequest(w, "?line required")
		return
	}
	depth := q.Int("depth", 3)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	chainResult, err := handler.callChain.Trace(r.Context(), callchain.Request{
		File: file, Line: line, Direction: "both", MaxDepth: depth, MaxNodes: 80, MaxFanout: 30,
	})
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	if chainResult.Target == nil {
		httputil.WriteErr(w, fmt.Errorf("codegraph: no callable node at %s:%d", file, line))
		return
	}
	response := endpointChain{
		Method: chainResult.Target.Node, Service: chainResult.Target.ServiceName,
		Callers: dashboardNodes(chainResult.Callers, true), Callees: dashboardNodes(chainResult.Callees, false),
		Truncated:    map[string]bool{"callers": chainResult.Callers.Truncated, "callees": chainResult.Callees.Truncated},
		NextFrontier: map[string][]string{"callers": chainResult.Callers.NextFrontier, "callees": chainResult.Callees.NextFrontier},
		Unresolved:   map[string][]string{"callers": chainResult.Callers.Unresolved, "callees": chainResult.Callees.Unresolved},
	}
	httputil.WriteJSON(w, response)
}

func dashboardNodes(direction callchain.DirectionResult, callers bool) []enrichedNode {
	nodes := make([]enrichedNode, 0, len(direction.Hops))
	for _, hop := range direction.Hops {
		node := hop.Target
		if callers {
			node = hop.Source
		}
		enriched := enrichedNode{
			Node: node.Node, Service: node.ServiceName, Depth: hop.Depth,
			CallLine: hop.Edge.Line, CallCol: hop.Edge.Col, Provenance: hop.Edge.Provenance, Bridge: hop.Bridge,
		}
		if hop.Bridge && !callers {
			enriched.TargetService = hop.Target.ServiceName
			enriched.DownstreamFile = hop.Target.FilePath
			enriched.DownstreamLine = hop.Target.StartLine
		}
		nodes = append(nodes, enriched)
	}
	return nodes
}

func (handler *Handler) APICodeGraphSource(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	rel := q.Required("file")
	start := q.Int("start", 0)
	end := q.Int("end", 0)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}

	clean := filepath.Clean("/" + rel)
	full := filepath.Join(handler.cfg.WorkspaceRoot, clean)
	if !strings.HasPrefix(full, filepath.Clean(handler.cfg.WorkspaceRoot)+string(os.PathSeparator)) {
		httputil.WriteBadRequest(w, "invalid path")
		return
	}

	data, err := os.ReadFile(full)
	if err != nil {
		base := filepath.Base(rel)
		if handler.codegraphDB != nil {
			if files, ferr := handler.codegraphDB.FindFilesByName(base, 5); ferr == nil && len(files) > 0 {
				full = filepath.Join(handler.cfg.WorkspaceRoot, files[0])
				data, err = os.ReadFile(full)
			}
		}
	}
	if err != nil {
		httputil.WriteErr(w, fmt.Errorf("read source: %w", err))
		return
	}
	lines := strings.Split(string(data), "\n")
	n := len(lines)

	if start < 1 {
		start = 1
	}
	if end < start || end > n {
		end = n
	}
	ctxStart := start
	if ctxStart > 3 {
		ctxStart = start - 3
	} else {
		ctxStart = 1
	}
	snippet := strings.Join(lines[ctxStart-1:end], "\n")

	lang := strings.TrimPrefix(filepath.Ext(rel), ".")
	httputil.WriteJSON(w, map[string]any{
		"file":      rel,
		"language":  lang,
		"startLine": ctxStart,
		"endLine":   end,
		"code":      snippet,
	})
}
