package indexing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/indexing/docgen"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
)

// EmbedDocs refreshes generated docs and the docs-backed vectors as one unit.
func (svc *Service) EmbedDocs(ctx context.Context) error {
	if svc.docDB == nil {
		log.Infof("[embed] document store unavailable; skip document embedding")
		return nil
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{
		Collection: svc.Cfg.Semantic.Collection,
		DenseDim:   svc.Embedder.Dim(),
	}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}
	generator, err := docgen.New(svc.Cfg, svc.Platform, svc.docDB)
	if err != nil {
		return fmt.Errorf("create documentation generator: %w", err)
	}
	generator.GenerateDocs(ctx, []string{svc.Cfg.WorkspaceRoot})

	runbooks, _, err := indexer.LoadKnowledgeBase(svc.docDB)
	if err != nil {
		return fmt.Errorf("load runbooks: %w", err)
	}
	if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{Repository: "docs"}); err != nil {
		return fmt.Errorf("delete docs-repo vectors: %w", err)
	}
	if err := svc.embedRunbooks(ctx, runbooks); err != nil {
		return fmt.Errorf("embed runbooks: %w", err)
	}
	if err := svc.EmbedDocuments(ctx); err != nil {
		return fmt.Errorf("embed documents: %w", err)
	}
	log.Infof("[embed] docs re-embedded: runbooks=%d, documents done", len(runbooks))
	return nil
}

// EmbedDocuments re-embeds generated document kinds from the doc store.
func (svc *Service) EmbedDocuments(ctx context.Context) error {
	if svc.docDB == nil {
		log.Infof("[embed] document store unavailable; skip generated document embedding")
		return nil
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{
		Collection: svc.Cfg.Semantic.Collection,
		DenseDim:   svc.Embedder.Dim(),
	}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}

	docs, err := svc.docDB.ListDocsByKinds(domain.GeneratedDocKinds)
	if err != nil {
		return fmt.Errorf("list docs: %w", err)
	}
	for _, doc := range docs {
		if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{DocumentID: doc.ID}); err != nil {
			log.Warnf("[embed] delete doc %s: %v", doc.ID, err)
		}
	}
	inputs := make([]indexer.EmbedDocInput, 0, len(docs))
	for _, doc := range docs {
		inputs = append(inputs, indexer.EmbedDocInput{
			ID:      doc.ID,
			Title:   doc.Title,
			Path:    doc.Filename,
			Scope:   doc.Kind,
			Repo:    "docs",
			Content: stripHashLine(doc.Content),
		})
	}
	count, err := indexer.EmbedDocsCanonical(ctx, svc.Embedder, svc.Semantic, inputs, svc.Cfg.EmbeddingBatch)
	if err != nil {
		return fmt.Errorf("embed documents: %w", err)
	}
	log.Infof("[embed] re-indexing documents: %d docs, %d chunks", len(docs), count)
	return nil
}

func stripHashLine(content string) string {
	const prefix = "<!-- hash:"
	if !strings.HasPrefix(content, prefix) {
		return content
	}
	if newline := strings.Index(content, "\n"); newline >= 0 {
		return content[newline+1:]
	}
	return ""
}

// GenerateDocsForRepo regenerates module docs for one repo.
func (svc *Service) GenerateDocsForRepo(ctx context.Context, repo string) error {
	_, err := svc.generateDocsForRepo(ctx, repo)
	return err
}

func (svc *Service) generateDocsForRepo(ctx context.Context, repo string) (bool, error) {
	if repo == "" {
		return false, fmt.Errorf("empty repo")
	}
	scanDir := filepath.Join("repos", repo)
	dir := filepath.Join(svc.Cfg.WorkspaceRoot, scanDir)
	if _, err := os.Stat(dir); err != nil {
		return false, fmt.Errorf("repo %q not found: %w", repo, err)
	}
	if svc.docDB == nil {
		log.Infof("[gendocs] document store unavailable; skip repo=%q", repo)
		return false, nil
	}

	log.Infof("[gendocs] repo=%q", repo)
	generator, err := docgen.New(svc.Cfg, svc.Platform, svc.docDB)
	if err != nil {
		return false, fmt.Errorf("create documentation generator: %w", err)
	}
	changed := generator.GenerateDocsChanged(ctx, []string{dir})
	log.Infof("[gendocs] repo=%q done", repo)
	return changed, nil
}

// embedDocsForRepo re-embeds generated module docs under one repo.
func (svc *Service) embedDocsForRepo(ctx context.Context, repo string) error {
	if svc.docDB == nil {
		log.Infof("[embed-docs] document store unavailable; skip repo=%q", repo)
		return nil
	}
	if err := svc.Semantic.Ensure(ctx, semantic.Schema{
		Collection: svc.Cfg.Semantic.Collection,
		DenseDim:   svc.Embedder.Dim(),
	}); err != nil {
		return fmt.Errorf("ensure semantic collection: %w", err)
	}
	docs, err := svc.docDB.ListDocsByKinds(domain.GeneratedDocKinds)
	if err != nil {
		return fmt.Errorf("list docs: %w", err)
	}
	prefix := repo + "/"
	inputs := make([]indexer.EmbedDocInput, 0, len(docs))
	for _, doc := range docs {
		if !strings.HasPrefix(doc.Filename, prefix) {
			continue
		}
		if err := svc.Semantic.Delete(ctx, semantic.DeleteQuery{DocumentID: doc.ID}); err != nil {
			log.Warnf("[init-project] delete doc %s: %v", doc.ID, err)
		}
		inputs = append(inputs, indexer.EmbedDocInput{
			ID:      doc.ID,
			Title:   doc.Title,
			Path:    doc.Filename,
			Scope:   doc.Kind,
			Repo:    "docs",
			Content: stripHashLine(doc.Content),
		})
	}
	if len(inputs) == 0 {
		return nil
	}
	if _, err := indexer.EmbedDocsCanonical(
		ctx,
		svc.Embedder,
		svc.Semantic,
		inputs,
		svc.Cfg.EmbeddingBatch,
	); err != nil {
		return fmt.Errorf("embed documents: %w", err)
	}
	log.Infof("[init-project] re-embedded %d docs for repo=%q", len(inputs), repo)
	return nil
}
