package indexing

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
)

type semanticDocument struct {
	id            string
	text          string
	payload       map[string]any
	sparseIndices []uint32
	sparseValues  []float32
}

func (svc *Service) embedBatch(ctx context.Context, label string, docs []semanticDocument) error {
	if len(docs) == 0 {
		return nil
	}
	batchSize := svc.Cfg.EmbeddingBatch
	if batchSize <= 0 {
		batchSize = 16
	}
	concurrency := svc.Cfg.EmbeddingConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	type batchRange struct{ start, end int }
	var batches []batchRange
	for start := 0; start < len(docs); start += batchSize {
		end := start + batchSize
		if end > len(docs) {
			end = len(docs)
		}
		batches = append(batches, batchRange{start, end})
	}

	var (
		wg      sync.WaitGroup
		limit   = make(chan struct{}, concurrency)
		done    int64
		skipped int64
		mu      sync.Mutex
		errs    []string
	)

	for _, batch := range batches {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		limit <- struct{}{}
		go func(batch batchRange) {
			defer wg.Done()
			defer func() { <-limit }()
			defer func() {
				if recovered := recover(); recovered != nil {
					mu.Lock()
					errs = append(errs, fmt.Sprintf("panic batch [%d:%d]: %v", batch.start, batch.end, recovered))
					skipped += int64(batch.end - batch.start)
					mu.Unlock()
				}
			}()

			group := docs[batch.start:batch.end]
			texts := make([]string, len(group))
			for i, doc := range group {
				texts[i] = trimText(doc.text)
			}
			vectors, err := svc.embedWithRetry(ctx, texts)
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("embed batch [%d:%d]: %v", batch.start, batch.end, err))
				skipped += int64(len(group))
				mu.Unlock()
				return
			}
			if len(vectors) != len(group) {
				mu.Lock()
				errs = append(errs, fmt.Sprintf(
					"embed batch [%d:%d]: got %d vectors for %d texts",
					batch.start, batch.end, len(vectors), len(group),
				))
				skipped += int64(len(group))
				mu.Unlock()
				return
			}
			points := make([]semantic.Record, 0, len(group))
			for i, doc := range group {
				points = append(points, semantic.Record{
					ID: doc.id, DenseVector: vectors[i], Metadata: doc.payload,
					SparseVector: &semantic.SparseVector{Indices: doc.sparseIndices, Values: doc.sparseValues},
				})
			}
			if len(points) == 0 {
				return
			}
			if err := svc.Semantic.Upsert(ctx, points); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("upsert batch [%d:%d]: %v", batch.start, batch.end, err))
				skipped += int64(len(points))
				mu.Unlock()
				return
			}
			embedded := atomic.AddInt64(&done, int64(len(points)))
			if embedded%2000 < int64(len(points)) {
				log.Infof("[semantic] %s: %d/%d embedded", label, embedded, len(docs))
			}
		}(batch)
	}
	wg.Wait()

	missing := int64(len(docs)) - done - skipped
	if missing > 0 {
		mu.Lock()
		errs = append(errs, fmt.Sprintf("embedding interrupted before scheduling %d documents", missing))
		skipped += missing
		mu.Unlock()
	}
	if len(errs) > 0 {
		log.Warnf("[semantic] %s: %d/%d embedded, %d skipped (%d errors: %v)",
			label, done, len(docs), skipped, len(errs), errs[0])
		return fmt.Errorf("%s: %d/%d embedded; %d batch errors: %s", label, done, len(docs), len(errs), errs[0])
	}
	log.Infof("[semantic] embedded %d %s (concurrency %d)", done, label, concurrency)
	return nil
}

func (svc *Service) embedWithRetry(ctx context.Context, texts []string) ([][]float32, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		started := time.Now()
		vectors, err := svc.Embedder.Embed(ctx, texts)
		if elapsed := time.Since(started); elapsed > 10*time.Second {
			log.Infof("[semantic] slow embed: %d texts in %v, err=%v", len(texts), elapsed, err)
		}
		if err == nil {
			return vectors, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func trimText(text string) string {
	text = strings.ToValidUTF8(text, "")
	text = strings.TrimSpace(text)
	if len(text) <= 8000 {
		return text
	}
	return strings.TrimSpace(text[:8000])
}
