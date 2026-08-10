package indexing

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

// EmbedCodeChunks performs the workspace-wide v2 migration/rebuild. Repository
// updates use EmbedRepoCode so existing token IDs remain stable.
func (svc *Service) EmbedCodeChunks(ctx context.Context, dirs []string) error {
	svc.indexMu.Lock()
	defer svc.indexMu.Unlock()
	if len(dirs) == 0 {
		return nil
	}
	if err := indexer.ValidateScanInputs(svc.Cfg.WorkspaceRoot, dirs); err != nil {
		return fmt.Errorf("validate code scan inputs: %w", err)
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{
		Collection: svc.Cfg.Semantic.Collection,
		DenseDim:   svc.Embedder.Dim(),
	}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}

	builder := retrieval.NewBM25Builder()
	preserveVocab := false
	if current := svc.bm25.Load(); current != nil && !svc.bm25MigrationRequired.Load() {
		builder = current.Clone()
		preserveVocab = true
	}
	generation := newIndexGeneration("workspace")
	chunks := indexer.ScanCodeChunks(svc.Cfg.WorkspaceRoot, dirs)
	docs, err := svc.buildCodeDocuments(ctx, chunks, builder, generation)
	if err != nil {
		return err
	}
	if preserveVocab {
		if err := builder.SaveVocab(svc.bm25VocabPath()); err != nil {
			return fmt.Errorf("save bm25 vocab: %w", err)
		}
	}
	if err := svc.embedBatch(ctx, "code", docs); err != nil {
		return err
	}
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{
		Filter: semantic.Filter{Keywords: map[string]string{"kind": "code_chunk"}},
		Except: semantic.Filter{Keywords: map[string]string{"index_generation": generation}},
	}); err != nil {
		return fmt.Errorf("delete stale code vectors: %w", err)
	}
	if !preserveVocab {
		if err := builder.SaveVocab(svc.bm25VocabPath()); err != nil {
			return fmt.Errorf("save bm25 vocab: %w", err)
		}
	}
	svc.bm25MigrationRequired.Store(false)
	svc.setBM25(builder)
	log.Infof("[indexing] embedded %d code chunks, bm25 vocab=%d", len(docs), builder.VocabularySize())
	return nil
}

func (svc *Service) buildCodeDocuments(
	ctx context.Context,
	chunks []domain.CodeChunk,
	builder *retrieval.BM25Builder,
	generation string,
) ([]semanticDocument, error) {
	services, err := svc.DB.AllServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("load service modules: %w", err)
	}
	docs := make([]semanticDocument, 0, len(chunks))
	for _, chunk := range chunks {
		text := trimText(chunk.Text)
		var indices []uint32
		var values []float32
		if indexer.IsSparseIndexableFile(chunk.Path) {
			tokens := builder.AddDoc(text)
			indices, values = retrieval.SparseToSorted(builder.BuildSparse(tokens))
		}
		evidenceClass, trustTier := domain.EvidenceForCodeChunk(chunk.Lang, chunk.Repo)
		docs = append(docs, semanticDocument{
			id: platform.UUIDFromString(
				"code:" + chunk.Path + ":" +
					strconv.Itoa(chunk.StartLine) + ":" +
					strconv.Itoa(chunk.EndLine),
			),
			text: text,
			payload: map[string]any{
				"kind":             "code_chunk",
				"repo":             chunk.Repo,
				"path":             chunk.Path,
				"lang":             chunk.Lang,
				"layer":            layerForCodePath(services, chunk.Path),
				"start_line":       chunk.StartLine,
				"end_line":         chunk.EndLine,
				"text":             text,
				"preview":          trimText(chunk.Text),
				"evidence_class":   evidenceClass,
				"trust_tier":       trustTier,
				"index_generation": generation,
			},
			sparseIndices: indices,
			sparseValues:  values,
		})
	}
	return docs, nil
}

func layerForCodePath(services []domain.ServiceRecord, path string) string {
	path = filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(path), "./"))
	bestPrefix := ""
	layer := ""
	for _, service := range services {
		prefix := "repos/" + service.Repo
		if service.ModulePath != "." {
			prefix += "/" + service.ModulePath
		}
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		if len(prefix) > len(bestPrefix) {
			bestPrefix = prefix
			layer = service.Layer
		}
	}
	return layer
}

func newIndexGeneration(scope string) string {
	return platform.UUIDFromString(scope + ":" + strconv.FormatInt(time.Now().UnixNano(), 10))
}

// EmbedRepoCode refreshes code vectors for a single repo.
func (svc *Service) EmbedRepoCode(ctx context.Context, repo string) error {
	if repo == "" {
		return fmt.Errorf("empty repo")
	}
	svc.indexMu.Lock()
	defer svc.indexMu.Unlock()
	if svc.bm25MigrationRequired.Load() {
		return retrieval.ErrLegacyVocabulary
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{
		Collection: svc.Cfg.Semantic.Collection,
		DenseDim:   svc.Embedder.Dim(),
	}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}
	scanDir := filepath.Join("repos", repo)
	log.Infof("[embed-repo] repo=%q", repo)
	chunks := indexer.ScanCodeChunks(svc.Cfg.WorkspaceRoot, []string{scanDir})
	base := svc.bm25.Load()
	builder := retrieval.NewBM25Builder()
	if base != nil {
		builder = base.Clone()
	}
	generation := newIndexGeneration(repo)
	docs, err := svc.buildCodeDocuments(ctx, chunks, builder, generation)
	if err != nil {
		return err
	}
	// Append-only IDs are persisted before the semantic write. A failed upsert can leave
	// unused IDs, but can never leave vectors with coordinates unknown on restart.
	if err := builder.SaveVocab(svc.bm25VocabPath()); err != nil {
		return fmt.Errorf("save bm25 vocab: %w", err)
	}
	if len(docs) == 0 {
		if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{Repository: repo}); err != nil {
			return fmt.Errorf("delete empty repo vectors: %w", err)
		}
		svc.setBM25(builder)
		return nil
	}
	if err := svc.embedBatch(ctx, "code repo="+repo, docs); err != nil {
		return fmt.Errorf("embed code: %w", err)
	}
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{
		Repository: repo,
		Except:     semantic.Filter{Keywords: map[string]string{"index_generation": generation}},
	}); err != nil {
		return fmt.Errorf("delete stale repo vectors: %w", err)
	}
	svc.setBM25(builder)
	log.Infof("[embed-repo] repo=%q done", repo)
	return nil
}

// ReindexAllServices refreshes service-level vectors after metadata-heavy rebuilds.
func (svc *Service) ReindexAllServices(ctx context.Context) error {
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{Repository: serviceRepoBucket}); err != nil {
		return fmt.Errorf("delete service bucket: %w", err)
	}
	services, err := svc.DB.AllServices(ctx)
	if err != nil {
		return fmt.Errorf("load services for re-embed: %w", err)
	}
	if err := svc.embedServices(ctx, services); err != nil {
		return fmt.Errorf("embed services: %w", err)
	}
	log.Infof("[reindex] re-embedded %d service vectors", len(services))
	return nil
}
