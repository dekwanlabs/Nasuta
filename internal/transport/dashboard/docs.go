package dashboard

import (
	"context"
	"fmt"
	"github.com/dekwanlabs/nasuta/platform/httputil"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/indexing/docgen"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

type docUploadReq struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Kind    string `json:"kind"`
	URL     string `json:"url"`
}

type docBatchReindexReq struct {
	IDs []string `json:"ids"`
}

type knowledgeCreateReq struct {
	Title   string `json:"title"`
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

type docChunkPreview struct {
	ChunkIndex    int    `json:"chunk_index"`
	SectionHeader string `json:"section_header"`
	Text          string `json:"text"`
}

type docSearchHitPreview struct {
	DocID         string  `json:"doc_id"`
	Title         string  `json:"title"`
	SectionHeader string  `json:"section_header"`
	ChunkIndex    int     `json:"chunk_index"`
	Score         float32 `json:"score"`
	Text          string  `json:"text"`
}

type docDraft struct {
	Title    string
	Filename string
	Kind     string
	Content  string
}

var allowedTextDocExt = map[string]struct{}{
	".md": {}, ".markdown": {},
}

var allowedUploadDocKinds = domain.UploadableDocKindSet

func (handler *Handler) APIDocs(w http.ResponseWriter, r *http.Request) {
	if handler.docDB == nil {
		httputil.WriteJSON(w, &domain.Page[domain.DocRecord]{Total: 0, Page: 1, PageSize: 20, List: []domain.DocRecord{}})
		return
	}
	q := httputil.Query(r)
	page, pageSize := q.Page(20, 200)
	if q.Err() != nil {
		httputil.WriteBadRequest(w, q.Err().Error())
		return
	}
	docs, err := handler.docDB.ListDocsMetaPageFiltered(
		page,
		pageSize,
		q.Str("kind"),
		q.Str("q"),
		q.Str("sort_by"),
		q.Str("sort_order"),
	)
	if err != nil {
		httputil.WriteErr(w, fmt.Errorf("list documents: %w", err))
		return
	}
	if docs == nil {
		docs = &domain.Page[domain.DocRecord]{Total: 0, Page: page, PageSize: pageSize, List: []domain.DocRecord{}}
	}
	httputil.WriteJSON(w, docs)
}

func (handler *Handler) APIDocUpload(w http.ResponseWriter, r *http.Request) {
	draft, err := readDocUpload(r)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	draft.Kind = normalizeUploadDocKind(draft.Kind)

	// Structural validation + LLM reformat for flow docs. The template is
	// the reference shape; when content doesn't conform, the LLM rewrites it
	// into the template structure before storage.
	if draft.Kind == domain.DocKindFlow {
		res := docgen.ValidateFlow(draft.Content)
		if !res.Valid {
			log.Warnf("[docs] flow %q failed validation: %s — reformatting", draft.Title, strings.Join(res.Errors, "; "))
			reformatted, rerr := docgen.ReformatFlowWithSettings(handler.cfg, handler.platform, handler.docDB, r.Context(), draft.Content)
			if rerr != nil {
				httputil.WriteErr(w, fmt.Errorf("flow validation failed (%s) and reformat unavailable: %w", strings.Join(res.Errors, "; "), rerr))
				return
			}
			draft.Content = reformatted
		}
	}

	doc, chunks, err := buildDocRecord(draft, allowedUploadDocKinds)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	doc, err = handler.saveDocRecord(r.Context(), "docs", doc, chunks)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, doc)
}

// APIDocTemplate returns the canonical template for a document kind. Powers the
// "view template" preview in the upload dialog.
func (handler *Handler) APIDocTemplate(w http.ResponseWriter, r *http.Request) {
	kind := httputil.Query(r).Str("kind")
	var content, name string
	switch kind {
	case domain.DocKindFlow:
		content = docgen.FlowTemplateEnglish
		name = "Flow Template"
	default:
		httputil.WriteErr(w, fmt.Errorf("no template for kind %q", kind))
		return
	}
	httputil.WriteJSON(w, map[string]any{"kind": kind, "name": name, "content": content})
}

func (handler *Handler) APIDocGet(w http.ResponseWriter, r *http.Request) {
	doc, err := handler.loadDoc(r.PathValue("id"), "document")
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, doc)
}

