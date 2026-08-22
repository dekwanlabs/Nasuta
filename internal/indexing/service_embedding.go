package indexing

import (
	"context"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

const serviceRepoBucket = "_services"

func (svc *Service) embedServices(ctx context.Context, services []domain.ServiceRecord) error {
	docs := make([]semanticDocument, 0, len(services))
	skipped := 0
	for _, service := range services {
		if strings.TrimSpace(service.Summary) == "" {
			skipped++
			continue
		}
		docs = append(docs, serviceDocument(service))
	}
	if err := svc.embedBatch(ctx, "services", docs); err != nil {
		return err
	}
	log.Infof("[embed] service vectors: included=%d skipped_without_summary=%d", len(docs), skipped)
	return nil
}

func serviceDocument(service domain.ServiceRecord) semanticDocument {
	text := service.ServiceName
	if service.Summary != "" {
		text += "\n" + service.Summary
	}
	return semanticDocument{
		id:   platform.UUIDFromString("service:" + service.ServiceName),
		text: text,
		payload: map[string]any{
			"kind": "service", "service_name": service.ServiceName,
			"repo": serviceRepoBucket, "layer": service.Layer, "owner": service.Owner,
			"evidence_class": domain.EvidenceClassServiceMeta,
			"trust_tier":     domain.TrustServiceMeta,
		},
	}
}

func (svc *Service) embedRunbooks(ctx context.Context, runbooks []domain.RunbookRecord) error {
	inputs := make([]indexer.EmbedDocInput, 0, len(runbooks))
	for _, runbook := range runbooks {
		inputs = append(inputs, indexer.EmbedDocInput{
			ID:      runbook.ID,
			Title:   runbook.Title,
			Path:    runbook.Path,
			Scope:   runbook.Scope,
			Repo:    runbook.Repo,
			Content: runbook.Text,
		})
	}
	count, err := indexer.EmbedDocsCanonical(ctx, svc.Embedder, svc.Semantic, inputs, svc.Cfg.EmbeddingBatch)
	if err != nil {
		return fmt.Errorf("embed runbooks: %w", err)
	}
	log.Infof("[embed] runbooks: %d docs, %d chunks", len(runbooks), count)
	return nil
}
