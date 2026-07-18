package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/dekwanlabs/astris/platform/httputil"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	types "github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/internal/platform/store/codegraph"
	"github.com/dekwanlabs/astris/log"
)

type enrichedNode struct {
	codegraph.Node
	Service        string         `json:"service"`
	TargetService  string         `json:"targetService,omitempty"`
	DownstreamFile string         `json:"downstreamFile,omitempty"`
	DownstreamLine int            `json:"downstreamLine,omitempty"`
	Children       []enrichedNode `json:"children,omitempty"`
}

type endpointChain struct {
	Method  codegraph.Node `json:"method"`
	Service string         `json:"service"`
	Callers []enrichedNode `json:"callers"`
	Callees []enrichedNode `json:"callees"`
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
		"semanticEnabled": handler.semantic.Enabled(),
	}
	if handler.semantic.Enabled() {
		if stats := handler.qdrantCollectionStats(); stats != nil {
			result["vectorCount"] = stats["vectors_count"]
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
	n, err := handler.docDB.CountByKind(types.DocKindModule)
	if err != nil {
		return 0
	}
	return n
}

// codeChunkCount returns the number of embedded code chunks (payload
// kind=code_chunk) in Qdrant — the status source for the "Embed Code" op.
// 0 when semantic search is disabled or the count fails.
func (handler *Handler) codeChunkCount(ctx context.Context) int {
	if handler.semantic == nil || !handler.semantic.Enabled() {
		return 0
	}
	n, err := handler.semantic.CountByFilter(ctx, map[string]string{"kind": "code_chunk"})
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

func (handler *Handler) APIQdrantStats(w http.ResponseWriter, r *http.Request) {
	if !handler.semantic.Enabled() {
		httputil.WriteJSON(w, map[string]any{"enabled": false})
		return
	}
	stats := handler.qdrantCollectionStats()
	if stats == nil {
		httputil.WriteJSON(w, map[string]any{"enabled": true, "error": "failed to fetch stats"})
		return
	}
	stats["enabled"] = true
	httputil.WriteJSON(w, stats)
}

func (handler *Handler) qdrantCollectionStats() map[string]any {
	port := handler.cfg.QdrantPort
	if port == 6334 {
		port = 6333
	}
	url := fmt.Sprintf("http://%s:%d/collections/%s",
		handler.cfg.QdrantHost,
		port,
		handler.cfg.QdrantCollection)
	resp, err := http.Get(url)
	if err != nil {
		log.Errorf("[dashboard] qdrant stats error: %v", err)
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var data struct {
		Result map[string]any `json:"result"`
	}
	if json.Unmarshal(body, &data) != nil {
		return nil
	}
	return data.Result
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
	limit := q.Int("limit", 5)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	httputil.WriteJSON(w, handler.tools.GetSymbol(r.Context(), query, limit))
}

func (handler *Handler) TraceCalls(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r)
	query := q.Str("query")
	direction := q.StrDefault("direction", "callers")
	limit := q.Int("limit", 5)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	httputil.WriteJSON(w, handler.tools.TraceCalls(r.Context(), query, direction, limit))
}

func (handler *Handler) APICodeGraphEndpoint(w http.ResponseWriter, r *http.Request) {
	if handler.codegraphDB == nil {
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
	log.Infof("[codegraph] endpoint chain: %s:%d", file, line)
	chain, err := handler.codegraphDB.GetCallChainByFile(file, line, 30)
	if err != nil {
		log.Errorf("[codegraph] endpoint chain error: %v", err)
		httputil.WriteErr(w, err)
		return
	}

	edges, _ := handler.db.Edges(r.Context())
	resolveTarget := func(filePath string) string {
		if filePath == "" {
			return ""
		}
		for _, e := range edges {
			for _, ev := range e.Evidence {
				if ev.Path != "" && strings.EqualFold(ev.Path, filePath) {
					return e.To
				}
			}
		}
		return ""
	}

	ownService := serviceFromPath(chain.Node.FilePath)
	const minCrossSvcConfidence = 0.8
	dropped := 0
	enrich := func(nodes []codegraph.Node) []enrichedNode {
		out := []enrichedNode{}
		for _, n := range nodes {
			svc := serviceFromPath(n.FilePath)
			target := resolveTarget(n.FilePath)
			crossService := svc != ownService
			if crossService && target == "" {
				log.Infof("[codegraph] drop (cross-svc non-feign): %s (%s) conf=%.2f", n.Name, svc, n.Confidence)
				dropped++
				continue
			}
			if crossService && n.Confidence > 0 && n.Confidence < minCrossSvcConfidence {
				log.Infof("[codegraph] drop (low-confidence cross-svc): %s (%s) conf=%.2f", n.Name, svc, n.Confidence)
				dropped++
				continue
			}
			en := enrichedNode{Node: n, Service: svc}
			if target != "" {
				if httpMethod, path, ok := handler.codegraphDB.RouteAt(n.FilePath, n.StartLine); ok {
					en.TargetService = target
					if impl, err := handler.codegraphDB.ResolveDownstreamMethod(target, httpMethod, path); err == nil && impl != nil {
						en.DownstreamFile = impl.FilePath
						en.DownstreamLine = impl.StartLine
						log.Infof("[codegraph] feign %s %s -> %s %s:%d", httpMethod, path, target, impl.FilePath, impl.StartLine)
					} else {
						log.Infof("[codegraph] feign %s %s -> %s: impl not found", httpMethod, path, target)
					}
				} else {
					log.Warnf("[codegraph] skip non-route feign method: %s @ %s:%d", n.Name, n.FilePath, n.StartLine)
				}
			}
			out = append(out, en)
		}
		return out
	}

	callers := enrich(chain.Callers)
	callees := enrich(chain.Callees)

	depth := httputil.Query(r).Int("depth", 0)
	if depth <= 0 {
		depth = 3
	}
	if depth > 5 {
		depth = 5
	}
	if depth > 1 {
		for i := range callees {
			if callees[i].DownstreamFile != "" {
				expandDownstream(&callees[i], handler, depth-1, resolveTarget)
			}
		}
	}

	result := endpointChain{
		Method:  chain.Node,
		Service: ownService,
		Callers: callers,
		Callees: callees,
	}
	log.Infof("[codegraph] endpoint chain ok: method=%s callers=%d callees=%d dropped=%d", result.Method.Name, len(result.Callers), len(result.Callees), dropped)
	httputil.WriteJSON(w, result)
}

func expandDownstream(en *enrichedNode, h *Handler, depth int, resolveTarget func(string) string) {
	if depth <= 0 || en.DownstreamFile == "" || h.codegraphDB == nil {
		return
	}
	chain, err := h.codegraphDB.GetCallChainByFile(en.DownstreamFile, en.DownstreamLine, 20)
	if err != nil {
		return
	}
	own := serviceFromPath(chain.Node.FilePath)
	for _, n := range chain.Callees {
		svc := serviceFromPath(n.FilePath)
		target := resolveTarget(n.FilePath)
		if svc != own && target == "" {
			continue
		}
		if svc != own && n.Confidence > 0 && n.Confidence < 0.8 {
			continue
		}
		child := enrichedNode{Node: n, Service: svc}
		if target != "" {
			if m, p, ok := h.codegraphDB.RouteAt(n.FilePath, n.StartLine); ok {
				child.TargetService = target
				if impl, e := h.codegraphDB.ResolveDownstreamMethod(target, m, p); e == nil && impl != nil {
					child.DownstreamFile = impl.FilePath
					child.DownstreamLine = impl.StartLine
				}
			}
		}
		expandDownstream(&child, h, depth-1, resolveTarget)
		en.Children = append(en.Children, child)
	}
}

func serviceFromPath(filePath string) string {
	if filePath == "" {
		return ""
	}
	seg := filePath
	if i := strings.Index(seg, "/"); i >= 0 {
		seg = seg[:i]
	}
	parts := strings.Split(seg, "__")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return seg
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