func (handler *Handler) APIDocChunks(w http.ResponseWriter, r *http.Request) {
	doc, err := handler.loadDoc(r.PathValue("id"), "document")
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	chunks := chunkDocument(doc)
	previews := make([]docChunkPreview, 0, len(chunks))
	for _, c := range chunks {
		previews = append(previews, docChunkPreview{
			ChunkIndex:    c.ChunkIndex,
			SectionHeader: c.SectionHeader,
			Text:          c.Text,
		})
	}
	httputil.WriteJSON(w, map[string]any{
		"doc_id": doc.ID,
		"title":  doc.Title,
		"total":  len(previews),
		"list":   previews,
	})
}

func (handler *Handler) APIDocDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httputil.WriteErr(w, fmt.Errorf("missing document id"))
		return
	}
	if handler.semantic != nil {
		if err := handler.deleteDocVectors(r.Context(), id); err != nil {
			log.Errorf("[docs] qdrant delete error for %q: %v", id, err)
		}
	}
	if handler.docDB != nil {
		if _, err := handler.docDB.DeleteDoc(id); err != nil {
			httputil.WriteErr(w, fmt.Errorf("delete document: %w", err))
			return
		}
	}
	httputil.WriteJSON(w, map[string]string{"deleted": id})
}

func (handler *Handler) APIDocReindex(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httputil.WriteErr(w, fmt.Errorf("missing document id"))
		return
	}
	doc, err := handler.reindexDoc(r.Context(), id)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, doc)
}

func (handler *Handler) APIDocsBatchReindex(w http.ResponseWriter, r *http.Request) {
	if handler.docDB == nil {
		httputil.WriteErr(w, fmt.Errorf("database not available"))
		return
	}
	var req docBatchReindexReq
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	if len(req.IDs) == 0 {
		httputil.WriteErr(w, fmt.Errorf("ids is empty"))
		return
	}
	type item struct {
		ID    string `json:"id"`
		Title string `json:"title,omitempty"`
		Error string `json:"error,omitempty"`
	}
	resp := struct {
		Total   int    `json:"total"`
		Success int    `json:"success"`
		Failed  int    `json:"failed"`
		Items   []item `json:"items"`
	}{Total: len(req.IDs), Items: make([]item, 0, len(req.IDs))}
	for _, id := range req.IDs {
		doc, err := handler.reindexDoc(r.Context(), id)
		if err != nil {
			resp.Failed++
			resp.Items = append(resp.Items, item{ID: id, Error: err.Error()})
			continue
		}
		resp.Success++
		resp.Items = append(resp.Items, item{ID: doc.ID, Title: doc.Title})
	}
	httputil.WriteJSON(w, resp)
}

func (handler *Handler) DocSearchTest(w http.ResponseWriter, r *http.Request) {
	q := httputil.Query(r).Str("q")
	if q == "" {
		httputil.WriteErr(w, fmt.Errorf("?q required"))
		return
	}
	if handler.semantic == nil || handler.embedder == nil {
		httputil.WriteJSON(w, map[string]any{"hits": []any{}, "error": "semantic search unavailable"})
		return
	}
	vecs, err := handler.embedder.Embed(r.Context(), []string{q})
	if err != nil || len(vecs) == 0 {
		httputil.WriteErr(w, fmt.Errorf("embed: %w", err))
		return
	}
	hits, err := handler.semantic.Search(r.Context(), semantic.Query{
		DenseVector: vecs[0], Filter: semantic.Filter{Keywords: map[string]string{"kind": "runbook"}}, Limit: 5,
	})
	if err != nil {
		httputil.WriteErr(w, fmt.Errorf("search: %w", err))
		return
	}
	previews := make([]docSearchHitPreview, 0, len(hits))
	for _, h := range hits {
		p := docSearchHitPreview{Score: h.Score}
		if v, _ := h.Metadata["doc_id"].(string); v != "" {
			p.DocID = v
		}
		if p.DocID == "" {
			if v, _ := h.Metadata["id"].(string); v != "" {
				p.DocID = v
			}
		}
		if v, _ := h.Metadata["title"].(string); v != "" {
			p.Title = v
		}
		if v, _ := h.Metadata["section_header"].(string); v != "" {
			p.SectionHeader = v
		}
		switch v := h.Metadata["chunk_index"].(type) {
		case float64:
			p.ChunkIndex = int(v)
		case int:
			p.ChunkIndex = v
		}
		if v, _ := h.Metadata["text"].(string); v != "" {
			p.Text = v
		}
		if p.Text == "" {
			path, _ := h.Metadata["path"].(string)
			handler.fillSearchHitPreview(&p, path)
		}
		previews = append(previews, p)
	}
	httputil.WriteJSON(w, map[string]any{"hits": previews})
}

