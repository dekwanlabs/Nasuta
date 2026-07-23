package indexer

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/semantic"
)

// EmbedDocMeta carries document fields that become the runbook payload.
// It is shared by all embedding entry points so payload schema lives in one place.
// That prevents embed paths from silently dropping fields.
type EmbedDocMeta struct {
	ID    string // doc id; written as both "id" and "doc_id"
	Title string // document title (H1) per chunk (chunk title overrides)
	Path  string // workspace-relative filename (payload "path")
	Scope string // payload "scope": mirrors DocStore kind — flow | schema | module | document
	Repo  string // payload "repo": "docs" for KB/user docs
}

// EmbedDocInput is the whole document unit for chunk-and-embed.
// It carries EmbedDocMeta plus the content body.
// Fields are duplicated so composite literals can assign them directly.
type EmbedDocInput struct {
	ID      string
	Title   string
	Path    string
	Scope   string
	Repo    string
	Content string
}

// EmbedDocsCanonical chunks inputs and upserts them with one uniform runbook payload.
// It does not delete prior vectors; callers must clear old state when needed.
// It returns the total embedded chunk count.
func EmbedDocsCanonical(ctx context.Context, emb embed.Embedder, sem semantic.Store, inputs []EmbedDocInput, batchSize int) (int, error) {
	if len(inputs) == 0 {
		return 0, nil
	}
	cfg := DefaultDocChunkConfig()
	total := 0
	for _, in := range inputs {
		chunks := ChunkMarkdown(in.ID, in.Title, in.Content, cfg)
		meta := EmbedDocMeta{ID: in.ID, Title: in.Title, Path: in.Path, Scope: in.Scope, Repo: in.Repo}
		n, err := EmbedChunksCanonical(ctx, emb, sem, meta, chunks, batchSize)
		if err != nil {
			return total, fmt.Errorf("embed doc %q: %w", in.ID, err)
		}
		total += n
	}
	return total, nil
}

// EmbedChunksCanonical embeds a pre-chunked document with the uniform runbook payload.
// Point IDs use DocChunkID(docID, chunkIndex) so all embed paths replace the same points.
// All document chunks use payload kind "runbook" and are separated by scope.
func EmbedChunksCanonical(ctx context.Context, emb embed.Embedder, sem semantic.Store, meta EmbedDocMeta, chunks []DocChunk, batchSize int) (int, error) {
	if len(chunks) == 0 {
		return 0, nil
	}
	if batchSize <= 0 {
		batchSize = 16
	}
	for start := 0; start < len(chunks); start += batchSize {
		end := start + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batch := chunks[start:end]
		texts := make([]string, len(batch))
		for i, c := range batch {
			texts[i] = c.Text
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			return 0, fmt.Errorf("embed batch [%d:%d]: %w", start, end, err)
		}
		points := make([]semantic.Record, 0, len(vecs))
		evidenceClass, trustTier := domain.EvidenceForRunbookScope(meta.Scope)
		for i, v := range vecs {
			c := batch[i]
			points = append(points, semantic.Record{
				ID:          DocChunkID(c.DocID, c.ChunkIndex),
				DenseVector: v,
				Metadata: map[string]any{
					"kind":           "runbook",
					"id":             c.DocID,
					"doc_id":         c.DocID,
					"repo":           meta.Repo,
					"path":           meta.Path,
					"title":          c.Title,
					"scope":          meta.Scope,
					"section_header": c.SectionHeader,
					"chunk_index":    c.ChunkIndex,
					"text":           c.Text,
					"evidence_class": evidenceClass,
					"trust_tier":     trustTier,
				},
			})
		}
		if err := sem.Upsert(ctx, points); err != nil {
			return 0, fmt.Errorf("upsert batch [%d:%d]: %w", start, end, err)
		}
	}
	return len(chunks), nil
}