func (handler *Handler) APIKnowledgeList(w http.ResponseWriter, r *http.Request) {
	if handler.docDB == nil {
		httputil.WriteJSON(w, []domain.DocRecord{})
		return
	}
	kind := httputil.Query(r).Str("kind")
	var docs []domain.DocRecord
	var err error
	if kind != "" {
		docs, err = handler.docDB.ListDocsMetaByKind(kind)
	} else {
		docs, err = handler.docDB.ListDocsMetaByKinds(domain.KnowledgeDocKinds)
	}
	if err != nil {
		httputil.WriteErr(w, fmt.Errorf("list knowledge: %w", err))
		return
	}
	if docs == nil {
		docs = []domain.DocRecord{}
	}
	httputil.WriteJSON(w, docs)
}

func (handler *Handler) APIKnowledgeGet(w http.ResponseWriter, r *http.Request) {
	doc, err := handler.loadDoc(r.PathValue("id"), "knowledge")
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, doc)
}

func (handler *Handler) APIKnowledgeCreate(w http.ResponseWriter, r *http.Request) {
	var req knowledgeCreateReq
	if err := httputil.DecodeJSON(r, &req); err != nil {
		httputil.WriteBadRequest(w, err.Error())
		return
	}
	doc, chunks, err := buildDocRecord(docDraft{
		Title:    req.Title,
		Filename: req.Title + ".md",
		Kind:     req.Kind,
		Content:  req.Content,
	}, domain.KnowledgeDocKindSet)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	doc, err = handler.saveDocRecord(r.Context(), "knowledge", doc, chunks)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, doc)
}

func (handler *Handler) APIKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		httputil.WriteErr(w, fmt.Errorf("missing knowledge id"))
		return
	}
	if err := handler.deleteDocAndVectors(r.Context(), id); err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]string{"deleted": id})
}

func (handler *Handler) APIKnowledgeReindex(w http.ResponseWriter, r *http.Request) {
	doc, err := handler.loadDoc(r.PathValue("id"), "knowledge")
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	doc, err = handler.reindexStoredDoc(r.Context(), "knowledge", doc)
	if err != nil {
		httputil.WriteErr(w, err)
		return
	}
	httputil.WriteJSON(w, doc)
}

func (handler *Handler) reindexDoc(ctx context.Context, id string) (domain.DocRecord, error) {
	doc, err := handler.loadDoc(id, "document")
	if err != nil {
		return domain.DocRecord{}, err
	}
	return handler.reindexStoredDoc(ctx, "docs", doc)
}

func chunkDocument(doc domain.DocRecord) []indexer.DocChunk {
	return indexer.ChunkMarkdown(doc.ID, doc.Title, stripDocHashLine(doc.Content), indexer.DefaultDocChunkConfig())
}

func (handler *Handler) fillSearchHitPreview(hit *docSearchHitPreview, path string) {
	var (
		title  string
		chunks []indexer.DocChunk
	)

	if handler.docDB != nil && hit.DocID != "" {
		if doc, err := handler.docDB.GetDoc(hit.DocID); err == nil {
			title = doc.Title
			chunks = chunkDocument(doc)
		}
	}
	if len(chunks) == 0 && path != "" {
		abs := filepath.Join(handler.cfg.WorkspaceRoot, path)
		if data, err := os.ReadFile(abs); err == nil {
			title = platform.FirstNonEmpty(hit.Title, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
			chunks = indexer.ChunkMarkdown(hit.DocID, title, string(data), indexer.DefaultDocChunkConfig())
		}
	}
	if hit.ChunkIndex < 0 || hit.ChunkIndex >= len(chunks) {
		return
	}
	chunk := chunks[hit.ChunkIndex]
	if hit.Title == "" {
		hit.Title = platform.FirstNonEmpty(chunk.Title, title)
	}
	if hit.SectionHeader == "" {
		hit.SectionHeader = chunk.SectionHeader
	}
	if hit.Text == "" {
		hit.Text = chunk.Text
	}
}

func stripDocHashLine(s string) string {
	const pre = "<!-- hash:"
	if !strings.HasPrefix(s, pre) {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return ""
}

func normalizeUploadDocKind(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" || kind == "all" {
		return domain.DocKindDocument
	}
	return kind
}

func readDocUpload(r *http.Request) (docDraft, error) {
	contentType := r.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			return docDraft{}, fmt.Errorf("parse multipart: %w", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			return docDraft{}, fmt.Errorf("read file: %w", err)
		}
		defer file.Close()
		body, err := io.ReadAll(file)
		if err != nil {
			return docDraft{}, fmt.Errorf("read file body: %w", err)
		}
		title := r.FormValue("title")
		filename := header.Filename
		if ext := strings.ToLower(filepath.Ext(filename)); ext != "" {
			if _, ok := allowedTextDocExt[ext]; !ok {
				return docDraft{}, fmt.Errorf("unsupported file type %q", ext)
			}
		}
		if title == "" {
			title = strings.TrimSuffix(filename, filepath.Ext(filename))
		}
		return docDraft{
			Title:    title,
			Filename: filename,
			Kind:     r.FormValue("kind"),
			Content:  string(body),
		}, nil
	}

	var req docUploadReq
	if err := httputil.DecodeJSON(r, &req); err != nil {
		return docDraft{}, err
	}
	// URL import: fetch the page and convert to markdown before validation.
	if strings.TrimSpace(req.URL) != "" {
		content, _, err := fetchURLContent(r.Context(), req.URL)
		if err != nil {
			return docDraft{}, fmt.Errorf("import url: %w", err)
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			title = deriveTitleFromURL(req.URL)
		}
		return docDraft{
			Title:    title,
			Filename: title + ".md",
			Kind:     req.Kind,
			Content:  content,
		}, nil
	}
	return docDraft{
		Title:    req.Title,
		Filename: req.Title + ".md",
		Kind:     req.Kind,
		Content:  req.Content,
	}, nil
}

// deriveTitleFromURL picks the last path segment of a URL as a fallback title
// when the user didn't supply one.
func deriveTitleFromURL(rawURL string) string {
	// strip query/fragment
	if i := strings.IndexAny(rawURL, "?#"); i >= 0 {
		rawURL = rawURL[:i]
	}
	rawURL = strings.TrimRight(rawURL, "/")
	if i := strings.LastIndex(rawURL, "/"); i >= 0 {
		seg := rawURL[i+1:]
		if seg != "" {
			return seg
		}
	}
	return "imported-doc"
}

func buildDocRecord(draft docDraft, allowedKinds map[string]struct{}) (domain.DocRecord, []indexer.DocChunk, error) {
	if draft.Title == "" {
		return domain.DocRecord{}, nil, fmt.Errorf("title is required")
	}
	if strings.TrimSpace(draft.Content) == "" {
		return domain.DocRecord{}, nil, fmt.Errorf("content is empty")
	}
	if _, ok := allowedKinds[draft.Kind]; !ok {
		return domain.DocRecord{}, nil, fmt.Errorf("unsupported kind %q", draft.Kind)
	}

	docID := indexer.DocID(draft.Title, draft.Filename)
	now := time.Now().UTC().Format(time.RFC3339)
	chunks := indexer.ChunkMarkdown(docID, draft.Title, draft.Content, indexer.DefaultDocChunkConfig())
	if len(chunks) == 0 {
		return domain.DocRecord{}, nil, fmt.Errorf("document produced no chunks")
	}

	return domain.DocRecord{
		ID:         docID,
		Title:      draft.Title,
		Filename:   draft.Filename,
		Kind:       draft.Kind,
		Content:    draft.Content,
		ChunkCount: len(chunks),
		CreatedAt:  now,
		UpdatedAt:  now,
	}, chunks, nil
}

func (handler *Handler) saveDocRecord(ctx context.Context, scope string, doc domain.DocRecord, chunks []indexer.DocChunk) (domain.DocRecord, error) {
	log.Infof("[%s] save %q (%s): %d chunks", scope, doc.Title, doc.ID, len(chunks))
	if handler.semantic != nil && handler.embedder != nil {
		if _, err := handler.embedDocChunks(ctx, doc, chunks); err != nil {
			log.Errorf("[%s] embed error for %q: %v", scope, doc.Title, err)
			return domain.DocRecord{}, fmt.Errorf("embed: %w", err)
		}
	}
	if handler.docDB != nil {
		if err := handler.docDB.InsertDoc(doc); err != nil {
			log.Errorf("[%s] db insert error for %q: %v", scope, doc.Title, err)
			return domain.DocRecord{}, fmt.Errorf("save %s: %w", singularScope(scope), err)
		}
	}
	doc.Content = ""
	return doc, nil
}

func (handler *Handler) loadDoc(id, label string) (domain.DocRecord, error) {
	if id == "" {
		return domain.DocRecord{}, fmt.Errorf("missing %s id", label)
	}
	if handler.docDB == nil {
		return domain.DocRecord{}, fmt.Errorf("database not available")
	}
	doc, err := handler.docDB.GetDoc(id)
	if err != nil {
		return domain.DocRecord{}, fmt.Errorf("%s not found: %w", label, err)
	}
	return doc, nil
}

func (handler *Handler) reindexStoredDoc(ctx context.Context, scope string, doc domain.DocRecord) (domain.DocRecord, error) {
	chunks := chunkDocument(doc)
	if len(chunks) == 0 {
		return domain.DocRecord{}, fmt.Errorf("document produced no chunks")
	}
	if handler.semantic != nil {
		_ = handler.deleteDocVectors(ctx, doc.ID)
	}
	if handler.semantic != nil && handler.embedder != nil {
		if _, err := handler.embedDocChunks(ctx, doc, chunks); err != nil {
			return domain.DocRecord{}, fmt.Errorf("reindex embed: %w", err)
		}
	}
	doc.ChunkCount = len(chunks)
	doc.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if handler.docDB != nil {
		if err := handler.docDB.InsertDoc(doc); err != nil {
			return domain.DocRecord{}, fmt.Errorf("save reindexed %s: %w", singularScope(scope), err)
		}
	}
	log.Infof("[%s] reindexed %q: %d chunks", scope, doc.Title, len(chunks))
	doc.Content = ""
	return doc, nil
}

func singularScope(scope string) string {
	switch scope {
	case "docs":
		return "document"
	case "knowledge":
		return "knowledge"
	default:
		return scope
	}
}

func (handler *Handler) embedDocChunks(ctx context.Context, doc domain.DocRecord, chunks []indexer.DocChunk) (int, error) {
	n, err := indexer.EmbedChunksCanonical(ctx, handler.embedder, handler.semantic,
		indexer.EmbedDocMeta{
			ID:    doc.ID,
			Title: doc.Title,
			Path:  doc.Filename,
			Scope: doc.Kind,
			Repo:  "docs",
		},
		chunks, handler.cfg.EmbeddingBatch)
	if err != nil {
		return 0, fmt.Errorf("reindex embed: %w", err)
	}
	log.Infof("[docs] embedded %d chunks for %q", n, doc.ID)
	return n, nil
}

func (handler *Handler) deleteDocVectors(ctx context.Context, docID string) error {
	if err := handler.semantic.Delete(ctx, semantic.DeleteQuery{DocumentID: docID}); err != nil {
		return fmt.Errorf("delete doc vectors %q: %w", docID, err)
	}
	return nil
}

func (handler *Handler) deleteDocAndVectors(ctx context.Context, id string) error {
	if handler.semantic != nil {
		if err := handler.deleteDocVectors(ctx, id); err != nil {
			log.Errorf("[docs] semantic delete error for %q: %v", id, err)
		}
	}
	if handler.docDB != nil {
		if _, err := handler.docDB.DeleteDoc(id); err != nil {
			return fmt.Errorf("delete %q: %w", id, err)
		}
	}
	return nil
}
